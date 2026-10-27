package instance

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"strings"
	"testing"

	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/hostmode"
	"github.com/linuxuser586/gcpemu/internal/runtime"
	"github.com/linuxuser586/gcpemu/internal/vmrelay"
)

// Host mode is Linux-only: macOS and Windows refuse it at start, saying so, rather
// than failing later in the netplane or the DNS wait (#175).
func TestRefuseHostMode(t *testing.T) {
	native := func() (hostModeRuntime, bool) { return hostModeRuntime{}, true }
	for _, goos := range []string{"linux", "darwin", "windows"} {
		if err := refuseHostMode(&config.Config{}, goos, native); err != nil {
			t.Errorf("%s without host mode: %v", goos, err)
		}
		err := refuseHostMode(&config.Config{HostMode: true}, goos, native)
		if goos == "linux" {
			if err != nil {
				t.Errorf("linux: %v", err)
			}
			continue
		}
		if !errors.Is(err, hostmode.ErrLinuxOnly) || !strings.Contains(err.Error(), "--host-mode") || !strings.Contains(err.Error(), "Linux-only") {
			t.Errorf("%s: %v", goos, err)
		}
	}
}

// A Containerized Instance on a VM runtime refuses host mode too: its GOOS
// is linux, but the host cannot route to the container's bridge IP (#492).
// The decision is DecideVM's, GCPEMU_VM_RUNTIME included, and a runtime
// that cannot be reached is no reason to refuse.
func TestRefuseHostModeOnVMRuntime(t *testing.T) {
	desktop := runtime.Info{OS: "Docker Desktop", Kernel: "6.10.14-linuxkit"}
	colima := runtime.Info{OS: "Ubuntu 24.04.1 LTS", Kernel: "6.8.0-50-generic"}
	for _, tc := range []struct {
		name        string
		setting     string
		inContainer bool
		info        runtime.Info
		unreachable bool
		reason      string // "" when host mode is accepted
	}{
		{name: "on", setting: "auto", inContainer: true, info: desktop, reason: "runtime OS Docker Desktop"},
		{name: "native Linux runtime", setting: "auto", inContainer: true, info: colima},
		{name: "native Instance", setting: "true", info: desktop},
		{name: "forced true", setting: "true", inContainer: true, info: colima, reason: "forced by GCPEMU_VM_RUNTIME"},
		{name: "forced false", setting: "false", inContainer: true, info: desktop},
		{name: "unreachable", setting: "true", inContainer: true, unreachable: true},
	} {
		cfg := &config.Config{HostMode: true, VMRuntime: tc.setting}
		vm := func() (hostModeRuntime, bool) {
			if tc.unreachable {
				return hostModeRuntime{}, false
			}
			return hostModeRuntime{VM: runtime.DecideVM(cfg.VMRuntime, tc.inContainer, tc.info)}, true
		}
		err := refuseHostMode(cfg, "linux", vm)
		if tc.reason == "" {
			if err != nil {
				t.Errorf("%s: %v", tc.name, err)
			}
			continue
		}
		if !errors.Is(err, hostmode.ErrVMRuntime) || !strings.HasPrefix(err.Error(), "--host-mode: ") || !strings.Contains(err.Error(), tc.reason) || !strings.Contains(err.Error(), "gcpemu env") {
			t.Errorf("%s: %v", tc.name, err)
		}
	}
}

// An emulator on the host whose VM relay is on because the services
// network's gateway cannot be bound is on a VM runtime too, as Docker Desktop on
// Linux shows: the host cannot route to the host-mode container (#560).
// A relay forced on by GCPEMU_VM_RELAY (Linux CI, where the host routes)
// or off is no reason to refuse.
func TestRefuseHostModeOnVMRelay(t *testing.T) {
	const unbindable, bindable = "192.0.2.1", "127.0.0.1" // TEST-NET-1 is on no host interface
	for _, tc := range []struct {
		name    string
		env     string
		gateway string
		reason  string // "" when host mode is accepted
	}{
		{name: "not bindable", gateway: unbindable, reason: "gateway 192.0.2.1 not bindable"},
		{name: "forced on", env: "1", gateway: unbindable},
		{name: "forced off", env: "0", gateway: unbindable},
		{name: "off", gateway: bindable},
	} {
		t.Setenv(vmrelay.Env, tc.env)
		probe := func() (hostModeRuntime, bool) {
			return hostModeRuntime{Relay: vmrelay.Decide(false, tc.gateway)}, true
		}
		err := refuseHostMode(&config.Config{HostMode: true}, "linux", probe)
		if tc.reason == "" {
			if err != nil {
				t.Errorf("%s: %v", tc.name, err)
			}
			continue
		}
		if !errors.Is(err, hostmode.ErrVMRelay) || !strings.HasPrefix(err.Error(), "--host-mode: ") || !strings.Contains(err.Error(), tc.reason) || !strings.Contains(err.Error(), "gcpemu env") {
			t.Errorf("%s: %v", tc.name, err)
		}
	}
}

