package edgeagent

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linuxuser586/gcpemu/internal/runtime"
)

// closeRelay closes a relay holding container id against a runtime that
// answers DELETE /containers/{id} with status and returns the log.
func closeRelay(t *testing.T, rt func(context.Context) (*runtime.Manager, error)) string {
	t.Helper()
	var buf bytes.Buffer
	r := &LANRelay{Runtime: rt, Name: "gfe-lan", Log: slog.New(slog.NewTextHandler(&buf, nil))}
	r.created, r.id, r.pushed = true, "abc123", "{}"
	r.Close(context.Background())
	if r.created || r.id != "" || r.pushed != "" {
		t.Errorf("after Close: created=%v id=%q pushed=%q; want reset", r.created, r.id, r.pushed)
	}
	return buf.String()
}

func fakeRuntime(t *testing.T, status int) func(context.Context) (*runtime.Manager, error) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodDelete && strings.HasSuffix(req.URL.Path, "/containers/abc123") {
			w.WriteHeader(status)
			return
		}
		http.NotFound(w, req)
	}))
	t.Cleanup(srv.Close)
	cl, err := runtime.New("tcp://" + strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	return func(context.Context) (*runtime.Manager, error) { return &runtime.Manager{Client: cl}, nil }
}

func TestCloseLogsRemoved(t *testing.T) {
	logs := closeRelay(t, fakeRuntime(t, http.StatusNoContent))
	if !strings.Contains(logs, "gfe-lan relay container removed") {
		t.Errorf("log = %q; want relay container removed", logs)
	}
	if strings.Contains(logs, "Cleanup") {
		t.Errorf("log = %q; want no left-for-Cleanup line", logs)
	}
}

func TestCloseRemoveFailsLeavesForCleanup(t *testing.T) {
	logs := closeRelay(t, fakeRuntime(t, http.StatusInternalServerError))
	assertLeftForCleanup(t, logs)
}

func TestCloseRuntimeFailsLeavesForCleanup(t *testing.T) {
	logs := closeRelay(t, func(context.Context) (*runtime.Manager, error) {
		return nil, errors.New("no runtime here")
	})
	assertLeftForCleanup(t, logs)
	if !strings.Contains(logs, "no runtime here") {
		t.Errorf("log = %q; want the runtime's error", logs)
	}
}

func assertLeftForCleanup(t *testing.T, logs string) {
	t.Helper()
	if strings.Contains(logs, "relay container removed") {
		t.Errorf("log = %q; want no relay container removed", logs)
	}
	if !strings.Contains(logs, "level=INFO") || !strings.Contains(logs, "gfe-lan relay container left for Cleanup") || !strings.Contains(logs, "err=") {
		t.Errorf("log = %q; want Info left for Cleanup with err", logs)
	}
}
