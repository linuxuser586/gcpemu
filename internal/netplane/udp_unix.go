//go:build unix

package netplane

import "syscall"

func setReuseAddr(fd uintptr) error {
	return syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1)
}
