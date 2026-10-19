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
)

// Host mode is Linux-only: macOS and Windows refuse it at start, saying so, rather
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
	cfg.LogFormat = "text"
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
