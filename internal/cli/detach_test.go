package cli

import (
	"errors"
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
	"github.com/linuxuser586/gcpemu/internal/instance"
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

// fakeGcpemu is the detached child. "exit" fails at startup. "stopped"
// exits cleanly, as one stopped during startup does. "stopped-seeding" runs
// a real `gcpemu start`, stopped during Seeding. "starting"
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
	case "stopped-seeding": // a real `gcpemu start`, stopped during Seeding
		if err := New(shutDownWhileSeeding()).Execute(); err != nil {
			os.Exit(1)
		}
	case "sleep": // an Instance's process, until it is killed
		time.Sleep(time.Minute)
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

	// #1000: the child exits cleanly when stopped during startup. `start
	// --detach` still fails, as no Instance is ready, but not as a crash.
	t.Run("stopped", func(t *testing.T) {
		cfg := cfgFor(t)
		fake(t, "stopped", cfg.DataDir)
		err := startDetached(cmd, cfg)
		if !errors.Is(err, instance.ErrShutDown) || strings.Contains(err.Error(), "<nil>") {
			t.Errorf("err: %v, want ErrShutDown", err)
		}
	})

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
			if r.URL.Path == "/_emu/v1/info" {
				_, _ = io.WriteString(w, `{}`)
				return
			}
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

	// #1024: ready means this data dir's Live Instance is ready.
	t.Run("ready", func(t *testing.T) {
		cfg := cfgFor(t)
		writeDataFile(t, cfg.DataDir, instance.IDFile, "mine")
		t.Setenv("GCPEMU_TEST_GATEWAY", instanceAPIGateway(t, "mine", ""))
		fake(t, "starting", cfg.DataDir)
		var out strings.Builder
		cmd := &cobra.Command{}
		cmd.SetOut(&out)
		if err := startDetached(cmd, cfg); err != nil || !strings.Contains(out.String(), "ready") {
			t.Errorf("start --detach = %q, %v", out.String(), err)
		}
	})
	t.Run("another Instance is not ready", func(t *testing.T) {
		cfg := cfgFor(t)
		writeDataFile(t, cfg.DataDir, instance.IDFile, "mine")
		t.Setenv("GCPEMU_TEST_GATEWAY", instanceAPIGateway(t, "theirs", ""))
		fake(t, "starting", cfg.DataDir)
		err := startDetached(cmd, cfg)
		if err == nil || !strings.Contains(err.Error(), "not ready") {
			t.Errorf("err = %v, want not ready", err)
		}
	})
}
