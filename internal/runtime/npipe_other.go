//go:build !windows

package runtime

import (
	"context"
	"errors"
	"net"
)

// pipeDialer refuses: named pipes exist only on Windows.
func pipeDialer(string) (func(context.Context) (net.Conn, error), error) {
	return nil, errors.New("npipe:// runtime endpoints are only supported on Windows")
}
