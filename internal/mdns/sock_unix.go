//go:build unix

package mdns

import "golang.org/x/sys/unix"

// bindIP is the group address: bound to it, the Responder receives only
// the group's datagrams, never unicast meant for another socket on the
// port.
var bindIP = Group

// setReuse sets SO_REUSEADDR and SO_REUSEPORT, as mDNS daemons do, so that
// the port is shared with them.
func setReuse(fd uintptr) error {
	if err := unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEADDR, 1); err != nil {
		return err
	}
	return unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEPORT, 1)
}
