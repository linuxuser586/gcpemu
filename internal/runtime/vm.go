package runtime

import "strings"

// A Containerized Instance on a VM runtime (ADR 0012). Its host cannot
// route into the VM, so container addresses it reports time out there;
// it serves host clients through its own published ports instead. It
// cannot probe the host's routing from inside its container, so it guesses
// from the runtime's info, and GCPEMU_VM_RUNTIME overrides the guess.

// VMRuntimeEnv names the setting: "auto" (the default), "true" or "false".
const VMRuntimeEnv = "GCPEMU_VM_RUNTIME"

// VMDecision says whether a Containerized Instance treats its runtime as a
// VM runtime, and why.
type VMDecision struct {
	On     bool
	Reason string
}

// String says "yes" or "no", with the reason.
func (d VMDecision) String() string {
	if d.On {
		return "yes (" + d.Reason + ")"
	}
	return "no (" + d.Reason + ")"
}

// DecideVM chooses for an Instance: never on the host, where the VM relay
// (ADR 0005) covers VM runtimes; otherwise as setting says ("true",
// "false"), or for "auto" and "" by the runtime's operating system and
// kernel, which name Docker Desktop and OrbStack.
func DecideVM(setting string, inContainer bool, info Info) VMDecision {
	if !inContainer {
		return VMDecision{Reason: "the emulator runs on the host"}
	}
	switch strings.ToLower(setting) {
	case "true":
		return VMDecision{On: true, Reason: "forced by " + VMRuntimeEnv}
	case "false":
		return VMDecision{Reason: "disabled by " + VMRuntimeEnv}
	}
	os, kernel := strings.ToLower(info.OS), strings.ToLower(info.Kernel)
	switch {
	case strings.Contains(os, "docker desktop"), strings.Contains(os, "orbstack"):
		return VMDecision{On: true, Reason: "runtime OS " + info.OS}
	case strings.Contains(kernel, "linuxkit"), strings.Contains(kernel, "orbstack"):
		return VMDecision{On: true, Reason: "runtime kernel " + info.Kernel}
	case info.OS != "":
		return VMDecision{Reason: "runtime OS " + info.OS + "; set " + VMRuntimeEnv + "=true on Colima or Podman machine"}
	}
	return VMDecision{Reason: "runtime OS unknown; set " + VMRuntimeEnv + "=true on a VM runtime"}
}
