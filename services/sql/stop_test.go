package sql

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/linuxuser586/gcpemu/internal/netplane"
)

// endedCtx returns a context whose deadline has already passed.
func endedCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	t.Cleanup(cancel)
	return ctx
}

// blockForever tracks a background goroutine that ignores cancellation
// until the test ends.
func blockForever(t *testing.T, s *Service) {
	t.Helper()
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	started := make(chan struct{})
	s.goBackground(func(context.Context) {
		close(started)
		<-release
	})
	<-started
}

// plantListeners exposes a loopback listener and a LAN router on s, as a
// running public instance would have, and returns their addresses.
func plantListeners(t *testing.T, s *Service) []string {
	t.Helper()
	plane := netplane.New(nil, nil, "127.0.0.1", s.env.Log)
	loop, err := plane.Expose("loopback", "127.0.0.1:0", "127.0.0.1:1", false)
	if err != nil {
		t.Fatal(err)
	}
	lan, err := plane.Expose("lan", "127.0.0.1:0", "127.0.0.1:1", false)
	if err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	router := serveConnect(l, func(string) (string, bool) { return "", false }, (&net.Dialer{}).DialContext)
	s.loopMu.Lock()
	s.exposed["p/i"] = append(s.exposed["p/i"], loop)
	s.loopMu.Unlock()
	s.lanMu.Lock()
	s.lan["p/i"] = &instanceLAN{exposed: []*netplane.Exposed{lan}}
	s.routers["127.0.0.1"] = router
	s.lanMu.Unlock()
	return []string{loop.Addr(), lan.Addr(), l.Addr().String()}
}

// assertClosed fails unless every address refuses connections.
func assertClosed(t *testing.T, addrs []string) {
	t.Helper()
	for _, a := range addrs {
		if c, err := net.DialTimeout("tcp", a, time.Second); err == nil {
			c.Close()
			t.Errorf("%s still accepts connections after Stop", a)
		}
	}
}

// Stop with an already-ended ctx on a Service that was never started
// returns nil: there is nothing to wait for (#1116). The race it guards
// against lost only some of the time, hence the repeats.
func TestStopEndedCtxNeverStarted(t *testing.T) {
	for i := range 50 {
		s := testService(t, "permissive")
		if err := s.Stop(endedCtx(t)); err != nil {
			t.Fatalf("call %d: Stop = %v, want nil", i, err)
		}
	}
}

// Stop with an already-ended ctx after Start returns nil: the backup loop
// returns as soon as it is cancelled, within the grace period (#1116).
func TestStopEndedCtxAfterStart(t *testing.T) {
	for i := range 50 {
		s := testService(t, "permissive")
		if err := s.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		addrs := plantListeners(t, s)
		if err := s.Stop(endedCtx(t)); err != nil {
			t.Fatalf("call %d: Stop = %v, want nil", i, err)
		}
		assertClosed(t, addrs)
	}
}

// Work that does not return within the grace period after ctx ends is a
// failure to stop: Stop reports ctx's error after about a second, and
// still closes the listeners (#1116).
func TestStopGraceExpires(t *testing.T) {
	s := testService(t, "permissive")
	addrs := plantListeners(t, s)
	blockForever(t, s)
	start := time.Now()
	err := s.Stop(endedCtx(t))
	took := time.Since(start)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Stop = %v, want deadline exceeded", err)
	}
	if took < stopGrace-100*time.Millisecond || took > 2*stopGrace-100*time.Millisecond {
		t.Errorf("Stop took %v, want about %v", took, stopGrace)
	}
	assertClosed(t, addrs)
}

// Reset with an already-ended ctx on a started Service returns nil (#1116).
func TestResetEndedCtxAfterStart(t *testing.T) {
	s := testService(t, "permissive")
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Stop(context.Background()) })
	for i := range 20 {
		if err := s.Reset(endedCtx(t)); err != nil {
			t.Fatalf("call %d: Reset = %v, want nil", i, err)
		}
	}
}

// Reset reports ctx's error when old work outlives the grace period.
func TestResetGraceExpires(t *testing.T) {
	s := testService(t, "permissive")
	blockForever(t, s)
	if err := s.Reset(endedCtx(t)); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Reset = %v, want deadline exceeded", err)
	}
}
