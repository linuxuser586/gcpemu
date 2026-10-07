//go:build !linux

package compute

import (
	"errors"
	"net"
)

// originalDst is only available on Linux (the gateway runs in a Linux container).
func originalDst(net.Conn) (string, int, error) {
	return "", 0, errors.New("SO_ORIGINAL_DST is Linux-only")
}