// The runtime is consulted only for host mode on Linux: off Linux the GOOS
// refusal comes first, and without --host-mode nothing is asked.
func TestRefuseHostModeAsksRuntimeOnlyWhenNeeded(t *testing.T) {
	for _, tc := range []struct {
		goos     string
		hostMode bool
		want     bool
	}{{"linux", true, true}, {"linux", false, false}, {"darwin", true, false}, {"windows", false, false}} {
		asked := false
		vm := func() (hostModeRuntime, bool) { asked = true; return hostModeRuntime{}, true }
		_ = refuseHostMode(&config.Config{HostMode: tc.hostMode}, tc.goos, vm)
		if asked != tc.want {
			t.Errorf("%s host mode %v: asked %v", tc.goos, tc.hostMode, asked)
		}
	}
}

// On a VM runtime, New refuses host mode before it touches the instance
// directory.
func TestNewRefusesHostModeOnVMRuntime(t *testing.T) {
	if hostmode.UnsupportedOn(goruntime.GOOS) != nil {
		t.Skip("off Linux the GOOS refusal comes first")
	}
	probe := hostModeProbe
	t.Cleanup(func() { hostModeProbe = probe })
	hostModeProbe = func(string) (hostModeRuntime, bool) {
		return hostModeRuntime{VM: runtime.VMDecision{On: true, Reason: "forced by GCPEMU_VM_RUNTIME"}}, true
	}
	cfg := config.Defaults()
	cfg.DataDir = filepath.Join(t.TempDir(), "absent")
	cfg.HostMode = true
	if _, err := New(&cfg, nil, io.Discard); !errors.Is(err, hostmode.ErrVMRuntime) {
		t.Fatalf("New = %v, want ErrVMRuntime", err)
	}
	if _, err := os.Stat(cfg.DataDir); !os.IsNotExist(err) {
		t.Fatalf("data dir created: %v", err)
	}
}

// Off Linux, New refuses host mode before it touches the instance
// directory.
func TestNewRefusesHostModeOffLinux(t *testing.T) {
	if hostmode.UnsupportedOn(goruntime.GOOS) == nil {
		t.Skip("host mode is supported")
	}
	cfg := config.Defaults()
	cfg.DataDir = filepath.Join(t.TempDir(), "absent")
	cfg.HostMode = true
	if _, err := New(&cfg, nil, io.Discard); !errors.Is(err, hostmode.ErrLinuxOnly) {
		t.Fatalf("New = %v, want ErrLinuxOnly", err)
	}
	if _, err := os.Stat(cfg.DataDir); !os.IsNotExist(err) {
		t.Fatalf("data dir created: %v", err)
	}
}

func TestDefaultServices(t *testing.T) {
	all := config.ResolveServices(nil)
	inProcess := slices.DeleteFunc(slices.Clone(all), func(s string) bool { return config.RuntimeServices[s] })
	noRuntime := errors.New("no runtime")
	for _, tc := range []struct {
		detect error
		want   []string
	}{
		{want: all},
		{detect: noRuntime, want: inProcess},
	} {
		got, skipped, why := defaultServices(func() error { return tc.detect })
		if !slices.Equal(got, tc.want) || why != tc.detect {
			t.Errorf("detect=%v: %v %v", tc.detect, got, why)
		}
		if tc.detect != nil && !slices.Equal(skipped, []string{"sql", "gke", "nat", "build"}) {
			t.Errorf("detect=%v: skipped %v", tc.detect, skipped)
		}
	}
}

// Without a usable container runtime, on every OS, a default list starts
// without the Runtime Services, saying why, and an explicit one still
// starts: its control plane needs no runtime (ADR 0015).
func TestNewWithoutUsableRuntime(t *testing.T) {
	t.Setenv("DOCKER_HOST", "tcp://127.0.0.1:1")
	cfg := config.Defaults()
	cfg.Ephemeral = true
	cfg.DataDir = t.TempDir()
	cfg.Services = []string{"gcs", "nat"}
	factories := map[string]Factory{
		"gcs": func(*emu.Env) emu.Service { return &fakeSvc{} },
		"nat": func(*emu.Env) emu.Service { return &fakeSvc{} },
		"dns": func(*emu.Env) emu.Service { return &fakeSvc{} },
	}
	in, err := New(&cfg, factories, io.Discard)
	if err != nil {
		t.Fatalf("explicit nat: %v", err)
	}
	if in.byName["nat"] == nil {
		t.Fatalf("explicit nat not built: %v", in.byName)
	}
	in.Shutdown(context.Background())

	cfg.Services = nil
	var out strings.Builder
	in, err = New(&cfg, factories, &out)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Shutdown(context.Background())
	log := out.String()
	if !strings.Contains(log, "no usable container runtime") || !strings.Contains(log, "skipped=sql,gke,nat,build") || !strings.Contains(log, "127.0.0.1:1") || strings.Contains(log, "WSL2") {
		t.Fatalf("log: %s", log)
	}
	if in.byName["nat"] != nil || in.byName["dns"] == nil {
		t.Fatalf("default list built %v", in.byName)
	}
}
