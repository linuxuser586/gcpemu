package runtime

import "fmt"

// CPUSetProblem says why GKE nodes cannot start on a runtime whose
// containers lack the cpuset cgroup controller (Info.NoCPUSet), and the
// fix for a host of goos (#632): a rootful Podman machine, or cpuset
// delegated to the user on rootless Linux. Both are "" when the runtime
// has the controller or did not say. On Linux a Podman runtime may still
// be a machine, gcpemu running in a container on it, so both fixes are
// given.
func CPUSetProblem(in Info, goos string) (problem, fix string) {
	if !in.NoCPUSet {
		return "", ""
	}
	problem = fmt.Sprintf("container runtime %s gives containers no cpuset cgroup controller, so GKE nodes (k3s) cannot start", in)
	machine := "`podman machine stop && podman machine set --rootful && podman machine start`"
	delegate := "delegate cpuset to the user: put `[Service]` and `Delegate=cpu cpuset io memory pids` in /etc/systemd/system/user@.service.d/delegate.conf, run `sudo systemctl daemon-reload`, and log in again"
	switch {
	case goos != "linux" && in.Name == "podman":
		fix = "make the Podman machine rootful: " + machine
	case goos != "linux":
		fix = "run the container runtime rootful, so containers get every cgroup controller"
	case in.Name == "podman":
		fix = "on a Podman machine, make it rootful: " + machine + "; on rootless Podman on Linux, " + delegate
	default:
		fix = delegate
	}
	return problem, fix
}
