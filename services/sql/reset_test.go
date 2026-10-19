package sql

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/runtime"
)

// noRuntime is a host without a container runtime.
type noRuntime struct{ emu.Containers }

func (noRuntime) Runtime(context.Context) (*runtime.Manager, error) {
	return nil, errors.New("no container runtime")
}

// startBlockedOp runs an asynchronous operation on instance p/i whose work
// blocks until its context ends, then is slow to stop (like removing a
// container) and marks the instance FAILED, as a failed create does. It
// returns the operation's name, a channel closed once the work runs and a
// flag set when the work returns.
func startBlockedOp(t *testing.T, s *Service) (string, <-chan struct{}, *atomic.Bool) {
	t.Helper()
	started := make(chan struct{})
	var returned atomic.Bool
	op := s.runOp(s.newOp(context.Background(), "p", "i", "CREATE"), true, func(ctx context.Context) error {
		close(started)
		<-ctx.Done()
		time.Sleep(100 * time.Millisecond)
		s.markFailed(ctx, "p", "i")
		returned.Store(true)
		return ctx.Err()
	})
	return op.Name, started, &returned
}

// Reset cancels an operation still running and waits for it: nothing the
// operation does after its context ends happens after Reset returns, and
// it records neither a failed operation nor a FAILED instance (#306).
func TestResetWaitsForOperations(t *testing.T) {
	s := testService(t, "permissive")
	s.env.Containers = noRuntime{}
	putInstance(t, s, "p", "i", false)
	name, started, returned := startBlockedOp(t, s)
	<-started
	// Reset reports that it could not remove the instance's containers (no
	// runtime); the operation has still stopped.
	_ = s.Reset(context.Background())
	if !returned.Load() {
		t.Fatal("operation still running after Reset returned")
	}
	if op, ok := s.loadOp("p", name); ok && (op.Status == "DONE" || op.Error != nil) {
		t.Fatalf("operation after Reset = %s, %+v", op.Status, op.Error)
	}
	if rec, err := s.loadRecord("p", "i"); err != nil || rec.Instance.State == "FAILED" {
		t.Fatalf("instance after Reset = %+v, %v", rec, err)
	}
}

// Reset waits for other background work (goBackground).
func TestResetWaitsForBackground(t *testing.T) {
	s := testService(t, "permissive")
	started := make(chan struct{})
	var returned atomic.Bool
	s.goBackground(func(ctx context.Context) {
		close(started)
		<-ctx.Done()
		time.Sleep(100 * time.Millisecond)
		returned.Store(true)
	})
	<-started
	if err := s.Reset(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !returned.Load() {
		t.Fatal("background work still running after Reset returned")
	}
}

// Reset gives up waiting when its context ends.
func TestResetBoundedByContext(t *testing.T) {
	s := testService(t, "permissive")
	release := make(chan struct{})
	defer close(release)
	started := make(chan struct{})
	s.goBackground(func(context.Context) {
		close(started)
		<-release
	})
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := s.Reset(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Reset = %v, want deadline exceeded", err)
	}
}

// Operations after a Reset run on a live context: synchronous and
// asynchronous ones complete.
func TestOperationsRunAfterReset(t *testing.T) {
	s := testService(t, "permissive")
	if err := s.Reset(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, async := range []bool{false, true} {
		var ctxErr error
		done := make(chan struct{})
		op := s.runOp(s.newOp(context.Background(), "p", "i", "UPDATE"), async, func(ctx context.Context) error {
			ctxErr = ctx.Err()
			close(done)
			return nil
		})
		<-done
		if ctxErr != nil {
			t.Fatalf("async=%v: operation context after Reset: %v", async, ctxErr)
		}
		deadline := time.Now().Add(5 * time.Second)
		for {
			got, _ := s.loadOp("p", op.Name)
			if got.Status == "DONE" {
				if got.Error != nil {
					t.Fatalf("async=%v: operation failed: %+v", async, got.Error.Errors[0])
				}
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("async=%v: operation not DONE", async)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
}

// Work stopped by Stop still records its failure, as before (#306 keeps
// Stop's meaning).
func TestStopRecordsInterruptedOperation(t *testing.T) {
	s := testService(t, "permissive")
	putInstance(t, s, "p", "i", false)
	name, started, returned := startBlockedOp(t, s)
	<-started
	if err := s.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !returned.Load() {
		t.Fatal("operation still running after Stop returned")
	}
	if op, _ := s.loadOp("p", name); op.Status != "DONE" || op.Error == nil {
		t.Fatalf("operation after Stop = %s, %+v", op.Status, op.Error)
	}
	if rec, _ := s.loadRecord("p", "i"); rec.Instance.State != "FAILED" {
		t.Fatalf("instance after Stop = %s", rec.Instance.State)
	}
}
