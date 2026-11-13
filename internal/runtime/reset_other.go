//go:build !windows

package runtime

import (
	"errors"
	"syscall"
)

// connReset reports whether err is the peer resetting or closing a
// connection we were still using.
func connReset(err error) bool {
	return errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE)
}
