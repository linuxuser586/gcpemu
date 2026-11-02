package lb

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/linuxuser586/gcpemu/internal/edgeagent"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/runtime"
	"github.com/linuxuser586/gcpemu/services/gke"
)

// fakeRouter is a PublicRouter that, like gke and compute, stores the
// request before applying it, and fails to apply it while fail is set.
type fakeRouter struct {
	name string
	mu   sync.Mutex
	via  string
	ips  []string
	fail bool
	// calls counts the requests.
	calls int
}

func (r *fakeRouter) Name() string                { return r.name }
func (r *fakeRouter) Register(emu.Router) error   { return nil }
func (r *fakeRouter) Start(context.Context) error { return nil }
func (r *fakeRouter) Stop(context.Context) error  { return nil }
func (r *fakeRouter) Ready() error                { return nil }

func (r *fakeRouter) setFail(fail bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.fail = fail
}

func (r *fakeRouter) requests() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

func (r *fakeRouter) routes() (string, []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.via, r.ips
}

func (r *fakeRouter) RoutePublic(_ context.Context, from string, pr emu.PublicRoutes) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if from != emu.PublicRoutesFromLB {
		return errors.New("routes from " + from)
	}
	r.via, r.ips = pr.Via, append([]string(nil), pr.Dests...)
	r.calls++
	if r.fail {
		return errors.New("context deadline exceeded")
	}
	return nil
}

