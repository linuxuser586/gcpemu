//go:build unix

package lanbind

import (
	"errors"
	"syscall"
)

// NotLocal reports whether err is a bind refused because the address is
// not one of the network namespace's.
func NotLocal(err error) bool { return errors.Is(err, syscall.EADDRNOTAVAIL) }
