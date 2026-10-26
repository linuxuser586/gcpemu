package gke

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linuxuser586/gcpemu/internal/runtime"
)

// TestExitedLogsUnavailable (#633): when a node or control-plane
// container exits and the runtime cannot read its logs, as Podman cannot
// with the journald log driver, the error says where they are, under the
// container's name, not only the API error.
func TestExitedLogsUnavailable(t *testing.T) {
	docker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/json"):
			_ = json.NewEncoder(w).Encode(map[string]any{"Name": "/gcpemu-i1-gke-node-1",
				"State": map[string]any{"Status": "exited", "ExitCode": 1}})
		case strings.HasSuffix(r.URL.Path, "/logs"):
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"message":"failed to obtain logs for Container 'x': unable to open a handle to the library"}`)
		default:
			t.Errorf("unexpected runtime request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(docker.Close)
	cl, err := runtime.New("tcp://" + strings.TrimPrefix(docker.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	rt := &runtime.Manager{Client: cl, InstanceID: "i1", Info: runtime.Info{Name: "podman", Rootless: true}}
	ctx := context.Background()
	for _, err := range []error{
		nodeExited(ctx, rt, "abc123", "gke-c1-default-pool-1"),
		serverExited(ctx, rt, "abc123"),
	} {
		if err == nil {
			t.Fatal("exited container reported as not exited")
		}
		for _, want := range []string{"exited with code 1", "unable to open a handle to the library", "journalctl --user CONTAINER_NAME=gcpemu-i1-gke-node-1", "podman machine ssh"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q; want it to contain %q", err, want)
			}
		}
	}
}
