package gateway

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"
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
