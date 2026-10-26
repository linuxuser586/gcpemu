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
	"sync"
	"sync/atomic"

	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/inflight"
	"github.com/linuxuser586/gcpemu/internal/store"
	"github.com/linuxuser586/gcpemu/services/compute"
	"github.com/linuxuser586/gcpemu/services/gke"
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
	// loops runs the managed-certificate and edge route loops, whose
	// passes Reset stops and waits for (#307).
	loops inflight.Loops

	// beforeCertWrite, set by tests, runs before a managed-certificate
	// pass writes a certificate's new status.
	beforeCertWrite func(ctx context.Context, path string)
	// podRoutes, set by tests, replaces GKE as the edge's source of pod
	// routes.
	podRoutes func(ctx context.Context) []gke.PodRoute

	// Options for tests. Tests set them after Start, while the data-plane
	// loops already read them.
	opts atomic.Pointer[Options]

	ptState
}

// Options tune the data plane (tests).
type Options struct {
	// DirectDial makes the proxy dial NEG endpoints directly from the host
	// instead of through the relay container (host-reachable test backends).
	DirectDial bool
	// Edge sends forwarding rules on ports below 1024 to the lb-edge
	// container without trying to bind them, so the edge is used even on
	// hosts that let unprivileged processes bind those ports. It still
	// needs a container runtime.
	Edge bool
}

// New returns the service.
func New(env *emu.Env) emu.Service {
	s := &Service{env: env, local: http.NewServeMux(), certKick: make(chan struct{}, 1)}
	s.opts.Store(&Options{DirectDial: os.Getenv("GCPEMU_LB_DIRECT_DIAL") == "1"})
	s.dp = newDataplane(s)
	return s
}

// SetOptions changes data-plane options (call before creating resources).
func (s *Service) SetOptions(o Options) { s.opts.Store(&o) }

func (s *Service) options() Options { return *s.opts.Load() }

func (s *Service) Name() string { return "lb" }

// Register installs the load-balancing collections on the compute API's
// mux (so selfLinks, operations, filters and paging are uniform with the
// rest of compute), the resource-in-use checks (FR-CORE-026) and the Web
// console's listener and routing reports (admin.go).
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
	cmp.AddSubnetReleaser(edgeIPOwner, func(ctx context.Context, subnetwork string) error {
		// The data plane starts a new Edge relay on each Start.
		if e := s.dp.edge; e != nil {
			return e.leaveSubnetwork(ctx, subnetwork)
		}
		return nil
	})
	r.Handle("GET /_emu/v1/lb/listeners", http.HandlerFunc(s.serveListeners))
	r.Handle("POST /_emu/v1/lb/route", http.HandlerFunc(s.serveRoute))
	return nil
}

// Start brings up listeners for stored forwarding rules and the background
// loops (health checks, managed certificates, NEG refresh).
func (s *Service) Start(ctx context.Context) error {
	bg, cancel := context.WithCancel(context.Background())
	s.stop = cancel
	s.dp.start(bg)
	s.loops.Start(s.managedCertLoop, s.dp.edge.routeLoop)
	s.changed()
	return nil
}

// Stop closes every listener and stops background work.
func (s *Service) Stop(ctx context.Context) error {
	if s.stop != nil {
		s.stop()
		_ = s.loops.Stop(ctx)
	}
	s.dp.close(ctx)
	return nil
}

// Ready: the proxy needs nothing external to start.
func (s *Service) Ready() error { return nil }

// ResourceCounts implements emu.ResourceCounter.
func (s *Service) ResourceCounts() map[string]int {
	nss := map[string]string{}
	for _, k := range allKinds() {
		nss[k.coll] = k.ns()
	}
	return store.Counts(s.env.Store, nss)
}

// Reset stops the managed-certificate and edge route passes in flight,
// waiting for them (bounded by ctx), so none writes a certificate or
// creates an edge container or attachment from the state before the
// Reset after it (#307); they run again from ResetDone. The store is
// wiped by the core.
func (s *Service) Reset(ctx context.Context) error {
	select {
	case <-s.certKick:
	default:
	}
	if err := s.loops.Reset(ctx); err != nil {
		return fmt.Errorf("lb: waiting for background work: %w", err)
	}
	return nil
}

var _ emu.ResetFinisher = (*Service)(nil)

// ResetDone rebuilds the data plane from the reset store, dropping the
// listeners and backend runtime state from before the Reset (#450), and
// runs the managed-certificate and edge route loops again.
func (s *Service) ResetDone() {
	s.changed()
	s.loops.Resume()
}

// Snapshot implements emu.Snapshotter: the load balancer's resources are
// in the store.
func (s *Service) Snapshot(ctx context.Context, dir string) error { return nil }

// Restore implements emu.Snapshotter: it rebuilds the data plane from the
// restored resources.
func (s *Service) Restore(ctx context.Context, dir string) error {
	s.changed()
	return nil
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
