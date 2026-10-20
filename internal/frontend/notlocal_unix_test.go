//go:build unix

package frontend_test

import "syscall"

// errNotLocal is the errno a bind returns for an address that is not one
// of the network namespace's, as lanbind.NotLocal recognizes it.
var errNotLocal error = syscall.EADDRNOTAVAIL
