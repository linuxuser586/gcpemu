package porttest

import (
	"fmt"
	"net"
	"testing"
)

// TestWindow: the n ports are contiguous, below every CI OS's ephemeral
// range, and the test can bind each of them.
func TestWindow(t *testing.T) {
	const n = 3
	lo := Window(t, n)
	if lo < base || lo+n > limit {
		t.Fatalf("window %d-%d, want within %d-%d", lo, lo+n-1, base, limit-1)
	}
	for p := lo; p < lo+n; p++ {
		l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p))
		if err != nil {
			t.Fatalf("port %d of the window: %v", p, err)
		}
		l.Close()
	}
}

// TestScanSkipsHeld: a port held on either loopback address is not free,
// and no window that contains it is returned.
func TestScanSkipsHeld(t *testing.T) {
	for _, host := range []string{"127.0.0.1", "::1"} {
		t.Run(host, func(t *testing.T) {
			if l, err := net.Listen("tcp", net.JoinHostPort(host, "0")); err != nil {
				t.Skipf("no %s: %v", host, err)
			} else {
				l.Close()
			}
			p := Window(t, 2)
			held, err := net.Listen("tcp", net.JoinHostPort(host, fmt.Sprint(p+1)))
			if err != nil {
				t.Fatal(err)
			}
			defer held.Close()
			if lo := scan(t, 2, p, tcpProbe); lo <= p+1 && p+1 <= lo+1 {
				t.Fatalf("window %d-%d contains %d, held on %s", lo, lo+1, p+1, host)
			}
		})
	}
}

// TestScanWraps: a scan that starts too near the top of the range for
// the window wraps to its bottom.
func TestScanWraps(t *testing.T) {
	if lo := scan(t, 2, limit-1, tcpProbe); lo < base || lo+2 > limit {
		t.Fatalf("window %d-%d, want within %d-%d", lo, lo+1, base, limit-1)
	}
}

// TestWindowSpreads: each call starts at a random port, so test packages
// running in parallel do not all probe the same ports.
func TestWindowSpreads(t *testing.T) {
	first := Free(t)
	for range 10 {
		if Free(t) != first {
			return
		}
	}
	t.Fatalf("11 calls all returned %d", first)
}

// TestFreeUDP: the port is below every CI OS's ephemeral range, and a
// plain udp4 bind on the wildcard address takes it.
func TestFreeUDP(t *testing.T) {
	p := FreeUDP(t)
	if p < base || p >= limit {
		t.Fatalf("port %d, want within %d-%d", p, base, limit-1)
	}
	pc, err := net.ListenPacket("udp4", fmt.Sprintf("0.0.0.0:%d", p))
	if err != nil {
		t.Fatalf("port %d: %v", p, err)
	}
	pc.Close()
}

// TestScanUDPSkipsHeld: a port a plain udp4 socket holds on the wildcard
// address is not free.
func TestScanUDPSkipsHeld(t *testing.T) {
	p := FreeUDP(t)
	held, err := net.ListenPacket("udp4", fmt.Sprintf("0.0.0.0:%d", p))
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	if got := scan(t, 1, p, udpProbe); got == p {
		t.Fatalf("scan returned %d, held by a udp4 socket", p)
	}
}
