// Package compute emulates the parts of compute.googleapis.com v1 the other
// services build on (SRS 1.2 supporting resources, 3.3, 5.10): regions and
// zones, projects.get, VPC networks, subnetworks, firewalls, routes,
// addresses, Cloud Routers with Cloud NAT (FR-NAT-001), network endpoint
// groups (zonal for FR-GKE-008, regional and global), VPC Network Peering
// (recorded) and compute Operations (FR-CORE-023), plus the minimal
// servicenetworking.googleapis.com API that binds private services access
// ranges to a VPC.
//
// It also implements emu.VPC: subnetworks are realised as container
// networks so workload addresses are real, and a per-VPC NAT egress proxy
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
	"sync/atomic"
	"time"

	"github.com/linuxuser586/gcpemu/internal/apidef"
	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/inflight"
	"github.com/linuxuser586/gcpemu/internal/lro"
	"github.com/linuxuser586/gcpemu/internal/store"
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
	// lastOp is closed, per Service ID, once that Service's most recently
	// accepted operation's work has run, so that its operations complete
	// in the order they were accepted.
	lastOp  map[string]chan struct{}
	stopped chan struct{} // closed by Stop
	// halt is closed by Reset (and replaced), releasing operations that
	// wait behind one it dropped.
	halt chan struct{}
	// opWork counts operations whose work has started; Reset starts its
	// next generation under opMu.
	opWork inflight.Tracker

	// vpcMu serialises realisation of container networks and IP allocation.
	vpcMu sync.Mutex
	// aliasDone is set once a probe found whether Managed containers can
	// alias Declared addresses (ADR 0004); aliasErr is why they cannot.
	aliasMu   sync.Mutex
	aliasDone bool
	aliasErr  error
	// hostMu serialises changes to the Host attachment (hostattach.go);
	// restoring is the restore of its Offered ports at start.
	hostMu    sync.Mutex
	restoring sync.WaitGroup
	// egress holds the NAT egress proxies by VPC network path.
	egMu   sync.Mutex
	egress map[string]*egressProxy
	// pubVia and pubIPs are the load balancer's public routes
	// (RoutePublic), programmed into every NAT egress proxy.
	pubMu  sync.Mutex
	pubVia string
	pubIPs []string

	// chkMu guards the extensions other services register: checkers,
	// igResolvers and releasers.
	chkMu       sync.RWMutex
	checkers    []UsageChecker
	igResolvers []InstanceGroupResolver
	releasers   map[string]SubnetReleaser

	sink *sinkServer
	// sinkOn: NAT-allowed egress goes to the offline sink (FR-NAT-005).
	sinkOn atomic.Bool
}

var (
	_ emu.VPC          = (*Service)(nil)
	_ emu.PublicRouter = (*Service)(nil)
)

// New returns the service.
func New(env *emu.Env) emu.Service {
	s := &Service{
		env:     env,
		mux:     http.NewServeMux(),
		sn:      http.NewServeMux(),
		opWake:  make(chan struct{}),
		timers:  map[string]*time.Timer{},
		lastOp:  map[string]chan struct{}{},
		stopped: make(chan struct{}),
		halt:    make(chan struct{}),
		egress:  map[string]*egressProxy{},
	}
	s.lro = lro.NewManager(env, "compute")
	s.sink = &sinkServer{token: randomToken()}
	s.sinkOn.Store(env.Config != nil && env.Config.Offline)
	s.routes()
	s.snRoutes()
	return s
}

func (s *Service) Name() string { return "compute" }

// Register mounts compute v1 at /compute/v1/ (the Go client and gcloud base
// URL path), under the /compute/ gateway prefix and for host
// compute.googleapis.com; servicenetworking under /servicenetworking/.
// Requests are first checked against the pinned discovery document's
// required parameters and fields (NFR-MNT-001).
func (s *Service) Register(r emu.Router) error {
	api := apidef.REST("compute", "v1").Handler(s.mux)
	r.Handle("/compute/v1/", api)
	r.Mount("compute", []string{"compute.googleapis.com"}, api)
	r.Mount("servicenetworking", []string{"servicenetworking.googleapis.com"}, s.sn)
	s.lro.Register(r)
	s.hostRoutes(r)
	r.Handle("GET /_emu/v1/vpc/containerNetworks", http.HandlerFunc(s.serveContainerNetworks))
	return nil
}

