package runtime

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

// TestReadableLogs (#633): on Podman, Managed containers are created with
// the json-file log driver (Podman's k8s-file), whose logs the Engine API
// reads, not the machine's journald default, whose it cannot. Docker
// Engine keeps its daemon's driver, and an explicit driver is kept.
func TestReadableLogs(t *testing.T) {
	var got any
	cl := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		var body struct{ HostConfig map[string]any }
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		got = body.HostConfig["LogConfig"]
		_, _ = io.WriteString(w, `{"Id":"c1"}`)
	})
	for _, tt := range []struct {
		name    string
		runtime string
		spec    ContainerSpec
		want    any
	}{
		{"podman", "podman", ContainerSpec{Name: "n"}, map[string]any{"Type": "json-file", "Config": map[string]any{"max-size": "10m"}}},
		{"docker", "docker", ContainerSpec{Name: "n"}, nil},
		{"explicit", "podman", ContainerSpec{Name: "n", LogDriver: "none"}, map[string]any{"Type": "none", "Config": nil}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got = nil
			m := &Manager{Client: cl, Info: Info{Name: tt.runtime}}
			if _, err := m.CreateContainer(context.Background(), tt.spec); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("LogConfig = %v; want %v", got, tt.want)
			}
		})
	}
}

// TestLogTail (#633): a container's log tail, or, when the runtime cannot
// read it, the error and, on Podman, where the logs are.
func TestLogTail(t *testing.T) {
	fail := false
	cl := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if fail {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"message":"failed to obtain logs for Container 'n': unable to open a handle to the library"}`)
			return
		}
		_, _ = w.Write(frame(2, "level=fatal msg=boom\n"))
	})
	ctx := context.Background()
	m := &Manager{Client: cl, Info: Info{Name: "podman", Rootless: true}}
	if got := m.LogTail(ctx, Container{ID: "c1", Name: "gke-node"}, 30); got != "level=fatal msg=boom" {
		t.Errorf("LogTail = %q", got)
	}
	fail = true
	for _, tt := range []struct {
		name string
		info Info
		want []string
		not  []string
	}{
		{"rootless podman", Info{Name: "podman", Rootless: true}, []string{"logs unavailable", "unable to open a handle", "`podman logs` is unavailable with the journald log driver", "journalctl --user CONTAINER_NAME=gke-node", "podman machine ssh"}, []string{"sudo"}},
		{"rootful podman", Info{Name: "podman"}, []string{"sudo journalctl CONTAINER_NAME=gke-node"}, []string{"--user"}},
		{"docker", Info{Name: "docker"}, []string{"logs unavailable", "unable to open a handle"}, []string{"journalctl"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			m := &Manager{Client: cl, Info: tt.info}
			got := m.LogTail(ctx, Container{ID: "c1", Name: "gke-node"}, 30)
			for _, w := range tt.want {
				if !strings.Contains(got, w) {
					t.Errorf("LogTail = %q; want it to contain %q", got, w)
				}
			}
			for _, n := range tt.not {
				if strings.Contains(got, n) {
					t.Errorf("LogTail = %q; want no %q", got, n)
				}
			}
		})
	}
}
