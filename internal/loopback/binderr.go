package loopback

import (
	"errors"
	"fmt"
	goruntime "runtime"
)

// AddrInUse reports whether err is a bind failing because another socket
// holds the address.
func AddrInUse(err error) bool { return isAny(err, addrInUseErrnos) }

// Refusal is what a bind refused for lack of permission means.
type Refusal int

const (
	// NotRefused: the bind succeeded, or failed for another reason.
	NotRefused Refusal = iota
	// Privileged: a privileged port, which only a privileged process
	// binds.
	Privileged
	// PrivilegedWildcard: a privileged port on macOS, which an
	// unprivileged process binds only on the wildcard address.
	PrivilegedWildcard
	// Excluded: a port in one of Windows' excluded port ranges, which
	// Hyper-V, WSL and others reserve. Windows has no privileged ports, and
	// no address or relay binds an excluded one (ADR 0015).
	Excluded
)

func (r Refusal) String() string {
	switch r {
	case NotRefused:
		return "not refused"
	case Privileged:
		return "privileged"
	case PrivilegedWildcard:
		return "privileged (wildcard only)"
	case Excluded:
		return "excluded"
	}
	return fmt.Sprintf("Refusal(%d)", int(r))
}

// Refused reports why the host refused the bind that returned err.
func Refused(err error) Refusal { return RefusedOn(goruntime.GOOS, err) }

// RefusedOn is Refused on goos.
func RefusedOn(goos string, err error) Refusal {
	if !isAny(err, bindDeniedErrnos) {
		return NotRefused
	}
	return RefusalOn(goos)
}

// RefusalOn is what a bind refused for lack of permission means on goos.
func RefusalOn(goos string) Refusal {
	switch goos {
	case "darwin":
		return PrivilegedWildcard
	case "windows":
		return Excluded
	}
	return Privileged
}

// ExcludedError wraps err, a bind refused as Excluded, with the command
// that lists the excluded ranges of network ("tcp" or "udp").
func ExcludedError(err error, network string) error {
	return fmt.Errorf("%w (the port is in one of Windows' excluded port ranges, which Hyper-V, WSL and others reserve; "+
		"`netsh int ipv4 show excludedportrange protocol=%s` lists them)", err, network)
}

func isAny(err error, targets []error) bool {
	for _, t := range targets {
		if errors.Is(err, t) {
			return true
		}
	}
	return false
}
