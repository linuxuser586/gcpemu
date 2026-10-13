//go:build !windows

package lb

import "syscall"

var (
	addrInUseErrnos  = []error{syscall.EADDRINUSE}
	bindDeniedErrnos = []error{syscall.EACCES}
)
