package cli

import (
	"fmt"
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

// TestMain lets the test binary stand in for the gcpemu that `start
// --detach` runs: with GCPEMU_TEST_FAKE set it acts as the fake instead of
// running the tests. A Go binary runs on every OS, unlike a #!/bin/sh fake.
func TestMain(m *testing.M) {
	if mode := os.Getenv("GCPEMU_TEST_FAKE"); mode != "" {
		fakeGcpemu(mode)
		return
	}
	os.Exit(m.Run())
}

// fakeGcpemu is the detached child. "exit" fails at startup. "starting"
// publishes the gateway in GCPEMU_TEST_GATEWAY and never becomes ready.
// It lives until it is signalled or, on Windows (no signals), the test
// closes that gateway, so it no longer holds the log open when the test's
// temp dir is removed.
func fakeGcpemu(mode string) {
	switch mode {
	case "exit":
		fmt.Println(`level=ERROR msg="not ready" err="gke: no container runtime"`)
		fmt.Println("Error: gke: no container runtime")
		os.Exit(1)
	case "starting":
		gw := os.Getenv("GCPEMU_TEST_GATEWAY")
		if err := os.WriteFile(filepath.Join(os.Getenv("GCPEMU_TEST_DIR"), "endpoints.json"), []byte(`{"gateway":"`+gw+`"}`), 0o600); err != nil {
			os.Exit(2)
		}
		for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
			resp, err := http.Get("http://" + gw + "/")
			if err != nil {
				break
			}
			resp.Body.Close()
		}
	case "touch": // a command `gcpemu run` runs
		_ = os.WriteFile(os.Getenv("GCPEMU_TEST_MARKER"), nil, 0o600)
	case "env": // a command `gcpemu run` runs, recording its environment
		_ = os.WriteFile(os.Getenv("GCPEMU_TEST_MARKER"), []byte(strings.Join(os.Environ(), "\n")), 0o600)
	}
	os.Exit(0)
}

// TestDetachFailureReasons is FR-CI-003: `start --detach` reports why the
// emulator did not start on stderr, not only in the log file.
func TestDetachFailureReasons(t *testing.T) {
	fake := func(t *testing.T, mode, dir string) {
		t.Helper()
		exe, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		t.Setenv("GCPEMU_TEST_FAKE", mode)
		t.Setenv("GCPEMU_TEST_DIR", dir)
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
		fake(t, "exit", cfg.DataDir)
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
		t.Setenv("GCPEMU_TEST_GATEWAY", strings.TrimPrefix(srv.URL, "http://"))
		fake(t, "starting", cfg.DataDir)
		err := startDetached(cmd, cfg)
		if err == nil || !strings.Contains(err.Error(), "sql: pulling postgres:17") || strings.Contains(err.Error(), "gcs") {
			t.Fatalf("err = %v", err)
		}
	})
}
