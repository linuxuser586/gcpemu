package runtime

import (
	"cmp"
	"context"
	"fmt"
	"strings"
)

// readableLogs sets the json-file log driver on a Podman container spec
// that names no driver; Podman writes it as k8s-file. The default on a
// Podman machine, journald, is one Podman's Engine API cannot read logs
// from ("unable to open a handle to the library"), so a container that
// exited before it was ready left no log tail in the error (#633).
//
// Docker Engine keeps its daemon's driver: since 20.10 it reads logs from
// any driver through its dual-logging cache, unless that is disabled.
func (m *Manager) readableLogs(s ContainerSpec) ContainerSpec {
	if m.Info.Name != "podman" || s.LogDriver != "" {
		return s
	}
	s.LogDriver = "json-file"
	// Bounds the log file, as journald bounds the journal.
	s.LogOpts = map[string]string{"max-size": "10m"}
	return s
}

// LogTail returns the last n lines of container ct's output. If the
// runtime cannot read them, it returns the error instead and, on Podman,
// how to read them from the journal, where the journald log driver keeps
// them even after the container is removed.
func (m *Manager) LogTail(ctx context.Context, ct Container, n int) string {
	logs, err := m.Logs(ctx, cmp.Or(ct.ID, ct.Name), n)
	if err == nil {
		return strings.TrimSpace(logs)
	}
	msg := "logs unavailable: " + err.Error()
	if m.Info.Name == "podman" {
		journalctl := "sudo journalctl"
		if m.Info.Rootless {
			journalctl = "journalctl --user"
		}
		msg += fmt.Sprintf("; `podman logs` is unavailable with the journald log driver; read `%s CONTAINER_NAME=%s`, on a Podman machine inside it (`podman machine ssh`)", journalctl, ct.Name)
	}
	return "(" + msg + ")"
}
