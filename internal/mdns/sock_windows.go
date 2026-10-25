package mdns

import (
	"net"
	"syscall"
)

// bindIP is the wildcard: Windows cannot bind a multicast address.
var bindIP = net.IPv4zero

// setReuse sets SO_REUSEADDR, as Windows' DNS Client and browsers do on
// 5353, so that the port is shared with them.
func setReuse(fd uintptr) error {
	return syscall.SetsockoptInt(syscall.Handle(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1)
}
