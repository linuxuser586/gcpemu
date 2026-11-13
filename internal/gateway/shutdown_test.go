package gateway

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/emu"
)

// served is an API gateway serving on a loopback listener.
type served struct {
	g    *Gateway
	addr string
	out  *lockedBuffer
	done chan error

	mu     sync.Mutex
	states map[net.Conn]http.ConnState
	change chan struct{} // closed and replaced on each state change
}

// startGateway starts an API gateway whose Admin API runs h, and waits
// until it serves.
func startGateway(t *testing.T, h http.Handler) *served {
	t.Helper()
	return startGatewayOn(t, h, func(l net.Listener) net.Listener { return l })
}

// startGatewayOn is startGateway serving on wrap of a loopback listener.
func startGatewayOn(t *testing.T, h http.Handler, wrap func(net.Listener) net.Listener) *served {
	t.Helper()
	out := &lockedBuffer{}
	g := New(&emu.Env{Config: &config.Config{}, Log: slog.New(slog.NewTextHandler(out, nil))}, nil)
	if h != nil {
		g.HandleAdmin(h)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &served{g: g, addr: l.Addr().String(), out: out, done: make(chan error, 1),
		states: map[net.Conn]http.ConnState{}, change: make(chan struct{})}
	g.connState = func(c net.Conn, st http.ConnState) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.states[c] = st
		close(s.change)
		s.change = make(chan struct{})
	}
	go func() { s.done <- g.Serve(wrap(l)) }()
	for {
		g.mu.RLock()
		srv := g.srv
		g.mu.RUnlock()
		if srv != nil {
			break
		}
		time.Sleep(time.Millisecond)
	}
	return s
}

// dial opens a TCP connection to the API gateway.
func (s *served) dial(t *testing.T) net.Conn {
	t.Helper()
	c, err := net.Dial("tcp", s.addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// ended is a context that has already ended, as Teardown's can be.
func ended() context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), -time.Second)
	cancel()
	return ctx
}

// assertClosed fails unless the server has closed c: reading it ends
// without waiting for the deadline.
func assertClosed(t *testing.T, c net.Conn) {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, err := io.Copy(io.Discard, c)
	if errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatal("connection still open after Shutdown returned")
	}
}

// shutdownLine is the Info line a force-close logs.
const shutdownLine = "API gateway closed connections still open at shutdown"

// TestShutdownEndedCtxActiveRequest: with Teardown's ctx already ended,
// a request still in progress is closed by force and Shutdown reports
// success (#1034).
func TestShutdownEndedCtxActiveRequest(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	s := startGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
	}))
	c := s.dial(t)
	if _, err := io.WriteString(c, "GET /_emu/block HTTP/1.1\r\nHost: localhost\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	<-entered
	if err := s.g.Shutdown(ended()); err != nil {
		t.Fatalf("Shutdown = %v, want nil", err)
	}
	assertClosed(t, c)
	if n := strings.Count(s.out.String(), shutdownLine); n != 1 {
		t.Errorf("force-close logged %d lines, want 1:\n%s", n, s.out.String())
	}
	if !strings.Contains(s.out.String(), "level=INFO msg=\""+shutdownLine) {
		t.Errorf("force-close line not at Info:\n%s", s.out.String())
	}
}

// TestShutdownEndedCtxFreshConn: a connection that has not sent its first
// request yet counts as active to the HTTP server; with ctx already ended
// it is closed by force and Shutdown reports success (#1034).
func TestShutdownEndedCtxFreshConn(t *testing.T) {
	s := startGateway(t, nil)
	c := s.dial(t)
	waitNew(t, s, 1)
	if err := s.g.Shutdown(ended()); err != nil {
		t.Fatalf("Shutdown = %v, want nil", err)
	}
	assertClosed(t, c)
	if n := strings.Count(s.out.String(), shutdownLine); n != 1 {
		t.Errorf("force-close logged %d lines, want 1:\n%s", n, s.out.String())
	}
}

