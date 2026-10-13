// Package agent runs gcpemu helper processes inside containers the emulator
// launches (GKE node metadata proxy, Cloud SQL server-side proxy). The
// emulator copies its own static binary into the container and starts it
// with GCPEMU_AGENT=<name>; Main dispatches to the registered agent.
package agent

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"syscall"
)

// EnvVar selects the agent to run.
const EnvVar = "GCPEMU_AGENT"

// BinaryEnv overrides the binary copied into containers (tests set it to a
// CGO_ENABLED=0 build of ./cmd/gcpemu). See Resolver.
const BinaryEnv = "GCPEMU_AGENT_BINARY"

// ContainerPath is where the binary is placed inside containers.
const ContainerPath = "/gcpemu/gcpemu"

// Func is an agent entry point. It runs until ctx is cancelled (SIGTERM).
type Func func(ctx context.Context, args []string) error

var (
	mu     sync.Mutex
	agents = map[string]Func{}
)

// Register installs an agent; call it from an init function.
func Register(name string, f Func) {
	mu.Lock()
	defer mu.Unlock()
	if _, dup := agents[name]; dup {
		panic("agent: duplicate " + name)
	}
	agents[name] = f
}

// Main runs the selected agent and exits if GCPEMU_AGENT is set; otherwise
// it returns immediately. Call it first thing in main (and in TestMain of
// packages whose tests start agents in-process).
func Main() {
	name := os.Getenv(EnvVar)
	if name == "" {
		return
	}
	mu.Lock()
	f, ok := agents[name]
	known := make([]string, 0, len(agents))
	for k := range agents {
		known = append(known, k)
	}
	mu.Unlock()
	if !ok {
		sort.Strings(known)
		fmt.Fprintf(os.Stderr, "gcpemu agent: unknown Sidecar agent %q (known: %v)\n", name, known)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	if err := f(ctx, os.Args[1:]); err != nil && ctx.Err() == nil {
		fmt.Fprintf(os.Stderr, "gcpemu agent %s: %v\n", name, err)
		os.Exit(1)
	}
	os.Exit(0)
}

// BuildForTests compiles ./cmd/gcpemu for linux/arch with CGO_ENABLED=0
// into dir (once per process) and sets GCPEMU_AGENT_BINARY. moduleRoot is
// the repository root; arch is the runtime's, or "" for the host's.
func BuildForTests(moduleRoot, dir, arch string) (string, error) {
	if arch == "" {
		arch = runtime.GOARCH
	}
	out := filepath.Join(dir, "gcpemu-agent-"+arch)
	cmd := exec.Command("go", "build", "-o", out, "./cmd/gcpemu")
	cmd.Dir = moduleRoot
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+arch)
	if b, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("build Sidecar agent binary: %v\n%s", err, b)
	}
	os.Setenv(BinaryEnv, out)
	return out, nil
}
