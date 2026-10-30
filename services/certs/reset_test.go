package certs

import (
	"context"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linuxuser586/gcpemu/internal/ca"
	"github.com/linuxuser586/gcpemu/internal/clock"
	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// holdLB is a load balancer whose ForwardingRuleIPs, once armed, signals
// entered and blocks until release is closed.
type holdLB struct {
	fakeLB
	armed   atomic.Bool
	entered chan struct{}
	release chan struct{}
}

func (f *holdLB) ForwardingRuleIPs(ctx context.Context, project string) []string {
	if f.armed.CompareAndSwap(true, false) {
		close(f.entered)
		<-f.release
	}
	return f.ips
}

// newResetService returns a started service whose managed certificates
// for app.example.test are authorized through lb.
func newResetService(t *testing.T, lb *holdLB) *Service {
	t.Helper()
	cfg := config.Defaults()
	cfg.Ports[config.AllPorts] = 0
	authority, err := ca.Load(t.TempDir(), "test")
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	env := &emu.Env{Config: &cfg, Store: store.NewMemory(), Clock: clock.Real{}, IDs: emu.NewIDs(true), Log: log,
		Auth: emu.NewPolicyAuthorizer(config.IAMOff, log), Endpoints: emu.NewEndpoints(), CA: authority}
	s := New(env).(*Service)
	lb.fakePeer, lb.ips = fakePeer{"lb"}, []string{"203.0.113.10"}
	dns := &fakeDNS{fakePeer{"dns"}, map[string]string{"app.example.test.": "203.0.113.10"}}
	env.SetServices(map[string]emu.Service{"certs": s, "dns": dns, "lb": lb})
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Stop(context.Background()) })
	return s
}

// A provisioning pass in flight across a Reset writes nothing afterwards:
// the certificate it read before the Reset does not replace one of the
// same name created after it (#307).
func TestResetWaitsForReconcile(t *testing.T) {
	lb := &holdLB{entered: make(chan struct{}), release: make(chan struct{})}
	lb.armed.Store(true)
	s := newResetService(t, lb)
	ctx := context.Background()
	n := resName{Project: "p", Location: "global", ID: "app"}
	managed := map[string]any{"domains": []any{"app.example.test"}}
	if err := s.upsert(ctx, kCert, n, obj{"managed": managed}); err != nil {
		t.Fatal(err)
	}
	s.kick()
	<-lb.entered

	recreated := make(chan error, 1)
	go func() {
		if err := s.Reset(ctx); err != nil {
			recreated <- err
			return
		}
		_ = s.env.Store.Reset()
		s.ResetDone()
		recreated <- s.upsert(ctx, kCert, n, obj{"managed": managed, "description": "after"})
	}()
	select {
	case err := <-recreated:
		close(lb.release)
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(200 * time.Millisecond):
		close(lb.release) // Reset is waiting for the pass
		if err := <-recreated; err != nil {
			t.Fatal(err)
		}
	}
	// Let a pass from before the Reset, if one survived, finish.
	time.Sleep(200 * time.Millisecond)
	o, _, err := s.loadKind(kCert, n.name(kCert))
	if err != nil {
		t.Fatal(err)
	}
	if o["description"] != "after" {
		t.Fatalf("the certificate created after Reset was replaced by one read before it: %v", o)
	}
}

// After a Reset managed certificates are provisioned again (#307).
func TestReconcileRunsAfterReset(t *testing.T) {
	s := newResetService(t, &holdLB{})
	ctx := context.Background()
	if err := s.Reset(ctx); err != nil {
		t.Fatal(err)
	}
	s.ResetDone()
	n := resName{Project: "p", Location: "global", ID: "app"}
	if err := s.upsert(ctx, kCert, n, obj{"managed": map[string]any{"domains": []any{"app.example.test"}}}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		o, _, _ := s.loadKind(kCert, n.name(kCert))
		if certActive(o) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("a managed certificate created after Reset was not provisioned")
		}
		time.Sleep(50 * time.Millisecond)
	}
}
