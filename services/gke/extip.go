package gke

import (
	"fmt"

	"github.com/linuxuser586/gcpemu/internal/netplane"
	"github.com/linuxuser586/gcpemu/internal/runtime"
)

// externalIPAttempts bounds how many addresses withExternalIP tries.
const externalIPAttempts = 5

// withExternalIP runs attempt with the k3s server's address on the
// external network ext and returns the address it succeeded with. The
// server's certificate must name its Public endpoint, and the runtime only
// assigns a dynamic address once the container starts, after its arguments
// are fixed (#644), so the emulator chooses the address itself: the
// recorded one while it is inside ext's subnet, so that the Public endpoint
// survives restarts, otherwise a new one. An address the runtime refuses as
// taken is replaced by another.
func withExternalIP(ext netplane.Net, recorded string, attempt func(ip string) error) (string, error) {
	tried := map[string]bool{}
	preferred := recorded
	var err error
	// Each draw may repeat an address already tried, on a small subnet
	// always, so the draws are bounded too.
	for draws := 0; len(tried) < externalIPAttempts && draws < 4*externalIPAttempts; draws++ {
		ip, perr := ext.PickAddr(preferred)
		if perr != nil {
			return "", perr
		}
		preferred = ""
		if tried[ip] {
			continue
		}
		tried[ip] = true
		if err = attempt(ip); !runtime.IsAddressInUse(err) {
			return ip, err
		}
	}
	// The runtime words a taken host port the same way, so the last error
	// is kept for the reader.
	return "", fmt.Errorf("network %s: the runtime refused %d addresses for the control plane as in use: %w", ext.Name, len(tried), err)
}
