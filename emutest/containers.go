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

// RelayStreams returns the VM relay's per-target stream counts
// (vmrelay.Relay.Streams), or nil when the Instance runs no relay. Tests
// use it to check that the relay carried a path.
func RelayStreams(t testing.TB, inst *Instance) map[string]int64 {
	t.Helper()
	p, err := inst.Env.Containers.Netplane(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	r := p.Relay()
	if r == nil {
		return nil
	}
	return r.Streams()
}

// RequireRelayStream fails the test when the Instance runs a VM relay and
// no stream it carried matches: a target equal to key, or ending in key
// when key starts with ":" (a port).
func RequireRelayStream(t testing.TB, inst *Instance, what, key string) {
	t.Helper()
	st := RelayStreams(t, inst)
	if st == nil {
		return
	}
	for k, n := range st {
		if n > 0 && (k == key || strings.HasPrefix(key, ":") && strings.HasSuffix(k, key)) {
			return
		}
	}
	t.Errorf("the VM relay carried no stream for %s (%s): %v", what, key, st)
}
