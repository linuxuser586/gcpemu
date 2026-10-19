//go:build unix

package netplane

import "syscall"

var (
	// plainBindErrs: a plain bind beside a wildcard SO_REUSEADDR socket.
	plainBindErrs = []error{syscall.EADDRINUSE}
	// holderRefusesPlainBinds: the test's holder refuses specific and
	// wildcard plain binds, as an mDNS daemon does.
	holderRefusesPlainBinds = true
	// noIPv6Errs: a [::] bind on a host without IPv6.
	noIPv6Errs = []error{syscall.EADDRNOTAVAIL, syscall.EAFNOSUPPORT}
)
