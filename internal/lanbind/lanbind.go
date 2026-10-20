// Package lanbind holds what the emulator says about the --lb-bind,
// --gke-bind, --ar-bind and --gfe-bind addresses in a Containerized
// Instance, in one place so that start, the Services and doctor say the
// same (#497, ADR 0011).
//
// A Containerized Instance on a bridge network has only its container's
// addresses: they reach container networks, never the LAN. Publishing a
// port on a host address is no substitute (its listeners are --bind's,
// which don't require credentials), so the fix is to run the container on
// the host's network, or the native binary on a VM runtime.
package lanbind

import (
	"fmt"
	"strings"
)

// Fix is how a Containerized Instance serves these flags on the LAN.
const Fix = "to reach the LAN, run the container with `--network host` on Linux, or the native binary on a VM runtime (ADR 0011)"

// Where is the network namespace the Instance binds the addresses in.
type Where int

const (
	// Host: the host, or a container on the host's network, where the
	// flags work as documented.
	Host Where = iota
	// Bridge: a Containerized Instance whose runtime confirms its own
	// container, so not on the host's network.
	Bridge
	// Unconfirmed: a container whose runtime can't confirm it (no
	// socket, or not reachable yet); it may be on the host's network.
	Unconfirmed
)

func (w Where) String() string {
	switch w {
	case Bridge:
		return "bridge"
	case Unconfirmed:
		return "unconfirmed"
	}
	return "host"
}

// Locate decides Where: inContainer is runtime.InContainer, hasRuntime
// whether the runtime was reached, and self its self container ("" on the
// host and on the host's network).
func Locate(inContainer, hasRuntime bool, self string) Where {
	switch {
	case !inContainer:
		return Host
	case !hasRuntime:
		return Unconfirmed
	case self != "":
		return Bridge
	}
	return Host
}

// Explain adds the cause and Fix to err, the failure to bind a --*-bind
// address, when the address is not one of the Instance's network
// namespace in a container. Any other err, and any err on the Host, is
// returned unchanged.
func Explain(w Where, err error) error {
	if w == Host || !NotLocal(err) {
		return err
	}
	cause := "not an address of this container"
	if w == Unconfirmed {
		cause = "may not be an address of this container"
	}
	return fmt.Errorf("%w (%s: these addresses reach only container networks; %s)", err, cause, Fix)
}

// Warning is the start warning for the flags set (without "--") in a
// Bridge Containerized Instance.
func Warning(flags []string) string {
	named := make([]string, len(flags))
	for i, f := range flags {
		named[i] = "--" + f
	}
	return fmt.Sprintf("%s: the Instance runs in a container on a bridge network, so these addresses reach only container networks, not the LAN; %s",
		strings.Join(named, ", "), Fix)
}
