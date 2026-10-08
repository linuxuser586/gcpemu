//go:build !linux

package compute

import (
	"context"
	"errors"
)

// readNflog is only available on Linux (the gateway runs in a Linux container).
func readNflog(context.Context, uint16, func([]byte)) error {
	return errors.New("NFLOG is Linux-only")
}
