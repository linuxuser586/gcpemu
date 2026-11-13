package runtime

import (
	"errors"
	"syscall"
)

// connReset reports whether err is the peer resetting or closing a
// connection we were still using. Windows sockets report a reset as
// WSAECONNRESET or WSAECONNABORTED, not as ECONNRESET, and a named pipe
// whose server closed as ERROR_BROKEN_PIPE.
func connReset(err error) bool {
	for _, e := range []error{syscall.ECONNRESET, syscall.EPIPE, syscall.WSAECONNRESET, syscall.WSAECONNABORTED, syscall.ERROR_BROKEN_PIPE} {
		if errors.Is(err, e) {
			return true
		}
	}
	return false
}
