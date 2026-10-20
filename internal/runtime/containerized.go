package runtime

import (
	"fmt"
	"strings"
)

// Is this a Containerized Instance? A container on the host's network is
// not one: it runs as on the host. The container marker files (InContainer)
// are there either way, so the runtime's answer wins when there is one:
// the emulator found its own container, not on the host's network (Self).
// Without it, --containerized decides, and "auto" goes by the markers
// (#502).

// ContainerizedEnv names the setting: "auto" (the default), "true" or
// "false".
const ContainerizedEnv = "GCPEMU_CONTAINERIZED"

// GuessWarning is what an Instance says when it assumes it is a
// Containerized Instance from the marker files alone.
const GuessWarning = "assuming a Containerized Instance: the container marker files are present and there is no container runtime socket to tell; " +
	"for a container on the host's network, set --containerized=false (" + ContainerizedEnv + "=false)"

// ContainerizedDecision is whether the Instance is a Containerized
// Instance.
type ContainerizedDecision struct {
	On bool
	// Guess: decided by the marker files alone, under auto with no
	// runtime (GuessWarning).
	Guess bool
	// Conflict, when not "", says how --containerized disagrees with the
	// runtime, which wins.
	Conflict string
}

// MayBeContainerized reports whether the Instance may be a Containerized
// Instance, given setting and the marker files: only then does asking the
// runtime matter.
func MayBeContainerized(setting string, markers bool) bool {
	return markers || strings.ToLower(setting) == "true"
}

// DecideContainerized decides from setting (--containerized), markers
// (InContainer), whether the runtime answered (rt), and its self container
// ("" on the host and on the host's network).
func DecideContainerized(setting string, markers, rt bool, self string) ContainerizedDecision {
	setting = strings.ToLower(setting)
	if rt {
		d := ContainerizedDecision{On: self != ""}
		if (setting == "true" && !d.On) || (setting == "false" && d.On) {
			where := "on the host's network or on the host"
			if d.On {
				where = "in its own container " + self + ", not on the host's network"
			}
			d.Conflict = fmt.Sprintf("--containerized=%s (%s) disagrees with the container runtime, which finds the emulator %s; going by the runtime",
				setting, ContainerizedEnv, where)
		}
		return d
	}
	switch setting {
	case "true":
		return ContainerizedDecision{On: true}
	case "false":
		return ContainerizedDecision{}
	}
	return ContainerizedDecision{On: markers, Guess: markers}
}
