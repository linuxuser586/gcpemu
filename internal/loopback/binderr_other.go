//go:build !windows

package loopback

import "syscall"

var (
	addrInUseErrnos  = []error{syscall.EADDRINUSE}
	bindDeniedErrnos = []error{syscall.EACCES}
)
