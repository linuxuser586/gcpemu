package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/instance"
	"github.com/linuxuser586/gcpemu/services"
)

// notReadySvc is a gcs that never becomes ready.
type notReadySvc struct{ stopped *atomic.Bool }

func (notReadySvc) Name() string                 { return "gcs" }
func (notReadySvc) Register(emu.Router) error    { return nil }
func (notReadySvc) Start(context.Context) error  { return nil }
func (s notReadySvc) Stop(context.Context) error { s.stopped.Store(true); return nil }
func (notReadySvc) Ready() error                 { return errors.New("never ready") }

// gcpemuRun runs `gcpemu run args...` in this process with factories and
// returns its stderr. The command is the test binary as fakeGcpemu, in
// mode, which records what it saw in the returned marker file.
func gcpemuRun(t *testing.T, factories map[string]instance.Factory, mode string, args ...string) (stderr, marker string, err error) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	marker = filepath.Join(t.TempDir(), "marker")
	t.Setenv("GCPEMU_TEST_FAKE", mode)
	t.Setenv("GCPEMU_TEST_MARKER", marker)
	cmd := New(factories)
	var errOut bytes.Buffer
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&errOut)
	cmd.SetArgs(append(append([]string{"run", "--data-dir", t.TempDir(), "--config", os.DevNull, "--port", "0"}, args...), "--", exe))
	err = cmd.Execute()
	return errOut.String(), marker, err
}

// ran reports whether the command `gcpemu run` was given ever ran.
func ran(marker string) bool {
	_, err := os.Stat(marker)
	return err == nil
}

// TestRunUsage is FR-CI-008: without a command after --, `run` fails
// before it starts anything.
func TestRunUsage(t *testing.T) {
	var started atomic.Int32
	factories := map[string]instance.Factory{"gcs": func(*emu.Env) emu.Service {
		started.Add(1)
		return notReadySvc{stopped: &atomic.Bool{}}
	}}
	for _, args := range [][]string{
		{"run", "true"},
		{"run", "--"},
		{"run", "true", "--", "true"},
		{"run", "--", filepath.Join(t.TempDir(), "no-such-command")},
	} {
		cmd := New(factories)
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs(append(args[:1:1], append([]string{"--data-dir", t.TempDir(), "--config", os.DevNull}, args[1:]...)...))
		if err := cmd.Execute(); err == nil {
			t.Errorf("gcpemu %s: no error", strings.Join(args, " "))
		}
	}
	if n := started.Load(); n != 0 {
		t.Errorf("%d Instances started", n)
	}
}

// TestRunNeverStartsCommand is FR-CI-008: when the Instance can't start,
// or isn't ready within --wait-timeout, the command never runs and the
// Instance is torn down.
func TestRunNeverStartsCommand(t *testing.T) {
	stopped := &atomic.Bool{}
	factories := map[string]instance.Factory{"gcs": func(*emu.Env) emu.Service { return notReadySvc{stopped: stopped} }}

	_, marker, err := gcpemuRun(t, factories, "touch", "--iam-mode", "bogus")
	if err == nil || ran(marker) {
		t.Errorf("--iam-mode bogus: err %v, command ran %v", err, ran(marker))
	}

	_, marker, err = gcpemuRun(t, factories, "touch", "--services", "gcs", "--wait-timeout", "200ms")
	var exit *ExitError
	if err == nil || errors.As(err, &exit) || !strings.Contains(err.Error(), "never ready") || ran(marker) {
		t.Errorf("not ready: err %v, command ran %v", err, ran(marker))
	}
	if !stopped.Load() {
		t.Error("the Instance that wasn't ready was not torn down")
	}
}

// TestRunEnv is FR-CI-008: the command gets the caller's environment plus
// what `gcpemu env` prints, which wins, and --trust and --no-universe
// change it as they change `gcpemu env`.
func TestRunEnv(t *testing.T) {
	t.Setenv("GCPEMU_TEST_CALLER", "kept")
	t.Setenv("CLOUDSDK_CORE_UNIVERSE_DOMAIN", "googleapis.com")
	childEnv := func(args ...string) map[string]string {
		t.Helper()
		stderr, marker, err := gcpemuRun(t, services.Factories(), "env", append([]string{"--services", "gcs"}, args...)...)
		if err != nil {
			t.Fatalf("run %v: %v\n%s", args, err, stderr)
		}
		b, err := os.ReadFile(marker)
		if err != nil {
			t.Fatal(err)
		}
		env := map[string]string{}
		for _, kv := range strings.Split(string(b), "\n") {
			k, v, _ := strings.Cut(kv, "=")
			env[strings.ToUpper(k)] = v // Windows names are case-insensitive
		}
		return env
	}

	env := childEnv()
	if env["GCPEMU_TEST_CALLER"] != "kept" || !strings.HasPrefix(env["STORAGE_EMULATOR_HOST"], "127.0.0.1:") ||
		env["CLOUDSDK_CORE_UNIVERSE_DOMAIN"] != "gcpemu.invalid" || env["GCPEMU_CA_FILE"] == "" || env["SSL_CERT_FILE"] != os.Getenv("SSL_CERT_FILE") {
		t.Errorf("default environment: %v", env)
	}
	env = childEnv("--no-universe")
	if env["CLOUDSDK_CORE_UNIVERSE_DOMAIN"] != "googleapis.com" || env["GOOGLE_CLOUD_UNIVERSE_DOMAIN"] != "gcpemu.invalid" {
		t.Errorf("--no-universe: CLOUDSDK_CORE_UNIVERSE_DOMAIN=%q GOOGLE_CLOUD_UNIVERSE_DOMAIN=%q", env["CLOUDSDK_CORE_UNIVERSE_DOMAIN"], env["GOOGLE_CLOUD_UNIVERSE_DOMAIN"])
	}
	env = childEnv("--trust")
	if p := env["SSL_CERT_FILE"]; p == "" || p == os.Getenv("SSL_CERT_FILE") {
		t.Errorf("--trust: SSL_CERT_FILE=%q", p)
	}
}
