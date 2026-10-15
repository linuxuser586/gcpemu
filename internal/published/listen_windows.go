package published

import "syscall"

// reusePort does nothing on Windows, where an Instance never runs in a
// Linux container.
func reusePort(_, _ string, _ syscall.RawConn) error { return nil }
