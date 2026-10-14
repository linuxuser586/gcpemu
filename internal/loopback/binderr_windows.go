package loopback

import (
	"syscall"

	"golang.org/x/sys/windows"
)

// Winsock reports bind failures as WSAEADDRINUSE and WSAEACCES, which Go's
// syscall.EADDRINUSE and EACCES do not match on Windows.
var (
	addrInUseErrnos  = []error{windows.WSAEADDRINUSE, syscall.EADDRINUSE}
	bindDeniedErrnos = []error{windows.WSAEACCES, syscall.EACCES}
)
