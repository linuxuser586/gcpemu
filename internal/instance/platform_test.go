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
)

// On native Windows an explicit Runtime Service or host mode is refused,
// whatever $DOCKER_HOST says; elsewhere nothing is (ADR 0008).
func TestRefuseRuntime(t *testing.T) {
	for _, goos := range []string{"linux", "darwin", "windows"} {
		unusable := runtime.UnusableOn(goos)
		for _, tc := range []struct {
			services []string
			hostMode bool
			named    string // in the error on Windows; "" for none
		}{
			{services: nil},
			{services: []string{"gcs", "pubsub"}},
			{services: []string{"sql"}, named: "services sql "},
			{services: []string{"gcs", "gke", "nat", "gke"}, named: "services gke,nat "},
			{services: []string{"build"}, named: "services build "},
			{hostMode: true, named: "--host-mode"},
		} {
			cfg := &config.Config{Services: tc.services, HostMode: tc.hostMode}
			err := refuseRuntime(cfg, unusable)
			if goos != "windows" || tc.named == "" {
				if err != nil {
					t.Errorf("%s %v hostMode=%v: %v", goos, tc.services, tc.hostMode, err)
				}
				continue
			}
			if !errors.Is(err, runtime.ErrNeedsWSL2) || !strings.Contains(err.Error(), tc.named) || !strings.Contains(err.Error(), "WSL2") {
				t.Errorf("%s %v hostMode=%v: %v, want it to name %q and WSL2", goos, tc.services, tc.hostMode, err, tc.named)
			}
		}
	}
}

// Host mode is Linux-only: macOS refuses it at start, saying so, rather
// than failing later in the netplane or the DNS wait (#175).
func TestRefuseHostMode(t *testing.T) {
	for _, goos := range []string{"linux", "darwin", "windows"} {
		if err := refuseHostMode(&config.Config{}, goos); err != nil {
			t.Errorf("%s without host mode: %v", goos, err)
		}
		err := refuseHostMode(&config.Config{HostMode: true}, goos)
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

// On darwin, New refuses host mode before it touches the instance
// directory.
func TestNewRefusesHostModeOffLinux(t *testing.T) {
	if hostmode.UnsupportedOn(goruntime.GOOS) == nil || runtime.Unusable() != nil {
		t.Skip("host mode is supported, or the runtime refuses it first")
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
		goos     string
		detect   error
		want     []string
		why      error
		detected bool
	}{
		{goos: "linux", want: all, detected: true},
		{goos: "darwin", want: all, detected: true},
		{goos: "linux", detect: noRuntime, want: inProcess, why: noRuntime, detected: true},
		// Windows decides by OS, so a reachable runtime is not consulted.
		{goos: "windows", want: inProcess, why: runtime.ErrNeedsWSL2},
	} {
		detected := false
		got, skipped, why := defaultServices(runtime.UnusableOn(tc.goos), func() error { detected = true; return tc.detect })
		if !slices.Equal(got, tc.want) || why != tc.why || detected != tc.detected {
			t.Errorf("%s detect=%v: %v %v detected=%v", tc.goos, tc.detect, got, why, detected)
		}
		if tc.why != nil && !slices.Equal(skipped, []string{"sql", "gke", "nat", "build"}) {
			t.Errorf("%s skipped %v", tc.goos, skipped)
		}
	}
}

// On an OS whose runtime is unusable (native Windows), New refuses a
// Runtime Service before it touches the instance directory, and a default
// list starts without them, saying why.
func TestNewWithoutUsableRuntime(t *testing.T) {
	if runtime.Unusable() == nil {
		t.Skip("the container runtime is usable on this OS")
	}
	cfg := config.Defaults()
	cfg.Ephemeral = true
	cfg.DataDir = t.TempDir()
	cfg.Services = []string{"gcs", "nat"}
	if _, err := New(&cfg, nil, io.Discard); !errors.Is(err, runtime.ErrNeedsWSL2) || !strings.Contains(err.Error(), "nat") {
		t.Fatalf("explicit nat: %v", err)
	}

	cfg.Services = nil
	cfg.LogFormat = "text"
	var out strings.Builder
	in, err := New(&cfg, map[string]Factory{"dns": func(*emu.Env) emu.Service { return &fakeSvc{} }}, &out)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Shutdown(context.Background())
	if !strings.Contains(out.String(), "skipped=sql,gke,nat") || !strings.Contains(out.String(), "WSL2") {
		t.Fatalf("log: %s", out.String())
	}
}