// newRoutedEdge returns an edge whose lb service finds fake gke and
// compute PublicRouters.
func newRoutedEdge(t *testing.T) (*edge, *fakeRouter, *fakeRouter) {
	t.Helper()
	gk, ce := &fakeRouter{name: "gke"}, &fakeRouter{name: "compute"}
	env := &emu.Env{Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	env.SetServices(map[string]emu.Service{"gke": gk, "compute": ce})
	s := &Service{env: env}
	s.opts.Store(&Options{})
	s.dp = &dataplane{s: s}
	s.dp.cfg.Store(&config{})
	s.dp.edge = newEdge(s.dp)
	return s.dp.edge, gk, ce
}

// wantRoutes fails t unless every router routes ips via via.
func wantRoutes(t *testing.T, via string, ips []string, routers ...*fakeRouter) {
	t.Helper()
	for _, r := range routers {
		if gotVia, gotIPs := r.routes(); gotVia != via || !slices.Equal(gotIPs, ips) {
			t.Errorf("%s routes %v via %q, want %v via %q", r.name, gotIPs, gotVia, ips, via)
		}
	}
}

// TestPublicRoutesRetriedAfterError: a route update one PublicRouter
// failed is sent again on the next pass, even when the routes asked for
// then are those the routers last accepted. Both routers store a request
// before applying it, so a removal compute failed left it with no routes
// while the edge still believed them in place, and the forwarding rule's
// address went to the internet (#912).
func TestPublicRoutesRetriedAfterError(t *testing.T) {
	ctx := context.Background()
	e, gk, ce := newRoutedEdge(t)
	pub := []edgeagent.Listener{{IP: "34.120.0.1", Port: 80}, {IP: "34.120.0.1", Port: 443}}
	e.routePublicLocked(ctx, "172.18.0.9", pub)
	wantRoutes(t, "172.18.0.9", []string{"34.120.0.1"}, gk, ce)

	ce.setFail(true)
	e.routePublicLocked(ctx, "172.18.0.9", nil)
	ce.setFail(false)
	e.routePublicLocked(ctx, "172.18.0.9", pub)
	wantRoutes(t, "172.18.0.9", []string{"34.120.0.1"}, gk, ce)
}

// TestPublicRoutesResentAfterEdgeRecreated: once the edge container is
// gone (found not running, or retired while idle), the next pass sends
// the routes again, even via the same external address: what the routers
// hold is no longer known to match it (#912).
func TestPublicRoutesResentAfterEdgeRecreated(t *testing.T) {
	ctx := context.Background()
	e, gk, ce := newRoutedEdge(t)
	pub := []edgeagent.Listener{{IP: "34.120.0.1", Port: 80}}
	e.routePublicLocked(ctx, "172.18.0.9", pub)
	e.routePublicLocked(ctx, "172.18.0.9", pub)
	if gk.requests() != 1 || ce.requests() != 1 {
		t.Fatalf("unchanged routes sent again: gke %d, compute %d requests, want 1", gk.requests(), ce.requests())
	}

	e.forgetContainerLocked()
	e.routePublicLocked(ctx, "172.18.0.9", pub)
	if gk.requests() != 2 || ce.requests() != 2 {
		t.Fatalf("after the edge was recreated: gke %d, compute %d requests, want 2", gk.requests(), ce.requests())
	}
	wantRoutes(t, "172.18.0.9", []string{"34.120.0.1"}, gk, ce)
}

// TestPublicRoutesUnreachableWhileNotServed: while an external forwarding
// rule exists and the Edge relay does not serve its address (here no GKE
// cluster reports pod routes, so it is not running, and no listener
// serves the rule yet), the routers hold unreachable routes for it rather
// than none, so its traffic never leaves for the internet; deleting the
// last such rule removes them (ADR 0055).
func TestPublicRoutesUnreachableWhileNotServed(t *testing.T) {
	ctx := context.Background()
	e, gk, ce := newRoutedEdge(t)
	e.d.s.podRoutes = func(context.Context) []gke.PodRoute { return nil }
	fe := &frontend{key: "34.120.0.1:443", ip: "34.120.0.1", port: 443}
	e.d.cfg.Store(&config{
		fronts: map[string]*frontend{fe.key: fe},
		public: map[string]bool{fe.ip: true},
	})
	e.syncRoutes(ctx)
	wantRoutes(t, "", []string{"34.120.0.1"}, gk, ce)

	e.d.cfg.Store(&config{})
	e.syncRoutes(ctx)
	wantRoutes(t, "", nil, gk, ce)
	if gk.requests() != 2 || ce.requests() != 2 {
		t.Fatalf("gke %d, compute %d requests, want 2: the routes and their removal", gk.requests(), ce.requests())
	}
}

// failingContainers is a container runtime that cannot be reached; start
// runs as the edge asks for it.
type failingContainers struct {
	emu.Containers
	start func()
}

func (c failingContainers) Runtime(context.Context) (*runtime.Manager, error) {
	c.start()
	return nil, errors.New("runtime socket not ready")
}

// TestPublicRoutesUnreachableWhileEdgeRelayStarts: before the edge starts
// the Edge relay, which can take as long as an image pull, the routers
// already hold unreachable routes for the forwarding rule addresses, and
// they keep them when the start fails; while the lb is stopping, a failed
// start sends nothing (ADR 0055).
func TestPublicRoutesUnreachableWhileEdgeRelayStarts(t *testing.T) {
	ctx := context.Background()
	e, gk, ce := newRoutedEdge(t)
	var atStart []string
	e.env().Containers = failingContainers{start: func() {
		via, ips := ce.routes()
		atStart = append(atStart, via+" "+strings.Join(ips, " "))
	}}
	e.d.s.podRoutes = func(context.Context) []gke.PodRoute {
		return []gke.PodRoute{{Cluster: "c", Node: "n", CIDR: "10.4.0.0/24", Via: "10.0.0.3", Subnetwork: "s"}}
	}
	fe := &frontend{key: "34.120.0.1:443", ip: "34.120.0.1", port: 443}
	e.d.cfg.Store(&config{
		fronts: map[string]*frontend{fe.key: fe},
		public: map[string]bool{fe.ip: true},
	})
	e.syncRoutes(ctx)
	if len(atStart) != 1 || atStart[0] != " 34.120.0.1" {
		t.Fatalf("compute's routes as the Edge relay started: %q, want unreachable 34.120.0.1", atStart)
	}
	wantRoutes(t, "", []string{"34.120.0.1"}, gk, ce)

	e.stopped = true
	fe2 := &frontend{key: "34.120.0.2:443", ip: "34.120.0.2", port: 443}
	e.d.cfg.Store(&config{
		fronts: map[string]*frontend{fe.key: fe, fe2.key: fe2},
		public: map[string]bool{fe.ip: true, fe2.ip: true},
	})
	e.syncRoutes(ctx)
	wantRoutes(t, "", []string{"34.120.0.1"}, gk, ce)
}
