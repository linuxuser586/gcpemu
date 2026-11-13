package dns

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"
)

// ended returns a ctx whose deadline has passed, as Teardown's is when a
// shutdown runs out of time.
func ended() context.Context {
	ctx, cancel := context.WithDeadline(context.Background(), time.Unix(0, 0))
	cancel()
	return ctx
}

// startLoopback starts a DNS Service on a dynamic loopback port, its
// TCP listener wrapped by wrap if that is set.
func startLoopback(t *testing.T, wrap func(net.Listener) net.Listener) *Service {
	t.Helper()
	s := newTestService(t, true)
	s.wrapTCP = wrap
	s.env.Config.Bind = "127.0.0.1"
	s.env.Config.Ports = map[string]int{"dns": 0}
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	return s
}

// Stop with Teardown's ctx already ended is not a failure once both
// servers have closed their sockets (#1115).
func TestStopEndedCtx(t *testing.T) {
	s := startLoopback(t, nil)
	addr := s.Addr()
	if err := s.Stop(ended()); err != nil {
		t.Fatalf("Stop = %v, want nil", err)
	}
	l, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("tcp port still bound: %v", err)
	}
	l.Close()
	pc, err := net.ListenPacket("udp", addr)
	if err != nil {
		t.Fatalf("udp port still bound: %v", err)
	}
	pc.Close()
}

// failClose is a listener whose Close fails.
type failClose struct{ net.Listener }

var errClose = errors.New("close failed")

func (l failClose) Close() error {
	l.Listener.Close()
	return errClose
}

// A failure to close the TCP listener is still a failure, ctx ended or
// not (#1115).
func TestStopTCPCloseFails(t *testing.T) {
	for name, ctx := range map[string]context.Context{"live": context.Background(), "ended": ended()} {
		t.Run(name, func(t *testing.T) {
			s := startLoopback(t, func(l net.Listener) net.Listener { return failClose{l} })
			if err := s.Stop(ctx); !errors.Is(err, errClose) {
				t.Fatalf("Stop = %v, want the listener's close error", err)
			}
		})
	}
}

// Stop on a DNS Service that never started has nothing to stop.
func TestStopNeverStarted(t *testing.T) {
	for name, ctx := range map[string]context.Context{"live": context.Background(), "ended": ended()} {
		t.Run(name, func(t *testing.T) {
			if err := newTestService(t, true).Stop(ctx); err != nil {
				t.Fatalf("Stop = %v, want nil", err)
			}
		})
	}
}
