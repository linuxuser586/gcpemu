package compute

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	computev1 "google.golang.org/api/compute/v1"

	"github.com/linuxuser586/gcpemu/internal/emu"
)

// fakeGKE is GKE's PublicRouter: it records compute's last request, and
// fails to apply it while fail is set.
type fakeGKE struct {
	mu    sync.Mutex
	from  string
	r     emu.PublicRoutes
	calls int
	fail  bool
}

func (f *fakeGKE) Name() string                { return "gke" }
func (f *fakeGKE) Register(emu.Router) error   { return nil }
func (f *fakeGKE) Start(context.Context) error { return nil }
func (f *fakeGKE) Stop(context.Context) error  { return nil }
func (f *fakeGKE) Ready() error                { return nil }

func (f *fakeGKE) RoutePublic(_ context.Context, from string, r emu.PublicRoutes) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.from, f.r = from, r
	f.calls++
	if f.fail {
		return errors.New("exec failed")
	}
	return nil
}

func (f *fakeGKE) setFail(fail bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fail = fail
}

func (f *fakeGKE) state() (string, []string, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.from, f.r.Dests, f.calls
}

// TestUnreachableRoutesFollowReservations: compute's Public routes are
// the External address pools from the start, plus a /32 for each reserved
// external address outside them while it is reserved, and reach compute's
// and GKE's routers as reservations change (ADR 0059).
func TestUnreachableRoutesFollowReservations(t *testing.T) {
	ctx := context.Background()
	s, _ := natTestService(t, nil)
	gk := &fakeGKE{}
	s.env.SetServices(map[string]emu.Service{"gke": gk})
	pools := []string{"34.16.0.0/12", "34.120.0.0/14"}
	want := func(label string, dests []string, calls int) {
		t.Helper()
		from, got, n := gk.state()
		if from != emu.PublicRoutesFromCompute || !slices.Equal(got, dests) || n != calls {
			t.Errorf("%s: gke got %v from %q (%d requests), want %v (%d)", label, got, from, n, dests, calls)
		}
		if got := s.pub.Dests(); len(dests) > 0 && !slices.Equal(got, slashed(dests)) {
			t.Errorf("%s: compute holds %v, want %v", label, got, dests)
		}
	}
	s.routeUnreachable(ctx)
	want("start", pools, 1)

	reserve := func(name, ip string) string {
		t.Helper()
		path := "projects/p/regions/us-central1/addresses/" + name
		a := &computev1.Address{Name: name, Address: ip}
		if err := s.createAddress(ctx, "p", "regions/us-central1", path, false, a); err != nil {
			t.Fatal(err)
		}
		return path
	}
	// An address in the pools is covered already: nothing to send.
	inPool := reserve("auto", "")
	want("pool address", pools, 1)
	outside := reserve("seeded", "198.51.100.7")
	want("outside address", append(slices.Clone(pools), "198.51.100.7"), 2)

	// A router that failed is sent the routes again next time.
	gk.setFail(true)
	if err := s.removeAddress(ctx, outside, false, &computev1.Address{}); err != nil {
		t.Fatal(err)
	}
	gk.setFail(false)
	if err := s.removeAddress(ctx, inPool, false, &computev1.Address{}); err != nil {
		t.Fatal(err)
	}
	want("deleted", pools, 4)
}

// slashed returns dests as the CIDRs a PublicRouteTable lists.
func slashed(dests []string) []string {
	var out []string
	for _, d := range dests {
		if !strings.Contains(d, "/") {
			d += "/32"
		}
		out = append(out, d)
	}
	return out
}

// TestNatRulesUnreachablePools: the proxy holds the pools unreachable
// alongside the load balancer's routes, neither sender's update removes
// the other's, and the Offline sink leaves the pools to their routes, so
// a client fails at once rather than being recorded as internet egress
// (ADR 0059).
func TestNatRulesUnreachablePools(t *testing.T) {
	ctx := context.Background()
	s, _ := natTestService(t, nil)
	s.SetNATSink(true)
	g := &egressProxy{s: s, network: tNet, extIP: "172.22.0.2", extGW: "172.22.0.1"}
	rules := func() string {
		r, err := s.natRules(g, s.netsOf(tNet))
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	s.routeUnreachable(ctx)
	if err := s.RoutePublic(ctx, emu.PublicRoutesFromLB, emu.PublicRoutes{Via: "172.22.0.9", Dests: []string{"34.120.0.5"}}); err != nil {
		t.Fatal(err)
	}
	poolRoutes := []string{
		"ip route replace unreachable 34.16.0.0/12 proto 245\n",
		"ip route replace unreachable 34.120.0.0/14 proto 245\n",
	}
	poolExempt := []string{
		"-A GCPEMU-PRE -s 10.0.0.0/24 -d 34.16.0.0/12 -j RETURN\n",
		"-A GCPEMU-PRE -s 10.0.0.0/24 -d 34.120.0.0/14 -j RETURN\n",
	}
	has := func(r string, lines ...string) bool {
		redirect := strings.Index(r, "-A GCPEMU-PRE -s 10.0.0.0/24 -p tcp -j REDIRECT")
		for _, l := range lines {
			i := strings.Index(r, l)
			if i < 0 || (strings.HasPrefix(l, "-A GCPEMU-PRE") && redirect < i) {
				return false
			}
		}
		return true
	}
	lbRoute := "ip route replace 34.120.0.5/32 via 172.22.0.9 proto 245\n"
	if r := rules(); !has(r, append(append(poolRoutes, poolExempt...), lbRoute)...) {
		t.Fatalf("pools and lb routes:\n%s", r)
	}
	// The load balancer removing its routes leaves the pools'.
	if err := s.RoutePublic(ctx, emu.PublicRoutesFromLB, emu.PublicRoutes{}); err != nil {
		t.Fatal(err)
	}
	if r := rules(); !has(r, append(poolRoutes, poolExempt...)...) || strings.Contains(r, "34.120.0.5") {
		t.Fatalf("after lb removal:\n%s", r)
	}
	// Compute's update leaves the load balancer's routes.
	if err := s.RoutePublic(ctx, emu.PublicRoutesFromLB, emu.PublicRoutes{Via: "172.22.0.9", Dests: []string{"34.120.0.5"}}); err != nil {
		t.Fatal(err)
	}
	s.unreachSent = ""
	s.routeUnreachable(ctx)
	if r := rules(); !has(r, append(poolRoutes, lbRoute)...) {
		t.Fatalf("after compute update:\n%s", r)
	}
}
