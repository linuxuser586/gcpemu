package httpserver

import (
	"bufio"
	"bytes"
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
)

// lockedBuffer is a bytes.Buffer safe for concurrent log writes.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

// served is a Server serving on a loopback listener.
type served struct {
	srv  *Server
	addr string
	out  *lockedBuffer
	done chan error

	mu     sync.Mutex
	states map[net.Conn]http.ConnState
	change chan struct{} // closed and replaced on each state change
}

// start starts a Server named "Test server" running h, serving on wrap of
// a loopback listener.
func start(t *testing.T, h http.Handler, wrap func(net.Listener) net.Listener) *served {
	t.Helper()
	if h == nil {
		h = http.NotFoundHandler()
	}
	if wrap == nil {
		wrap = func(l net.Listener) net.Listener { return l }
	}
	out := &lockedBuffer{}
	srv := New("Test server", h, slog.New(slog.NewTextHandler(out, nil)))
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &served{srv: srv, addr: l.Addr().String(), out: out, done: make(chan error, 1),
		states: map[net.Conn]http.ConnState{}, change: make(chan struct{})}
	srv.ConnState = func(c net.Conn, st http.ConnState) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.states[c] = st
		close(s.change)
		s.change = make(chan struct{})
	}
	go func() { s.done <- srv.Serve(wrap(l)) }()
	return s
}

// dial opens a TCP connection to the Server.
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

// shutdownLine is the Info line a force-close logs, naming the Server.
const shutdownLine = "Test server closed connections still open at shutdown"

// TestShutdownEndedCtxActiveRequest: with ctx already ended, a request
// still in progress is closed by force, one Info line names the Server,
// and Shutdown reports success.
func TestShutdownEndedCtxActiveRequest(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	s := start(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
	}), nil)
	c := s.dial(t)
	if _, err := io.WriteString(c, "GET /block HTTP/1.1\r\nHost: localhost\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	<-entered
	if err := s.srv.Shutdown(ended()); err != nil {
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
// it is closed by force and Shutdown reports success.
func TestShutdownEndedCtxFreshConn(t *testing.T) {
	s := start(t, nil, nil)
	c := s.dial(t)
	waitState(t, s, http.StateNew)
	if err := s.srv.Shutdown(ended()); err != nil {
		t.Fatalf("Shutdown = %v, want nil", err)
	}
	assertClosed(t, c)
	if n := strings.Count(s.out.String(), shutdownLine); n != 1 {
		t.Errorf("force-close logged %d lines, want 1:\n%s", n, s.out.String())
	}
}

// TestShutdownEndedCtxIdle: with ctx already ended, Shutdown reports
// success both with only an idle keep-alive connection and with none, and
// logs no force-close.
func TestShutdownEndedCtxIdle(t *testing.T) {
	t.Run("keep-alive", func(t *testing.T) {
		s := start(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}), nil)
		c := s.dial(t)
		if _, err := io.WriteString(c, "GET /x HTTP/1.1\r\nHost: localhost\r\n\r\n"); err != nil {
			t.Fatal(err)
		}
		resp, err := http.ReadResponse(bufio.NewReader(c), nil)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		waitState(t, s, http.StateIdle)
		if err := s.srv.Shutdown(ended()); err != nil {
			t.Fatalf("Shutdown = %v, want nil", err)
		}
		assertClosed(t, c)
		if strings.Contains(s.out.String(), shutdownLine) {
			t.Errorf("idle connection logged a force-close:\n%s", s.out.String())
		}
	})
	t.Run("none", func(t *testing.T) {
		s := start(t, nil, nil)
		waitServing(t, s)
		if err := s.srv.Shutdown(ended()); err != nil {
			t.Fatalf("Shutdown = %v, want nil", err)
		}
		if strings.Contains(s.out.String(), shutdownLine) {
			t.Errorf("no connections logged a force-close:\n%s", s.out.String())
		}
	})
}

// TestShutdownDrainsInTime: a request that ends within ctx drains, gets
// its response, and no force-close is logged.
func TestShutdownDrainsInTime(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	s := start(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
	}), nil)
	c := s.dial(t)
	if _, err := io.WriteString(c, "GET /block HTTP/1.1\r\nHost: localhost\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	go func() {
		time.Sleep(50 * time.Millisecond)
		close(release)
	}()
	if err := s.srv.Shutdown(ctx); err != nil {
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

// failClose is a listener whose Close fails.
type failClose struct{ net.Listener }

var errClose = errors.New("close failed")

func (l failClose) Close() error {
	l.Listener.Close()
	return errClose
}

// TestShutdownEndedCtxListenerCloseFails: a force-close is not an error,
// but failing to close a listener still is.
func TestShutdownEndedCtxListenerCloseFails(t *testing.T) {
	s := start(t, nil, func(l net.Listener) net.Listener { return failClose{l} })
	c := s.dial(t)
	waitState(t, s, http.StateNew)
	if err := s.srv.Shutdown(ended()); !errors.Is(err, errClose) {
		t.Fatalf("Shutdown = %v, want the listener's close error", err)
	}
	assertClosed(t, c)
	if strings.Contains(s.out.String(), shutdownLine) {
		t.Errorf("failed shutdown logged a force-close:\n%s", s.out.String())
	}
}

// TestShutdownHijackedConn: a connection a handler took over, which the
// HTTP server no longer tracks, closes too once its request's context
// ends, whether the drain finishes in time or not.
func TestShutdownHijackedConn(t *testing.T) {
	for name, ctx := range map[string]func() context.Context{"ended": ended, "in time": context.Background} {
		t.Run(name, func(t *testing.T) {
			s := start(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				c, _, err := http.NewResponseController(w).Hijack()
				if err != nil {
					return
				}
				defer c.Close()
				_, _ = io.WriteString(c, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
				<-r.Context().Done()
			}), nil)
			c := s.dial(t)
			if _, err := io.WriteString(c, "GET /ws HTTP/1.1\r\nHost: localhost\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n"); err != nil {
				t.Fatal(err)
			}
			resp, err := http.ReadResponse(bufio.NewReader(c), nil)
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != http.StatusSwitchingProtocols {
				t.Fatalf("status = %d, want 101", resp.StatusCode)
			}
			if err := s.srv.Shutdown(ctx()); err != nil {
				t.Fatalf("Shutdown = %v, want nil", err)
			}
			assertClosed(t, c)
		})
	}
}

// waitServing waits until the Server serves: a connection to it is seen.
func waitServing(t *testing.T, s *served) {
	t.Helper()
	c := s.dial(t)
	waitState(t, s, http.StateNew)
	c.Close()
	waitState(t, s, http.StateClosed)
}

// waitState waits until one of the Server's connections is in state st.
func waitState(t *testing.T, s *served, st http.ConnState) {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		s.mu.Lock()
		found := false
		for _, got := range s.states {
			found = found || got == st
		}
		change := s.change
		s.mu.Unlock()
		if found {
			return
		}
		select {
		case <-change:
		case <-timeout:
			t.Fatalf("no connection in state %v", st)
		}
	}
}
