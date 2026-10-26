// Package porttest picks free ports for gcpemu's own tests.
//
// A test that listens on port 0, closes the listener and binds the port
// again later leaves a window in which another process can take it: the
// OS hands out port-0 and outgoing-connection ports from its ephemeral
// range, which test packages running in parallel use all the time. So
// ports come from 20000-32767 instead, below the ephemeral range on
// Linux (32768-60999), macOS and Windows (49152-65535), where only
// explicit binds take them. Each call starts at a random port, so
// parallel packages do not all probe the same ones.
//
// A test whose code under test can take a listener the test already
// holds should hold its port and hand it over instead (#643, #648).
// Tests use this package rather than probing port 0 themselves (#743,
// #750).
package porttest

import (
	"io"
	"math/rand/v2"
	"net"
	"strconv"
	"sync"
	"testing"
)

// base is the range's first port, limit the port just past its last.
const base, limit = 20000, 32768

// Window returns the first port of n contiguous ports that can all be
// bound, scanning from a random port of the range.
func Window(t testing.TB, n int) int {
	t.Helper()
	return scan(t, n, base+rand.IntN(limit-base), tcpProbe)
}

// Free returns a port that can be bound, as Window(t, 1).
func Free(t testing.TB) int {
	t.Helper()
	return Window(t, 1)
}

// FreeUDP returns a port that a plain udp4 bind on 0.0.0.0 can take,
// scanning from a random port of the range. A port any IPv4 UDP socket
// holds is not free, even one bound with SO_REUSEADDR or SO_REUSEPORT,
// so a test's own mDNS port meets no other test's traffic. The code
// under test, or a child process, binds the port itself with
// SO_REUSEADDR, so the test can't hold it and hand it over.
func FreeUDP(t testing.TB) int {
	t.Helper()
	return scan(t, 1, base+rand.IntN(limit-base), udpProbe)
}

// probe binds port the way a caller needs it free and returns the
// sockets holding it.
type probe func(port int) (io.Closer, error)

// tcpProbe binds TCP on port on every address of hosts.
func tcpProbe(port int) (io.Closer, error) {
	var held closers
	for _, host := range hosts() {
		l, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
		if err != nil {
			held.Close()
			return nil, err
		}
		held = append(held, l)
	}
	return held, nil
}

// udpProbe binds udp4 on port on the wildcard address, which fails while
// any IPv4 UDP socket holds it.
func udpProbe(port int) (io.Closer, error) {
	return net.ListenPacket("udp4", net.JoinHostPort("0.0.0.0", strconv.Itoa(port)))
}

// closers holds a probe's sockets, closing them all at once.
type closers []io.Closer

func (cs closers) Close() error {
	for _, c := range cs {
		c.Close()
	}
	return nil
}

// scan is Window starting at port from, with ports free as probe finds
// them, wrapping to base when a window would run past the range.
func scan(t testing.TB, n, from int, probe probe) int {
	t.Helper()
	var lastErr error
	for lo, tried := from, 0; tried < limit-base; {
		if lo+n > limit {
			tried += limit - lo
			lo = base
			continue
		}
		var held closers
		next := 0
		for p := lo; p < lo+n; p++ {
			c, err := probe(p)
			if err != nil {
				lastErr = err
				next = p + 1
				break
			}
			held = append(held, c)
		}
		held.Close()
		if next == 0 {
			return lo
		}
		tried += next - lo
		lo = next
	}
	t.Fatalf("no %d contiguous free ports in %d-%d: %v", n, base, limit-1, lastErr)
	return 0
}

// hosts are the loopback addresses a port must be free on: 127.0.0.1,
// and [::1] where the host has it.
var hosts = sync.OnceValue(func() []string {
	l, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		return []string{"127.0.0.1"}
	}
	l.Close()
	return []string{"127.0.0.1", "::1"}
})
