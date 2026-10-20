package frontend_test

import "golang.org/x/sys/windows"

// errNotLocal is the error a bind returns for an address that is not one
// of the host's, as lanbind.NotLocal recognizes it.
var errNotLocal error = windows.WSAEADDRNOTAVAIL
