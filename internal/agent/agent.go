// Package agent runs gcpemu helper processes inside containers the emulator
// launches (GKE node metadata proxy, Cloud SQL server-side proxy). The
// emulator copies its own static binary into the container and starts it
// with GCPEMU_AGENT=<name>; Main dispatches to the registered agent.
package agent

import (
	"context"
	"debug/elf"
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
// CGO_ENABLED=0 build of ./cmd/gcpemu).
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
		fmt.Fprintf(os.Stderr, "gcpemu agent: unknown agent %q (known: %v)\n", name, known)
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

var (
	binOnce sync.Once
	binPath string
	binErr  error
)

// Binary returns the path of a static linux binary of gcpemu for the host
// architecture, suitable for copying into containers.
func Binary() (string, error) {
	binOnce.Do(func() {
		if p := os.Getenv(BinaryEnv); p != "" {
			binPath = p
			return
		}
		if runtime.GOOS != "linux" {
			binErr = fmt.Errorf("in-container agents need a linux gcpemu binary; set %s", BinaryEnv)
			return
		}
		binPath, binErr = os.Executable()
		if binErr == nil && dynamic(binPath) {
			binErr = fmt.Errorf("%s is dynamically linked, so it can't run as an agent in containers; build gcpemu with CGO_ENABLED=0 (as `make build` does) or set %s to a static build", binPath, BinaryEnv)
		}
	})
	return binPath, binErr
}

// dynamic reports whether the ELF executable at path needs a dynamic
// loader (it has a PT_INTERP program header).
func dynamic(path string) bool {
	f, err := elf.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	for _, p := range f.Progs {
		if p.Type == elf.PT_INTERP {
			return true
		}
	}
	return false
}

// BuildForTests compiles ./cmd/gcpemu with CGO_ENABLED=0 into dir (once per
// process) and sets GCPEMU_AGENT_BINARY. moduleRoot is the repository root.
func BuildForTests(moduleRoot, dir string) (string, error) {
	out := filepath.Join(dir, "gcpemu-agent")
	cmd := exec.Command("go", "build", "-o", out, "./cmd/gcpemu")
	cmd.Dir = moduleRoot
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux")
	if b, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("build agent binary: %v\n%s", err, b)
	}
	os.Setenv(BinaryEnv, out)
	return out, nil
}
