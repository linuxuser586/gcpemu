package netplane

import "golang.org/x/sys/windows"

var (
	// plainBindErrs: a plain bind beside a wildcard SO_REUSEADDR socket.
	plainBindErrs = []error{windows.WSAEACCES, windows.WSAEADDRINUSE}
	// holderRefusesPlainBinds: Winsock refuses plain binds (WSAEACCES)
	// beside an mDNS socket of another security context, such as Chrome's
	// sandboxed network service or the DNS Client service. Beside a holder
	// in the test's own process, specific and wildcard binds both succeed.
	holderRefusesPlainBinds = false
	// noIPv6Errs: a [::] bind on a host without IPv6.
	noIPv6Errs = []error{windows.WSAEADDRNOTAVAIL, windows.WSAEAFNOSUPPORT}
)
