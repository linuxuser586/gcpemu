//go:build !windows

package runtime

import "syscall"

// resetErrs are the errors of a peer resetting or closing a connection we
// were still using.
var resetErrs = []error{syscall.ECONNRESET, syscall.EPIPE}
