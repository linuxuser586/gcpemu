package dns

import (
	"context"
	"net"
	"syscall"
)

// listenUDP binds the UDP side of the DNS server. Port 5353 is also the
// mDNS port: avahi-daemon, systemd-resolved and browsers hold UDP
// 0.0.0.0:5353 with SO_REUSEADDR on most Linux desktops, so a plain bind
// of 127.0.0.1:5353 fails with "address already in use". Setting
// SO_REUSEADDR on our socket lets it coexist; unicast queries to the
// specific bind address reach our socket while multicast mDNS traffic keeps
// going to the wildcard sockets. On a wildcard bind we would share (and
// steal) mDNS traffic, so reuse is only enabled for specific addresses.
// The TCP listener on the same port is bound first without reuse, so a
// second emulator instance still fails cleanly.
func listenUDP(ctx context.Context, host, port string) (net.PacketConn, error) {
	addr := net.JoinHostPort(host, port)
	ip := net.ParseIP(host)
	if host == "" || ip == nil || ip.IsUnspecified() {
		return net.ListenPacket("udp", addr)
	}
	lc := net.ListenConfig{Control: func(_, _ string, c syscall.RawConn) error {
		var serr error
		if err := c.Control(func(fd uintptr) {
			serr = setReuseAddr(fd)
		}); err != nil {
			return err
		}
		return serr
	}}
	return lc.ListenPacket(ctx, "udp", addr)
}
