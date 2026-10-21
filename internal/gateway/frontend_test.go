package gateway

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/emu"
)

// TestServeFrontendAbandoned: a hand-off the API gateway never accepts
// (not serving yet) returns when the Google front end's context ends, so
// closing the front end cannot hang on it.
func TestServeFrontendAbandoned(t *testing.T) {
	g := New(nil, nil)
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- g.ServeFrontend(ctx, a) }()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("ServeFrontend = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ServeFrontend still blocked after its context ended")
	}
}

// lockedBuffer is a goroutine-safe log sink.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// TestShutdownFrontendQuiet: Shutdown stops the Google front end's side of
// the API gateway without logging an error, and Serve returns only once it
// has stopped, so nothing logs after Serve returns.
func TestShutdownFrontendQuiet(t *testing.T) {
	for range 20 {
		var out lockedBuffer
		g := New(&emu.Env{Config: &config.Config{}, Log: slog.New(slog.NewTextHandler(&out, nil))}, nil)
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		served := make(chan error, 1)
		go func() { served <- g.Serve(l) }()
		for {
			g.mu.RLock()
			srv := g.srv
			g.mu.RUnlock()
			if srv != nil {
				break
			}
			time.Sleep(time.Millisecond)
		}
		if err := g.Shutdown(context.Background()); err != nil {
			t.Fatal(err)
		}
		if err := <-served; err != nil {
			t.Fatalf("Serve = %v", err)
		}
		if s := out.String(); strings.Contains(s, "level=ERROR") {
			t.Fatalf("Shutdown logged an error:\n%s", s)
		}
	}
}
