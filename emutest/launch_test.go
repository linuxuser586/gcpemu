package emutest

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linuxuser586/gcpemu/internal/instance"
	"github.com/linuxuser586/gcpemu/internal/runtime"
)

// TestLaunchFailureCleansUp: an Instance that fails to launch reports the
// error, calls release once, and removes the Test data dir it created
// (#863).
func TestLaunchFailureCleansUp(t *testing.T) {
	cfg := testConfig([]string{"no-such-service"}, nil)
	released := 0
	i, err := launch(context.Background(), &cfg, func() { released++ })
	if err == nil {
		i.Close(context.Background())
		t.Fatal("launch with an unknown service succeeded")
	}
	if released != 1 {
		t.Errorf("release called %d times, want 1", released)
	}
	if cfg.DataDir == "" {
		t.Fatal("launch created no data dir")
	}
	if _, err := os.Stat(cfg.DataDir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("data dir %s left behind: %v", cfg.DataDir, err)
	}
}

// TestNewReportsLaunchError: New returns a failed launch's error, prefixed
// "emutest: ", and a nil Instance rather than panicking (#863, #820).
func TestNewReportsLaunchError(t *testing.T) {
	i, err := New(context.Background(), []string{"no-such-service"})
	if err == nil {
		i.Close(context.Background())
		t.Fatal("New with an unknown service succeeded")
	}
	if i != nil {
		t.Errorf("New returned an Instance with error %v, want nil", err)
	}
	if !strings.HasPrefix(err.Error(), "emutest: ") {
		t.Errorf("error %q lacks the emutest: prefix", err)
	}
}

// TestNewFailureRemovesItsTestDataDir: a failed New removes the Test data
// dir it created (#820).
func TestNewFailureRemovesItsTestDataDir(t *testing.T) {
	// No sweep: it would contact a runtime, and with the temp dir
	// redirected it would skip real Abandoned Instances for the process.
	saved := processSweep
	processSweep = &sweeper{detect: func() (*runtime.Client, error) {
		return nil, errors.New("no runtime")
	}}
	t.Cleanup(func() { processSweep = saved })
	tmp := t.TempDir()
	// os.TempDir reads TMPDIR on Unix, TMP then TEMP on Windows.
	for _, v := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(v, tmp)
	}
	if i, err := New(context.Background(), []string{"no-such-service"}); err == nil {
		i.Close(context.Background())
		t.Fatal("New with an unknown service succeeded")
	}
	leftover, err := os.ReadDir(tmp)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range leftover {
		if strings.HasPrefix(e.Name(), "gcpemu-test-") {
			t.Errorf("data dir %s left behind", e.Name())
		}
	}
}

// TestLaunchFailureKeepsGivenDataDir: a failed launch in a data dir the
// caller gave leaves that dir and its files in place (#820).
func TestLaunchFailureKeepsGivenDataDir(t *testing.T) {
	dir := t.TempDir()
	keep := filepath.Join(dir, "keep")
	if err := os.WriteFile(keep, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := testConfig([]string{"no-such-service"}, []Option{WithDataDir(dir)})
	released := 0
	i, err := launch(context.Background(), &cfg, func() { released++ })
	if err == nil {
		i.Close(context.Background())
		t.Fatal("launch with an unknown service succeeded")
	}
	if released != 1 {
		t.Errorf("release called %d times, want 1", released)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("given data dir not kept: %v", err)
	}
}

// TestLaunchFailureKeepsTestDataDirWhenResourcesRemain: a launch that fails after its
// Instance was created, and whose containers, networks or volumes cannot
// be removed, keeps its Test data dir, owner lock released, for the next
// sweep, as Close does (ADR 0047, #877).
func TestLaunchFailureKeepsTestDataDirWhenResourcesRemain(t *testing.T) {
	savedShutdown, savedRemove := shutdownInstance, removeInstanceResources
	shutdownInstance = func(ctx context.Context, in *instance.Instance) error {
		return errors.Join(in.Shutdown(ctx), errors.New("container left"))
	}
	removeInstanceResources = func(string) error { return errors.New("no runtime") }
	t.Cleanup(func() { shutdownInstance, removeInstanceResources = savedShutdown, savedRemove })
	cfg := testConfig([]string{"gcs"}, []Option{WithSeed(filepath.Join(t.TempDir(), "missing.yaml"))})
	released := 0
	i, err := launch(context.Background(), &cfg, func() { released++ })
	if err == nil {
		i.Close(context.Background())
		t.Fatal("launch with a missing seed file succeeded")
	}
	t.Cleanup(func() { _ = os.RemoveAll(cfg.DataDir) })
	if released != 1 {
		t.Errorf("release called %d times, want 1", released)
	}
	if _, err := os.Stat(cfg.DataDir); err != nil {
		t.Fatalf("data dir not kept: %v", err)
	}
	unlock, err := lockNow(filepath.Join(cfg.DataDir, ownerLockFile), 0)
	if err != nil {
		t.Fatalf("owner lock of the kept dir still held: %v", err)
	}
	unlock()
}
