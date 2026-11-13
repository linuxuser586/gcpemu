package runtime

import "syscall"

// resetErrs are the errors of a peer resetting or closing a connection we
// were still using. Windows sockets report a reset as WSAECONNRESET or
// WSAECONNABORTED, not as ECONNRESET.
var resetErrs = []error{syscall.ECONNRESET, syscall.EPIPE, syscall.WSAECONNRESET, syscall.WSAECONNABORTED}
