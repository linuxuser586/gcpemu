package inflight

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Reset halts the running generation with ErrReset and waits for it; the
// loops start again only on Resume, and not at all after Stop.
func TestLoopsResetAndResume(t *testing.T) {
	var l Loops
	starts := make(chan context.Context, 4)
	l.Start(func(ctx context.Context) { starts <- ctx; <-ctx.Done() })
	first := <-starts
	if err := l.Reset(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !Halted(first) {
		t.Fatalf("the generation before Reset ended with %v, want ErrReset", context.Cause(first))
	}
	select {
	case <-starts:
		t.Fatal("the loops started again before Resume")
	case <-time.After(50 * time.Millisecond):
	}
	l.Resume()
	second := <-starts
	if second.Err() != nil {
		t.Fatal("the loops did not start again on Resume")
	}
	l.Resume() // already running: no second generation
	if err := l.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if Halted(second) {
		t.Fatal("Stop reported as a Reset")
	}
	if err := l.Reset(context.Background()); err != nil {
		t.Fatal(err)
	}
	l.Resume()
	select {
	case <-starts:
		t.Fatal("the loops started after Stop")
	case <-time.After(50 * time.Millisecond):
	}
}

// A Reset whose ctx ends before the old generation returns reports it;
// Resume starts the loops again all the same.
func TestLoopsResetBounded(t *testing.T) {
	var l Loops
	release := make(chan struct{})
	starts := make(chan struct{}, 4)
	l.Start(func(ctx context.Context) {
		starts <- struct{}{}
		<-release
	})
	<-starts
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := l.Reset(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Reset = %v, want the ctx's deadline", err)
	}
	l.Resume()
	<-starts
	close(release)
	if err := l.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
}
