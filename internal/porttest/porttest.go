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
// Tests use this package rather than probing port 0 themselves (#743).
package porttest

import (
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
	return scan(t, n, base+rand.IntN(limit-base))
}

// Free returns a port that can be bound, as Window(t, 1).
func Free(t testing.TB) int {
	t.Helper()
	return Window(t, 1)
}

// scan is Window starting at port from, wrapping to base when a window
// would run past the range.
func scan(t testing.TB, n, from int) int {
	t.Helper()
	for lo, tried := from, 0; tried < limit-base; {
		if lo+n > limit {
			tried += limit - lo
			lo = base
			continue
		}
		var probes []net.Listener
		next := 0
	probe:
		for p := lo; p < lo+n; p++ {
			for _, host := range hosts() {
				l, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(p)))
				if err != nil {
					next = p + 1
					break probe
				}
				probes = append(probes, l)
			}
		}
		for _, l := range probes {
			l.Close()
		}
		if next == 0 {
			return lo
		}
		tried += next - lo
		lo = next
	}
	t.Fatalf("no %d contiguous free ports on %v in %d-%d", n, hosts(), base, limit-1)
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
