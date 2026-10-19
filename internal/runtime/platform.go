package runtime

import (
	"errors"
	goruntime "runtime"
)

// ErrNeedsWSL2 is why the container runtime is unusable on native Windows.
var ErrNeedsWSL2 = errors.New("the container runtime is not usable on native Windows; run gcpemu in WSL2")

// Unusable reports why an Instance cannot use a container runtime on this
// OS, or nil when it can; see UnusableOn.
func Unusable() error { return UnusableOn(goruntime.GOOS) }

// UnusableOn reports why an Instance cannot use a container runtime on
// goos, or nil when it can. It is decided by OS, not by detection: on native
// Windows, Docker Desktop's bridge networks are inside the WSL2 VM and not
// routable from the host, whatever $DOCKER_HOST names (ADR 0008). ADR 0015
// lifts it (#373): the client already dials named-pipe endpoints.
func UnusableOn(goos string) error {
	if goos == "windows" {
		return ErrNeedsWSL2
	}
	return nil
}
