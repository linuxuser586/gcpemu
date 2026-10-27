//go:build linux || darwin

package porttest

import (
	"context"
	"fmt"
	"net"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// TestScanUDPSkipsReuseHeld: a port a udp4 socket holds on the wildcard
// address with SO_REUSEADDR and SO_REUSEPORT, as the mDNS code binds it,
// is not free.
func TestScanUDPSkipsReuseHeld(t *testing.T) {
	p := FreeUDP(t)
	lc := net.ListenConfig{Control: func(_, _ string, c syscall.RawConn) error {
		var serr error
		err := c.Control(func(fd uintptr) {
			for _, opt := range []int{unix.SO_REUSEADDR, unix.SO_REUSEPORT} {
				if serr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, opt, 1); serr != nil {
					return
				}
			}
		})
		if err != nil {
			return err
		}
		return serr
	}}
	held, err := lc.ListenPacket(context.Background(), "udp4", fmt.Sprintf("0.0.0.0:%d", p))
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	if got := scan(t, 1, p, udpProbe); got == p {
		t.Fatalf("scan returned %d, held with SO_REUSEADDR and SO_REUSEPORT", p)
	}
}
