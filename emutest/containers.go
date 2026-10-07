package emutest

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linuxuser586/gcpemu/internal/agent"
	"github.com/linuxuser586/gcpemu/internal/runtime"
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
	if _, err := cl.Info(ctx); err != nil {
		t.Skipf("container runtime unavailable: %v", err)
	}
	agentOnce.Do(func() {
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
		_, agentErr = agent.BuildForTests(root, dir)
	})
	if agentErr != nil {
		t.Fatalf("emutest: %v", agentErr)
	}
}
