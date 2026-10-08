package cli

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestStatusReady is NFR-PORT-004: `gcpemu status --ready` is the OCI
// image's HEALTHCHECK, so it fails until every service is ready, and plain
// `status` does not.
func TestStatusReady(t *testing.T) {
	ready := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/_emu/v1/info":
			_, _ = io.WriteString(w, `{"instance":"default"}`)
		case "/_emu/v1/ready":
			if !ready {
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = io.WriteString(w, `{"ready":false,"services":{"gcs":{"ready":false,"reason":"starting"}}}`)
				return
			}
			_, _ = io.WriteString(w, `{"ready":true,"services":{"gcs":{"ready":true}}}`)
		default:
			_, _ = io.WriteString(w, `{"containers":[]}`)
		}
	}))
	defer srv.Close()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "endpoints.json"), []byte(`{"gateway":"`+strings.TrimPrefix(srv.URL, "http://")+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) error {
		root := New(nil)
		root.SetOut(io.Discard)
		root.SetArgs(append([]string{"status", "--config", filepath.Join(dir, "none.yaml"), "--data-dir", dir}, args...))
		return root.Execute()
	}
	if err := run(); err != nil {
		t.Errorf("status while starting: %v", err)
	}
	for _, args := range [][]string{{"--ready"}, {"--ready", "--json"}} {
		if err := run(args...); err == nil || !strings.Contains(err.Error(), "not ready") {
			t.Errorf("status %v while starting: %v", args, err)
		}
	}
	ready = true
	for _, args := range [][]string{{"--ready"}, {"--ready", "--json"}} {
		if err := run(args...); err != nil {
			t.Errorf("status %v when ready: %v", args, err)
		}
	}
	if err := run("--ready", "--data-dir", t.TempDir()); err == nil || !strings.Contains(err.Error(), "not running") {
		t.Errorf("status --ready without an instance: %v", err)
	}
}
