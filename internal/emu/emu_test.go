package emu

import (
	"errors"
	"fmt"
	"net"
	"testing"

	"github.com/linuxuser586/gcpemu/internal/config"
)

// TestListenTCPRange: port 0 is taken from --port-range; an exhausted
// range is an error (FR-CORE-042).
func TestListenTCPRange(t *testing.T) {
	lo := freePortWindow(t, 3)
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

// freePortWindow returns the first port of n contiguous ports on
// 127.0.0.1 that can all be bound. It scans below Linux's ephemeral
// range (32768-60999), where only explicit binds take ports, so outgoing
// connections from other test packages cannot claim the window while the
// test uses it.
func freePortWindow(t *testing.T, n int) int {
	t.Helper()
	const base, limit = 20000, 32768
	for lo := base; lo+n <= limit; {
		var probes []net.Listener
		next := 0
		for p := lo; p < lo+n; p++ {
			l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p))
			if err != nil {
				next = p + 1
				break
			}
			probes = append(probes, l)
		}
		for _, l := range probes {
			l.Close()
		}
		if next == 0 {
			return lo
		}
		lo = next
	}
	t.Fatalf("no %d contiguous free ports on 127.0.0.1 in %d-%d", n, base, limit-1)
	return 0
}

// TestEndpointsDialable: a Service endpoint is never an unspecified
// address; a listener bound to all interfaces is recorded as loopback,
// which clients on every OS can dial (#613).
func TestEndpointsDialable(t *testing.T) {
	for _, tc := range []struct{ addr, want string }{
		{"0.0.0.0:4510", "127.0.0.1:4510"},
		{"[::]:4510", "127.0.0.1:4510"},
		{"127.0.0.1:4510", "127.0.0.1:4510"},
		{"[::1]:4510", "[::1]:4510"},
		{"192.0.2.1:4510", "192.0.2.1:4510"},
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
