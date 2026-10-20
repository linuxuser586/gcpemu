package lanbind

import (
	"errors"

	"golang.org/x/sys/windows"
)

// NotLocal reports whether err is a bind refused because the address is
// not one of the host's.
func NotLocal(err error) bool { return errors.Is(err, windows.WSAEADDRNOTAVAIL) }
