package cli

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/linuxuser586/gcpemu/internal/config"
)

// TestDetachFailureReasons is FR-CI-003: `start --detach` reports why the
// emulator did not start on stderr, not only in the log file.
func TestDetachFailureReasons(t *testing.T) {
	fake := func(t *testing.T, script string) {
		t.Helper()
		exe := filepath.Join(t.TempDir(), "fake-gcpemu")
		if err := os.WriteFile(exe, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
			t.Fatal(err)
		}
		old := detachExe
		detachExe = func() (string, error) { return exe, nil }
		t.Cleanup(func() { detachExe = old })
	}
	cfgFor := func(t *testing.T) *config.Config {
		c := config.Defaults()
		c.DataDir = t.TempDir()
		c.WaitTimeout = 300 * time.Millisecond
		return &c
	}
	cmd := &cobra.Command{}
	cmd.SetOut(io.Discard)

	t.Run("exit", func(t *testing.T) {
		cfg := cfgFor(t)
		_ = os.WriteFile(filepath.Join(cfg.DataDir, "gcpemu.log"), []byte("an older run\n"), 0o600)
		fake(t, "echo 'level=ERROR msg=\"not ready\" err=\"gke: no container runtime\"'\necho 'Error: gke: no container runtime'\nexit 1\n")
		err := startDetached(cmd, cfg)
		if err == nil || !strings.Contains(err.Error(), "Error: gke: no container runtime") || strings.Contains(err.Error(), "an older run") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("timeout", func(t *testing.T) {
		cfg := cfgFor(t)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"ready":false,"services":{"gcs":{"ready":true},"sql":{"ready":false,"reason":"pulling postgres:17"}}}`)
		}))
		defer srv.Close()
		fake(t, "echo '{\"gateway\":\""+strings.TrimPrefix(srv.URL, "http://")+"\"}' > '"+filepath.Join(cfg.DataDir, "endpoints.json")+"'\nexec sleep 30\n")
		err := startDetached(cmd, cfg)
		if err == nil || !strings.Contains(err.Error(), "sql: pulling postgres:17") || strings.Contains(err.Error(), "gcs") {
			t.Fatalf("err = %v", err)
		}
	})
}
