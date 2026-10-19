package netplane

import "syscall"

// On Windows, Chrome and the DNS Client service hold UDP 0.0.0.0:5353 with
// SO_REUSEADDR for mDNS; a plain bind of a specific address on that port
// fails with WSAEACCES, and SO_REUSEADDR lets it coexist as on Unix.
func setReuseAddr(fd uintptr) error {
	return syscall.SetsockoptInt(syscall.Handle(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1)
}
