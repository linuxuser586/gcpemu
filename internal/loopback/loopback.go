// Package loopback reports whether the host has loopback addresses beyond
// 127.0.0.1 (ADR 0005). Linux routes all of 127.0.0.0/8 to loopback; a
// macOS host has only 127.0.0.1 on lo0 until `gcpemu loopback install`
// adds aliases for 127.0.0.2 to 127.0.0.254.
package loopback

import "net"

// InstallHint is the command that adds the aliases on a host without them.
const InstallHint = "sudo gcpemu loopback install"

// Aliases reports whether 127.0.0.x addresses other than 127.0.0.1 can be
// bound, by binding 127.0.0.2:0.
func Aliases() bool {
	l, err := net.Listen("tcp", "127.0.0.2:0")
	if err != nil {
		return false
	}
	_ = l.Close()
	return true
}
