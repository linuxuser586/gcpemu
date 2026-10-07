// Package compute emulates the parts of compute.googleapis.com v1 the other
// services build on (SRS 1.2 supporting resources, 3.3, 5.10): regions and
// zones, projects.get, VPC networks, subnetworks, firewalls, routes,
// addresses, Cloud Routers with Cloud NAT (FR-NAT-001), zonal network
// endpoint groups (FR-GKE-008) and compute Operations (FR-CORE-023), plus
// the minimal servicenetworking.googleapis.com API that binds private
// services access ranges to a VPC.
//
// It also implements emu.VPC: subnetworks are realised as container
// networks so workload addresses are real, and a per-VPC egress gateway
// container forwards traffic to the internet only while a Cloud NAT covers
// the source subnetwork (FR-NAT-002..005).
package compute

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/lro"
)

// Service is the compute service module. It implements emu.VPC.
type Service struct {
	env *emu.Env
	mux *http.ServeMux
	sn  *http.ServeMux // servicenetworking
	lro *lro.Manager

	opMu   sync.Mutex
	opWake chan struct{}
	timers map[string]*time.Timer
	closed bool

	// vpcMu serialises realisation of container networks and IP allocation.
	vpcMu sync.Mutex
	// egress holds the NAT egress gateways by VPC network path.
	egMu   sync.Mutex
	egress map[string]*gateway

	chkMu       sync.RWMutex
	checkers    []UsageChecker
	igResolvers []InstanceGroupResolver

	sink *sinkServer
}

var _ emu.VPC = (*Service)(nil)

// New returns the service.
func New(env *emu.Env) emu.Service {
	s := &Service{
		env:    env,
		mux:    http.NewServeMux(),
		sn:     http.NewServeMux(),
		opWake: make(chan struct{}),
		timers: map[string]*time.Timer{},
		egress: map[string]*gateway{},
	}
	s.lro = lro.NewManager(env, "compute")
	s.sink = &sinkServer{token: randomToken()}
	s.routes()
	s.snRoutes()
	return s
}

func (s *Service) Name() string { return "compute" }

// Register mounts compute v1 at /compute/v1/ (the Go client and gcloud base
// URL path), under the /compute/ gateway prefix and for host
// compute.googleapis.com; servicenetworking under /servicenetworking/.
func (s *Service) Register(r emu.Router) error {
	r.Handle("/compute/v1/", s.mux)
	r.Mount("compute", []string{"compute.googleapis.com"}, s.mux)
	r.Mount("servicenetworking", []string{"servicenetworking.googleapis.com"}, s.sn)
	s.lro.Register(r)
	return nil
}

// Start fails operations interrupted by a previous run. Container networks
// and the egress gateway are (re)created lazily when workloads ask for them.
func (s *Service) Start(ctx context.Context) error {
	s.recoverOps()
	return nil
}

// Stop cancels pending operations and stops the NAT sink.
func (s *Service) Stop(ctx context.Context) error {
	s.opMu.Lock()
	s.closed = true
	for k, t := range s.timers {
		t.Stop()
		delete(s.timers, k)
	}
	s.opMu.Unlock()
	s.lro.Close()
	s.egMu.Lock()
	gws := s.egress
	s.egress = map[string]*gateway{}
	s.egMu.Unlock()
	for _, g := range gws {
		g.stop()
	}
	return nil
}

// Ready: the control plane needs nothing external.
func (s *Service) Ready() error { return nil }

// Reset removes the NAT egress gateways (their state is wiped with the
// store); workload containers and networks belong to their owners and the core.
func (s *Service) Reset(ctx context.Context) error {
	s.egMu.Lock()
	var nets []string
	for np := range s.egress {
		nets = append(nets, np)
	}
	s.egMu.Unlock()
	for _, np := range nets {
		s.removeGateway(ctx, np)
	}
	return nil
}

// EnvVars implements emu.EnvVarer. gcloud's compute client has BASE_URL
// https://compute.googleapis.com/compute/v1/ and uses an override with a
// non-root path verbatim. gcloud defines no api_endpoint_overrides
// property for servicenetworking (`gcloud services vpc-peerings` always
// calls servicenetworking.googleapis.com), so none is exported for it;
// OpenTofu (service_networking_custom_endpoint, see `gcpemu tofu-provider`)
// and the Go client (/servicenetworking/ base path) can be pointed at it.
func (s *Service) EnvVars(gateway string, endpoints map[string]string) map[string]string {
	return map[string]string{
		"CLOUDSDK_API_ENDPOINT_OVERRIDES_COMPUTE": "http://" + gateway + "/compute/v1/",
	}
}

// check authorizes the caller for permission on a compute resource path.
func (s *Service) check(ctx context.Context, permission, path string) error {
	return s.env.Auth.Check(ctx, permission, "//compute.googleapis.com/"+path)
}

// UsageChecker reports a resource that uses the compute resource at path
// ("projects/P/global/networks/N", ...), or "" when none does. Other
// services (the load balancer in M3) register checkers so deletes honour
// their references (FR-CORE-026).
type UsageChecker func(ctx context.Context, path string) string

// AddUsageChecker registers a resource-in-use checker.
func (s *Service) AddUsageChecker(c UsageChecker) {
	s.chkMu.Lock()
	s.checkers = append(s.checkers, c)
	s.chkMu.Unlock()
}

// HandleFunc adds a route to the compute v1 mux (patterns are rooted at
// "/compute/v1/..."), so other modules (the M3 load balancer) can serve
// further compute collections with the same mount and host routing.
func (s *Service) HandleFunc(pattern string, h http.HandlerFunc) { s.mux.HandleFunc(pattern, h) }

// externalUser asks the registered checkers about path.
func (s *Service) externalUser(ctx context.Context, path string) string {
	s.chkMu.RLock()
	defer s.chkMu.RUnlock()
	for _, c := range s.checkers {
		if u := c(ctx, path); u != "" {
			return u
		}
	}
	return ""
}

// notFoundHandler answers unknown compute paths like Google's front end.
func notFoundHandler(w http.ResponseWriter, r *http.Request) {
	apierr.Write(w, apierr.NotFound("The requested URL %s was not found on this server.", r.URL.Path))
}

var errNoRuntime = errors.New("compute: no container runtime")

// randomToken returns a secret for the NAT event channel.
func randomToken() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
