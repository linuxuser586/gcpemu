//go:build linux

package compute

import (
	"encoding/binary"
	"errors"
	"net"
	"syscall"
)

// soOriginalDst is SO_ORIGINAL_DST from linux/netfilter_ipv4.h.
const soOriginalDst = 80

// originalDst returns the pre-REDIRECT destination of a TCP connection.
func originalDst(c net.Conn) (string, int, error) {
	tc, ok := c.(*net.TCPConn)
	if !ok {
		return "", 0, errors.New("not a TCP connection")
	}
	raw, err := tc.SyscallConn()
	if err != nil {
		return "", 0, err
	}
	var ip string
	var port int
	var serr error
	err = raw.Control(func(fd uintptr) {
		mreq, e := syscall.GetsockoptIPv6Mreq(int(fd), syscall.IPPROTO_IP, soOriginalDst)
		if e != nil {
			serr = e
			return
		}
		// struct sockaddr_in: family(2) port(2, big endian) addr(4).
		port = int(binary.BigEndian.Uint16(mreq.Multiaddr[2:4]))
		ip = net.IP(mreq.Multiaddr[4:8]).String()
	})
	if err == nil {
		err = serr
	}
	return ip, port, err
}
