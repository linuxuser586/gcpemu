package emu

import (
	"errors"
	"fmt"
	"net"
	"testing"

	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/porttest"
)

// TestListenTCPRange: port 0 is taken from --port-range; an exhausted
// range is an error (FR-CORE-042).
func TestListenTCPRange(t *testing.T) {
	lo := porttest.Window(t, 3)
	hi := lo + 2
	cfg := config.Defaults()
	cfg.PortRange = fmt.Sprintf("%d-%d", lo, hi)
	e := &Env{Config: &cfg}
	var ls []net.Listener
	defer func() {
		for _, l := range ls {
			l.Close()
		}
	}()
	seen := map[int]bool{}
	for range 3 {
		l, err := e.ListenTCP("127.0.0.1", 0)
		if err != nil {
			t.Fatal(err)
		}
		ls = append(ls, l)
		p := l.Addr().(*net.TCPAddr).Port
		if p < lo || p > hi || seen[p] {
			t.Fatalf("port %d outside %d-%d or repeated (seen %v)", p, lo, hi, seen)
		}
		seen[p] = true
	}
	if _, err := e.ListenTCP("127.0.0.1", 0); err == nil {
		t.Fatal("exhausted range still allocated a port")
	}
	cfg.PortRange = ""
	l, err := e.ListenTCP("127.0.0.1", 0)
	if err != nil {
		t.Fatal(err)
	}
	l.Close()
}

// TestEndpointsDialable: a Service endpoint is never an unspecified
// address; a listener bound to all interfaces is recorded as loopback,
// which clients on every OS can dial (#613).
func TestEndpointsDialable(t *testing.T) {
	for _, tc := range []struct{ addr, want string }{
		{"0.0.0.0:4562", "127.0.0.1:4562"},
		{"[::]:4562", "127.0.0.1:4562"},
		{"127.0.0.1:4562", "127.0.0.1:4562"},
		{"[::1]:4562", "[::1]:4562"},
		{"192.0.2.1:4562", "192.0.2.1:4562"},
		{"192.0.2.1:443,192.0.2.2:443", "192.0.2.1:443,192.0.2.2:443"},
	} {
		e := NewEndpoints()
		e.Set("gateway", tc.addr)
		if got := e.Get("gateway"); got != tc.want {
			t.Errorf("Set(%q): Get = %q, want %q", tc.addr, got, tc.want)
		}
	}
}

// NoRuntime marks a permanent runtime failure and keeps the error it
// wraps, message and all (#313).
func TestNoRuntime(t *testing.T) {
	cause := errors.New("no runtime found")
	err := NoRuntime(cause)
	if !errors.Is(err, ErrNoRuntime) || !errors.Is(err, cause) {
		t.Errorf("NoRuntime(%v) matches ErrNoRuntime %v, cause %v; want both", cause, errors.Is(err, ErrNoRuntime), errors.Is(err, cause))
	}
	if err.Error() != cause.Error() {
		t.Errorf("message %q, want %q", err, cause)
	}
	if errors.Is(cause, ErrNoRuntime) {
		t.Error("an unmarked error matches ErrNoRuntime")
	}
}