// TestShutdownEndedCtxIdle: with ctx already ended, Shutdown reports
// success both with only an idle keep-alive connection and with none, and
// logs no force-close (#1034).
func TestShutdownEndedCtxIdle(t *testing.T) {
	t.Run("keep-alive", func(t *testing.T) {
		s := startGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		c := s.dial(t)
		if _, err := io.WriteString(c, "GET /_emu/x HTTP/1.1\r\nHost: localhost\r\n\r\n"); err != nil {
			t.Fatal(err)
		}
		resp, err := http.ReadResponse(bufio.NewReader(c), nil)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		waitIdle(t, s)
		if err := s.g.Shutdown(ended()); err != nil {
			t.Fatalf("Shutdown = %v, want nil", err)
		}
		assertClosed(t, c)
		if strings.Contains(s.out.String(), shutdownLine) {
			t.Errorf("idle connection logged a force-close:\n%s", s.out.String())
		}
	})
	t.Run("none", func(t *testing.T) {
		s := startGateway(t, nil)
		if err := s.g.Shutdown(ended()); err != nil {
			t.Fatalf("Shutdown = %v, want nil", err)
		}
		if strings.Contains(s.out.String(), shutdownLine) {
			t.Errorf("no connections logged a force-close:\n%s", s.out.String())
		}
	})
}

// TestShutdownDrainsInTime: a request that ends within ctx drains, and no
// force-close is logged.
func TestShutdownDrainsInTime(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	s := startGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
	}))
	c := s.dial(t)
	if _, err := io.WriteString(c, "GET /_emu/block HTTP/1.1\r\nHost: localhost\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	go func() {
		time.Sleep(50 * time.Millisecond)
		close(release)
	}()
	if err := s.g.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown = %v, want nil", err)
	}
	if resp, err := http.ReadResponse(bufio.NewReader(c), nil); err != nil {
		t.Fatalf("drained request got no response: %v", err)
	} else {
		resp.Body.Close()
	}
	if strings.Contains(s.out.String(), shutdownLine) {
		t.Errorf("drain in time logged a force-close:\n%s", s.out.String())
	}
}

// waitNew waits until n of the HTTP server's connections are new.
func waitNew(t *testing.T, s *served, n int) {
	t.Helper()
	waitState(t, s, func(st http.ConnState) bool { return st == http.StateNew }, n)
}

// waitIdle waits until the HTTP server has seen a connection go idle.
func waitIdle(t *testing.T, s *served) {
	t.Helper()
	waitState(t, s, func(st http.ConnState) bool { return st == http.StateIdle }, 1)
}

// waitState waits until n of the HTTP server's connections are in a state
// match accepts.
func waitState(t *testing.T, s *served, match func(http.ConnState) bool, n int) {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		s.mu.Lock()
		got := 0
		for _, st := range s.states {
			if match(st) {
				got++
			}
		}
		change := s.change
		s.mu.Unlock()
		if got >= n {
			return
		}
		select {
		case <-change:
		case <-timeout:
			t.Fatalf("%d connections in the wanted state, want %d", got, n)
		}
	}
}

// failClose is a listener whose Close fails.
type failClose struct{ net.Listener }

var errClose = errors.New("close failed")

func (l failClose) Close() error {
	l.Listener.Close()
	return errClose
}

// TestShutdownEndedCtxListenerCloseFails: a force-close is not an error,
// but failing to close a listener still is, ctx ended or not (#1034).
func TestShutdownEndedCtxListenerCloseFails(t *testing.T) {
	s := startGatewayOn(t, nil, func(l net.Listener) net.Listener { return failClose{l} })
	c := s.dial(t)
	waitNew(t, s, 1)
	if err := s.g.Shutdown(ended()); !errors.Is(err, errClose) {
		t.Fatalf("Shutdown = %v, want the listener's close error", err)
	}
	assertClosed(t, c)
}
