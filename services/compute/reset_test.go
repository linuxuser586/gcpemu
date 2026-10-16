package compute

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	computev1 "google.golang.org/api/compute/v1"
	"google.golang.org/grpc/codes"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/clock"
	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// opService returns a compute service whose operations run after latency.
func opService(t *testing.T, latency string) *Service {
	t.Helper()
	cfg := config.Defaults()
	cfg.LROLatency = map[string]string{"compute": latency}
	s := New(&emu.Env{
		Config: &cfg, Store: store.NewMemory(), Clock: clock.Real{}, IDs: emu.NewIDs(true),
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}).(*Service)
	t.Cleanup(func() { _ = s.Stop(context.Background()) })
	return s
}

var resetSpec = opSpec{project: "p", scope: "global", opType: "insert", target: "projects/p/global/networks/n"}

// opCount returns how many operations the store holds.
func opCount(s *Service) int {
	var n int
	_ = s.env.Store.View(func(tx store.Tx) error {
		n = len(list[computev1.Operation](tx, nsOps, ""))
		return nil
	})
	return n
}

// awaitDone waits for the operation op to be DONE.
func awaitDone(t *testing.T, s *Service, op *computev1.Operation) {
	t.Helper()
	path := relPath(op.SelfLink)
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		var done bool
		_ = s.env.Store.View(func(tx store.Tx) error {
			cur, ok := get[computev1.Operation](tx, nsOps, path)
			done = ok && cur.Status == "DONE"
			return nil
		})
		if done {
			return
		}
	}
	t.Fatalf("operation %s not done", op.Name)
}

func TestResetDropsPendingOperations(t *testing.T) {
	s := opService(t, "20ms")
	ran := make(chan struct{}, 2)
	work := func(context.Context) error { ran <- struct{}{}; return nil }
	for range 2 { // the second queued behind the first (lastOp)
		if _, err := s.startOp(context.Background(), resetSpec, work); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Reset(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.env.Store.Reset(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if len(ran) != 0 || opCount(s) != 0 {
		t.Fatalf("after reset: %d ran, %d operations", len(ran), opCount(s))
	}
	op, err := s.startOp(context.Background(), resetSpec, func(context.Context) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	awaitDone(t, s, op)
}

func TestResetWaitsForRunningOperation(t *testing.T) {
	s := opService(t, "1ms")
	started, release := make(chan struct{}), make(chan struct{})
	if _, err := s.startOp(context.Background(), resetSpec, func(context.Context) error {
		close(started)
		<-release
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	queued := make(chan struct{}, 1)
	if _, err := s.startOp(context.Background(), resetSpec, func(context.Context) error {
		queued <- struct{}{}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	<-started
	time.Sleep(10 * time.Millisecond) // the queued operation's timer has fired
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := s.Reset(ctx); err == nil {
		t.Fatal("Reset returned nil while an operation was still running")
	}
	reset := make(chan error, 1)
	go func() { reset <- s.Reset(context.Background()) }()
	select {
	case err := <-reset:
		t.Fatalf("Reset returned %v before the running operation finished", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := <-reset; err != nil {
		t.Fatal(err)
	}
	if err := s.env.Store.Reset(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	if len(queued) != 0 || opCount(s) != 0 {
		t.Fatalf("after reset: queued ran %d, %d operations", len(queued), opCount(s))
	}
	op, err := s.startOp(context.Background(), resetSpec, func(context.Context) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	awaitDone(t, s, op)
}

// hookStore runs after once, after the next Update commits.
type hookStore struct {
	store.Store
	after func()
}

func (h *hookStore) Update(fn func(store.Tx) error) error {
	err := h.Store.Update(fn)
	if f := h.after; f != nil {
		h.after = nil
		f()
	}
	return err
}

func TestResetDuringStartOpAbortsOperation(t *testing.T) {
	s := opService(t, "20ms")
	hs := &hookStore{Store: s.env.Store}
	s.env.Store = hs
	// A whole Instance Reset lands between the pending record's write and
	// the operation's timer.
	hs.after = func() {
		if err := s.Reset(context.Background()); err != nil {
			t.Error(err)
		}
		if err := hs.Store.Reset(); err != nil {
			t.Error(err)
		}
	}
	ran := make(chan struct{}, 1)
	op, err := s.startOp(context.Background(), resetSpec, func(context.Context) error { ran <- struct{}{}; return nil })
	if err == nil || apierr.From(err).Code != codes.Aborted {
		t.Fatalf("startOp across a Reset = %v %v, want Aborted", op, err)
	}
	time.Sleep(100 * time.Millisecond)
	if len(ran) != 0 || opCount(s) != 0 {
		t.Fatalf("after reset: %d ran, %d operations", len(ran), opCount(s))
	}
}