// Start fails operations interrupted by a previous run. Container networks
// and the NAT egress proxy are (re)created lazily when workloads ask for
// them, except those of subnetworks with Offered ports, which are served
// again in the background.
func (s *Service) Start(ctx context.Context) error {
	s.recoverOps()
	rctx, cancel := context.WithCancel(context.Background())
	s.restoring.Add(1)
	go func() {
		defer s.restoring.Done()
		defer cancel()
		go func() {
			select {
			case <-s.stopped:
				cancel()
			case <-rctx.Done():
			}
		}()
		s.restoreOffers(rctx)
	}()
	return nil
}

// Stop cancels pending operations and stops the NAT sink.
func (s *Service) Stop(ctx context.Context) error {
	s.opMu.Lock()
	if !s.closed {
		close(s.stopped)
	}
	s.closed = true
	for k, t := range s.timers {
		t.Stop()
		delete(s.timers, k)
	}
	s.opMu.Unlock()
	s.restoring.Wait()
	s.lro.Close()
	s.egMu.Lock()
	gws := s.egress
	s.egress = map[string]*egressProxy{}
	s.egMu.Unlock()
	for _, g := range gws {
		g.stop()
	}
	return nil
}

// Ready: the control plane needs nothing external.
func (s *Service) Ready() error { return nil }

// ResourceCounts implements emu.ResourceCounter.
func (s *Service) ResourceCounts() map[string]int {
	return store.Counts(s.env.Store, map[string]string{
		"networks": nsNetworks, "subnetworks": nsSubnets, "firewalls": nsFirewalls, "routes": nsRoutes,
		"routers": nsRouters, "addresses": nsAddresses, "globalAddresses": nsGlobalAddresses,
		"networkEndpointGroups": nsNEGs,
	})
}

// Reset drops pending operations and waits, bounded by ctx, for those
// already running (#301). It then removes the NAT egress proxies (their
// state is wiped with the store); workload containers and networks belong
// to their owners and the core.
func (s *Service) Reset(ctx context.Context) error {
	err := errors.Join(s.resetOps(ctx), s.lro.Reset(ctx))
	s.hostMu.Lock()
	s.withdrawAll(ctx, s.hostAttachments())
	s.hostMu.Unlock()
	s.egMu.Lock()
	var nets []string
	for np := range s.egress {
		nets = append(nets, np)
	}
	s.egMu.Unlock()
	for _, np := range nets {
		s.removeProxy(ctx, np)
	}
	return err
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

// SubnetReleaser detaches its service's containers from subnetwork's
// container network and frees the addresses they hold there.
type SubnetReleaser func(ctx context.Context, subnetwork string) error

// AddSubnetReleaser registers the releaser of owner's addresses. Other
// services register one for infrastructure that joins a subnetwork only
// while workloads on it need it (the load balancer's Edge relay), so that
// such an address alone does not keep the subnetwork from being deleted
// (#683): compute asks the releaser to leave instead.
func (s *Service) AddSubnetReleaser(owner string, r SubnetReleaser) {
	s.chkMu.Lock()
	if s.releasers == nil {
		s.releasers = map[string]SubnetReleaser{}
	}
	s.releasers[owner] = r
	s.chkMu.Unlock()
}

// releaser returns the releaser of owner's addresses, or nil.
func (s *Service) releaser(owner string) SubnetReleaser {
	s.chkMu.RLock()
	defer s.chkMu.RUnlock()
	return s.releasers[owner]
}

// releaseSubnet asks the releasers of the addresses on subnetwork path's
// container network to leave it; the subnetwork stays in use by an owner
// that cannot.
func (s *Service) releaseSubnet(ctx context.Context, path string) error {
	var owners []string
	_ = s.env.Store.View(func(tx store.Tx) error {
		if st, ok := get[vpcNet](tx, nsVPCNets, path); ok {
			for _, o := range ownersOf(tx, st.Name) {
				owners = append(owners, o.owner)
			}
		}
		return nil
	})
	for _, o := range owners {
		r := s.releaser(o)
		if r == nil {
			continue
		}
		if err := r(ctx, path); err != nil {
			s.env.Log.Warn("compute: releasing a subnetwork address", "subnetwork", path, "owner", o, "err", err)
			return errInUse("subnetwork", path, o)
		}
	}
	return nil
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
