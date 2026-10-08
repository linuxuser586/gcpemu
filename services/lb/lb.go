// Package lb emulates the Application Load Balancer (SRS 5.8): the compute
// v1 load-balancing collections (forwarding rules, target HTTP(S) proxies,
// URL maps, backend services and buckets, health checks, SSL certificates
// and SSL policies, global and regional; FR-LB-001) served on the compute
// API, and a real in-process L7 proxy per forwarding rule wired exactly as
// GCP wires them: forwarding rule → target proxy → URL map → backend
// service or bucket → NEG endpoints (FR-LB-002..010). Cloud Armor security
// policies, target TCP, SSL and gRPC proxies and legacy HTTP(S) health
// checks are recorded: served by the API with no data-plane effect.
//
// The routing engine (FR-LB-003) is the pure package lb/urlmap. Cloud CDN
// (services/cdn) wraps backends with enableCdn; Certificate Manager and
// Network Security resources come from the "certs" service (emu.CertManager).
package lb

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"

	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/services/compute"
)

// Service is the lb service module.
type Service struct {
	env *emu.Env
	cmp *compute.Service

	// local mirrors the routes registered on compute's mux, for seeding.
	local *http.ServeMux

	dp *dataplane

	certMu   sync.Mutex
	certKick chan struct{}
	stop     context.CancelFunc
	bgDone   chan struct{}

	// Options for tests.
	opts Options

	ptState
}

// Options tune the data plane (tests).
type Options struct {
	// DirectDial makes the proxy dial NEG endpoints directly from the host
	// instead of through the relay container (host-reachable test backends).
	DirectDial bool
	// Edge forces the lb-edge container for forwarding rules on privileged
	// ports even when binding fails for another reason.
	Edge bool
}

// New returns the service.
func New(env *emu.Env) emu.Service {
	s := &Service{env: env, local: http.NewServeMux(), certKick: make(chan struct{}, 1)}
	s.dp = newDataplane(s)
	if os.Getenv("GCPEMU_LB_DIRECT_DIAL") == "1" {
		s.opts.DirectDial = true
	}
	return s
}

// SetOptions changes data-plane options (call before creating resources).
func (s *Service) SetOptions(o Options) { s.opts = o }

func (s *Service) Name() string { return "lb" }

// Register installs the load-balancing collections on the compute API's
// mux (so selfLinks, operations, filters and paging are uniform with the
// rest of compute) and the resource-in-use checks (FR-CORE-026).
func (s *Service) Register(r emu.Router) error {
	svc, ok := s.env.Lookup("compute")
	if !ok {
		return fmt.Errorf("lb: the compute service is required")
	}
	cmp, ok := svc.(*compute.Service)
	if !ok {
		return fmt.Errorf("lb: unexpected compute service %T", svc)
	}
	s.cmp = cmp
	s.routes(func(pattern string, h http.HandlerFunc) {
		cmp.HandleFunc(pattern, h)
		s.local.HandleFunc(pattern, h)
	})
	cmp.AddUsageChecker(s.usedBy)
	return nil
}

// Start brings up listeners for stored forwarding rules and the background
// loops (health checks, managed certificates, NEG refresh).
func (s *Service) Start(ctx context.Context) error {
	bg, cancel := context.WithCancel(context.Background())
	s.stop = cancel
	s.bgDone = make(chan struct{})
	s.dp.start(bg)
	go func() {
		defer close(s.bgDone)
		s.managedCertLoop(bg)
	}()
	s.changed()
	return nil
}

// Stop closes every listener and stops background work.
func (s *Service) Stop(ctx context.Context) error {
	if s.stop != nil {
		s.stop()
		<-s.bgDone
	}
	s.dp.close(ctx)
	return nil
}

// Ready: the proxy needs nothing external to start.
func (s *Service) Ready() error { return nil }

// Reset drops the data plane's runtime state; the store is wiped by the core.
func (s *Service) Reset(ctx context.Context) error {
	s.changed()
	return nil
}

// EnvVars implements emu.EnvVarer: one GCPEMU_LB_<NAME>=host:port per
// forwarding rule listener (FR-LB-002).
func (s *Service) EnvVars(gateway string, endpoints map[string]string) map[string]string {
	out := map[string]string{}
	var names []string
	for k := range endpoints {
		if strings.HasPrefix(k, "lb:") {
			names = append(names, k)
		}
	}
	sort.Strings(names)
	for _, k := range names {
		out["GCPEMU_LB_"+envName(strings.TrimPrefix(k, "lb:"))] = endpoints[k]
	}
	return out
}

// envName upper-cases a forwarding rule name for an env var.
func envName(s string) string {
	var b strings.Builder
	for _, c := range s {
		switch {
		case c >= 'a' && c <= 'z':
			b.WriteRune(c - 32)
		case c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
			b.WriteRune(c)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// changed re-reads the configuration and reconciles the data plane. It is
// called after every committed mutation.
func (s *Service) changed() {
	s.dp.reconcile()
	select {
	case s.certKick <- struct{}{}:
	default:
	}
}
