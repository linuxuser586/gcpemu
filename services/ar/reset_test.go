package ar

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linuxuser586/gcpemu/internal/clock"
	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// startedService builds a service on a memory store, without listeners,
// in the state Start leaves it.
func startedService(t *testing.T) *Service {
	t.Helper()
	cfg := config.Defaults()
	env := &emu.Env{
		Config: &cfg, Store: store.NewMemory(), Clock: clock.Real{}, IDs: emu.NewIDs(false),
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Endpoints: emu.NewEndpoints(),
	}
	s := New(env).(*Service)
	s.ready = true
	return s
}

// Reset cancels an upstream manifest fetch still running and waits for it,
// so it writes no manifest or tag afterwards (#306); a fetch after the
// Reset runs on a live context.
func TestResetWaitsForManifestFetch(t *testing.T) {
	s := startedService(t)
	started := make(chan struct{})
	var returned atomic.Bool
	go func() {
		_, _ = s.upstreamFetch(func(ctx context.Context) (any, error) {
			close(started)
			<-ctx.Done()
			time.Sleep(100 * time.Millisecond) // the response is being stored
			returned.Store(true)
			return nil, ctx.Err()
		})
	}()
	<-started
	if err := s.Reset(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !returned.Load() {
		t.Fatal("manifest fetch still running after Reset returned")
	}
	if _, err := s.upstreamFetch(func(ctx context.Context) (any, error) { return nil, ctx.Err() }); err != nil {
		t.Fatalf("fetch after Reset: %v", err)
	}
}

// Reset gives up waiting for upstream fetches when its context ends.
func TestResetBoundedByContext(t *testing.T) {
	s := startedService(t)
	release := make(chan struct{})
	defer close(release)
	started := make(chan struct{})
	go func() {
		_, _ = s.upstreamFetch(func(context.Context) (any, error) {
			close(started)
			<-release
			return nil, nil
		})
	}()
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := s.Reset(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Reset = %v, want deadline exceeded", err)
	}
}
