package emutest

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linuxuser586/gcpemu/internal/agent"
	"github.com/linuxuser586/gcpemu/internal/instance"
	"github.com/linuxuser586/gcpemu/internal/runtime"
	"github.com/linuxuser586/gcpemu/internal/vmrelay"
)

var (
	agentOnce sync.Once
	agentErr  error
)

// RequireRuntime skips the test unless a container runtime is available
// (and GCPEMU_SKIP_CONTAINER_TESTS is unset), and makes a static gcpemu
// binary available for in-container agents.
func RequireRuntime(t testing.TB) {
	t.Helper()
	if os.Getenv("GCPEMU_SKIP_CONTAINER_TESTS") != "" {
		t.Skip("GCPEMU_SKIP_CONTAINER_TESTS is set")
	}
	if testing.Short() {
		t.Skip("container test skipped in -short mode")
	}
	cl, err := runtime.Detect()
	if err != nil {
		t.Skipf("no container runtime: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	info, err := cl.Info(ctx)
	if err != nil {
		t.Skipf("container runtime unavailable: %v", err)
	}
	agentOnce.Do(func() {
		// The VM relay's image, for Instances that start one offline.
		pctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if err := cl.EnsureImage(pctx, vmrelay.Image, false); err != nil {
			agentErr = err
			return
		}
		if os.Getenv(agent.BinaryEnv) != "" {
			return
		}
		out, err := exec.Command("go", "env", "GOMOD").Output()
		if err != nil {
			agentErr = err
			return
		}
		root := filepath.Dir(strings.TrimSpace(string(out)))
		dir := filepath.Join(os.TempDir(), "gcpemu-test-agent")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			agentErr = err
			return
		}
		_, agentErr = agent.BuildForTests(root, dir, info.Arch)
	})
	if agentErr != nil {
		t.Fatalf("emutest: %v", agentErr)
	}
}

// cleanupRegistered holds the tests and Test data dirs whose Instance's
// resources are removed when the test ends (WithTestDataDir), so that each
// pair registers one cleanup.
var cleanupRegistered sync.Map

type testDir struct {
	t   testing.TB
	dir string
}

// removeAtEnd removes the containers, networks and volumes of the
// Instance in dir when t ends. It is called before the Instance starts, so
// its cleanup runs after the Instance's Close.
func removeAtEnd(t testing.TB, dir string) {
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	key := testDir{t, dir}
	if _, loaded := cleanupRegistered.LoadOrStore(key, true); loaded {
		return
	}
	t.Cleanup(func() {
		cleanupRegistered.Delete(key)
		if err := removeResources(dir); err != nil {
			t.Errorf("emutest: %v", err)
		}
	})
}

// removeResources removes the containers, networks and volumes of the
// Instance whose ID dir holds. Without an ID the Instance did not start,
// and without a runtime it could not create any. A removal the runtime
// still has in progress from the Instance's own stop is tried again.
func removeResources(dir string) error {
	id, err := os.ReadFile(filepath.Join(dir, instance.IDFile))
	if errors.Is(err, fs.ErrNotExist) || err == nil && len(id) == 0 {
		return nil
	}
	if err != nil {
		return err
	}
	cl, err := runtime.Detect()
	if err != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	m := &runtime.Manager{Client: cl, InstanceID: string(id)}
	for attempt := 1; ; attempt++ {
		err = m.Cleanup(ctx, true)
		if err == nil || attempt == 3 {
			break
		}
		select {
		case <-ctx.Done():
		case <-time.After(2 * time.Second):
		}
	}
	if err != nil {
		return fmt.Errorf("remove the containers, networks and volumes of Instance %s: %w", id, err)
	}
	return nil
}
