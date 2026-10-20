package runtime

import (
	"fmt"
	"strings"
)

// Is this a Containerized Instance? A container on the host's network is
// not one: it runs as on the host. The container marker files (InContainer)
// are there either way, so the runtime's answer wins when there is one:
// it found the emulator's own container, on the host's network or off it
// (SelfFound). Without one (no runtime, or a container it can't find),
// --containerized decides, and "auto" goes by the markers (#502).

// ContainerizedEnv names the setting: "auto" (the default), "true" or
// "false".
const ContainerizedEnv = "GCPEMU_CONTAINERIZED"

// ContainerizedFacts is what DecideContainerized decides from.
type ContainerizedFacts struct {
	// Setting is --containerized.
	Setting string
	// Markers is InContainer.
	Markers bool
	// Runtime: the runtime was reached.
	Runtime bool
	// Found: it found the emulator's own container (SelfFound), Self
	// being "" on the host's network.
	Found bool
	Self  string
}

// ContainerizedDecision is whether the Instance is a Containerized
// Instance.
type ContainerizedDecision struct {
	On bool
	// Guess, when not "", says that the marker files alone decided, under
	// auto with no answer from the runtime, and why.
	Guess string
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

// DecideContainerized decides from f.
func DecideContainerized(f ContainerizedFacts) ContainerizedDecision {
	setting := strings.ToLower(f.Setting)
	if f.Runtime && f.Found {
		d := ContainerizedDecision{On: f.Self != ""}
		if (setting == "true" && !d.On) || (setting == "false" && d.On) {
			where := "on the host's network"
			if d.On {
				where = "in its own container " + f.Self + ", not on the host's network"
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
	if !f.Markers {
		return ContainerizedDecision{}
	}
	why, fix := "there is no container runtime socket to tell", ""
	if f.Runtime {
		why, fix = "the container runtime can't find the emulator's own container", "; to let it, set "+SelfEnv+"=<container name>"
	}
	return ContainerizedDecision{On: true, Guess: "assuming a Containerized Instance: the container marker files are present and " + why +
		"; for a container on the host's network, set --containerized=false (" + ContainerizedEnv + "=false)" + fix}
}
