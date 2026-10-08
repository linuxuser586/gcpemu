//go:build linux

package compute

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"syscall"
)

// readNflog binds NFLOG group and calls fn with every datagram received
// until ctx ends.
func readNflog(ctx context.Context, group uint16, fn func([]byte)) error {
	fd, err := syscall.Socket(syscall.AF_NETLINK, syscall.SOCK_RAW|syscall.SOCK_CLOEXEC, syscall.NETLINK_NETFILTER)
	if err != nil {
		return err
	}
	defer syscall.Close(fd)
	if err := syscall.Bind(fd, &syscall.SockaddrNetlink{Family: syscall.AF_NETLINK}); err != nil {
		return err
	}
	// A receive timeout lets the loop notice ctx cancellation.
	tv := syscall.Timeval{Sec: 1}
	if err := syscall.SetsockoptTimeval(fd, syscall.SOL_SOCKET, syscall.SO_RCVTIMEO, &tv); err != nil {
		return err
	}
	if err := syscall.Sendto(fd, nflogConfigMsg(group, 1), 0, &syscall.SockaddrNetlink{Family: syscall.AF_NETLINK}); err != nil {
		return err
	}
	buf := make([]byte, 1<<16)
	acked := false
	for ctx.Err() == nil {
		n, _, err := syscall.Recvfrom(fd, buf, 0)
		if err != nil {
			if errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EINTR) || errors.Is(err, syscall.ENOBUFS) {
				continue
			}
			return err
		}
		b := buf[:n]
		if !acked {
			var cerr error
			nlmsgs(b, func(typ uint16, body []byte) {
				if typ == nlmsgError && len(body) >= 4 {
					acked = true
					if e := int32(binary.NativeEndian.Uint32(body)); e != 0 {
						cerr = fmt.Errorf("bind group %d: %w", group, syscall.Errno(-e))
					}
				}
			})
			if cerr != nil {
				return cerr
			}
		}
		fn(b)
	}
	return nil
}
