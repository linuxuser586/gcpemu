package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"go.yaml.in/yaml/v3"

	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/instance"
)

// waitedSvc is a gcs that never becomes ready and calls waited, once, when
// something first waits for it.
type waitedSvc struct {
	notReadySvc
	once   sync.Once
	waited func()
}

func (s *waitedSvc) Ready() error {
	s.once.Do(func() { go s.waited() })
	return s.notReadySvc.Ready()
}

// stoppedWhileWaiting is factories whose Instance is asked, as `gcpemu
// stop` asks it, to shut down while it waits to become ready.
func stoppedWhileWaiting(t *testing.T) map[string]instance.Factory {
	t.Helper()
	return map[string]instance.Factory{"gcs": func(env *emu.Env) emu.Service {
		return &waitedSvc{notReadySvc: notReadySvc{stopped: &atomic.Bool{}}, waited: func() {
			if err := postShutdown(env); err != nil {
				t.Errorf("shutdown: %v", err)
			}
		}}
	}}
}

// postShutdown asks env's Instance to shut down, as `gcpemu stop` does.
func postShutdown(env *emu.Env) error {
	resp, err := http.Post("http://"+env.Endpoints.Get("gateway")+"/_emu/v1/shutdown", "", nil)
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

// waitTimeout is what these tests give an Instance to become ready.
const waitTimeout = 20 * time.Second

// promptly fails t unless what began at start ended well before
// waitTimeout.
func promptly(t *testing.T, start time.Time) {
	t.Helper()
	if d := time.Since(start); d > waitTimeout/2 {
		t.Errorf("took %s, with a wait timeout of %s", d, waitTimeout)
	}
}

// TestStartStoppedDuringStartup is #1000: under CI, where an Instance must
// become ready, one stopped during startup exits at once and cleanly. It
// has not failed to become ready.
func TestStartStoppedDuringStartup(t *testing.T) {
	t.Setenv("CI", "true")
	cmd := New(stoppedWhileWaiting(t))
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"start", "--data-dir", t.TempDir(), "--config", os.DevNull, "--port", "0", "--services", "gcs", "--wait-timeout", waitTimeout.String()})
	start := time.Now()
	if err := cmd.Execute(); err != nil {
		t.Errorf("start: %v", err)
	}
	promptly(t, start)
}

// TestRunStoppedDuringStartup is #1000: `gcpemu run` never runs its command
// on an Instance stopped during startup, and does not call that a readiness
// failure.
func TestRunStoppedDuringStartup(t *testing.T) {
	start := time.Now()
	stderr, marker, err := gcpemuRun(t, stoppedWhileWaiting(t), "touch", "--services", "gcs", "--wait-timeout", waitTimeout.String())
	promptly(t, start)
	if !errors.Is(err, instance.ErrShutDown) {
		t.Errorf("run: %v, want ErrShutDown", err)
	}
	if ran(marker) {
		t.Error("the command ran")
	}
	if strings.Contains(stderr, "not ready") {
		t.Errorf("reported as a readiness failure:\n%s", stderr)
	}
}

// TestHostInterruptedDuringStartup is #1000: a signal during startup, as
// that cancels host's context, stops the Instance and is not a readiness
// failure, whether or not the Instance had to become ready.
func TestHostInterruptedDuringStartup(t *testing.T) {
	for _, mustBeReady := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		stopped := &atomic.Bool{}
		o := &rootOpts{factories: map[string]instance.Factory{"gcs": func(*emu.Env) emu.Service {
			return &waitedSvc{notReadySvc: notReadySvc{stopped: stopped}, waited: cancel}
		}}}
		cfg := config.Defaults()
		cfg.Ephemeral = true
		cfg.DataDir = t.TempDir()
		cfg.Services = []string{"gcs"}
		cfg.Ports["gateway"] = 0
		cfg.WaitTimeout = waitTimeout
		var logs bytes.Buffer
		start := time.Now()
		_, err := o.host(ctx, &cfg, &logs, false, mustBeReady)
		promptly(t, start)
		if !errors.Is(err, instance.ErrShutDown) {
			t.Errorf("mustBeReady %v: %v, want ErrShutDown", mustBeReady, err)
		}
		if !stopped.Load() {
			t.Errorf("mustBeReady %v: the Instance kept running", mustBeReady)
		}
		if strings.Contains(logs.String(), "not ready") {
			t.Errorf("mustBeReady %v: reported as a readiness failure:\n%s", mustBeReady, logs.String())
		}
	}
}

// seedingSvc is a gcs whose Seeder runs seed.
type seedingSvc struct {
	notReadySvc
	env       *emu.Env
	ready     error
	seed      func(ctx context.Context) error
	stopErr   error
	stopBegun chan struct{} // closed when shutdown reaches Stop
}

func (s *seedingSvc) Ready() error { return s.ready }

func (s *seedingSvc) Stop(ctx context.Context) error {
	close(s.stopBegun)
	return errors.Join(s.notReadySvc.Stop(ctx), s.stopErr)
}

func (s *seedingSvc) ApplySeed(ctx context.Context, _ *yaml.Node, _ string) error {
	return s.seed(ctx)
}

// requestShutdown asks s's Instance to shut down, as `gcpemu stop` does, and
// returns once its shutdown has begun.
func (s *seedingSvc) requestShutdown() error {
	if err := postShutdown(s.env); err != nil {
		return err
	}
	<-s.stopBegun
	return nil
}

// seedingFactories makes s ready to run and returns factories for it.
func seedingFactories(s *seedingSvc) map[string]instance.Factory {
	s.stopped, s.stopBegun = &atomic.Bool{}, make(chan struct{})
	return map[string]instance.Factory{"gcs": func(env *emu.Env) emu.Service { s.env = env; return s }}
}

// shutDownWhileSeeding is factories whose Instance is asked to shut down
// during Seeding, by a Seeder that then waits for its context to end. It is
// also the Instance of the child that `start --detach` runs in a test.
func shutDownWhileSeeding() map[string]instance.Factory {
	s := &seedingSvc{}
	s.seed = func(ctx context.Context) error {
		if err := s.requestShutdown(); err != nil {
			return err
		}
		<-ctx.Done()
		return ctx.Err()
	}
	return seedingFactories(s)
}

// seedFile writes a seed file with a section for seedingSvc.
func seedFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "seed.yaml")
	if err := os.WriteFile(path, []byte("gcs: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// seedingConfig is hostConfig, Seeding from seedFile.
func seedingConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg := hostConfig(t)
	cfg.Seed = seedFile(t)
	return cfg
}

// hostConfig is an Ephemeral Instance's config running gcs.
func hostConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg := config.Defaults()
	cfg.Ephemeral = true
	cfg.DataDir = t.TempDir()
	cfg.Services = []string{"gcs"}
	cfg.Ports["gateway"] = 0
	cfg.WaitTimeout = waitTimeout
	return &cfg
}

// TestHostStoppedDuringSeeding is #1009: a stop during Seeding wins. Whether
// the stop is a signal or a shutdown request, however Seeding then ended, and
// whether or not the Instance had to become ready, host stops the Instance
// and reports a stop, promptly.
func TestHostStoppedDuringSeeding(t *testing.T) {
	ends := map[string]func(ctx context.Context) error{
		"waits for its context": func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() },
		"fails":                 func(context.Context) error { return errors.New("bad zone") },
		"succeeds":              func(context.Context) error { return nil },
	}
	stops := map[string]func(s *seedingSvc, cancel func()) error{
		"signal":           func(_ *seedingSvc, cancel func()) error { cancel(); return nil },
		"shutdown request": func(s *seedingSvc, _ func()) error { return s.requestShutdown() },
	}
	for stopName, stop := range stops {
		for endName, end := range ends {
			for _, mustBeReady := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/mustBeReady=%v", stopName, endName, mustBeReady), func(t *testing.T) {
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					s := &seedingSvc{}
					s.seed = func(ctx context.Context) error {
						if err := stop(s, cancel); err != nil {
							return err
						}
						return end(ctx)
					}
					o := &rootOpts{factories: seedingFactories(s)}
					var logs bytes.Buffer
					start := time.Now()
					_, err := o.host(ctx, seedingConfig(t), &logs, false, mustBeReady)
					promptly(t, start)
					if !errors.Is(err, instance.ErrShutDown) {
						t.Fatalf("host: %v, want ErrShutDown", err)
					}
					if strings.Contains(err.Error(), "seed:") {
						t.Errorf("reported as a Seeding failure: %v", err)
					}
					if !s.stopped.Load() {
						t.Error("the Instance kept running")
					}
					if l := logs.String(); strings.Contains(l, "msg=ready") || strings.Contains(l, "not ready") {
						t.Errorf("logged as ready or not ready:\n%s", l)
					}
				})
			}
		}
	}
}

// TestHostStopCoincidesWithSeedDeadline is #1009: when the seed deadline and
// a stop coincide, the result is a stop.
func TestHostStopCoincidesWithSeedDeadline(t *testing.T) {
	s := &seedingSvc{}
	s.seed = func(ctx context.Context) error {
		deadline, _ := ctx.Deadline()
		if err := s.requestShutdown(); err != nil {
			return err
		}
		time.Sleep(time.Until(deadline))
		return context.DeadlineExceeded
	}
	o := &rootOpts{factories: seedingFactories(s)}
	cfg := seedingConfig(t)
	cfg.WaitTimeout = 200 * time.Millisecond
	_, err := o.host(context.Background(), cfg, &bytes.Buffer{}, false, true)
	if !errors.Is(err, instance.ErrShutDown) || strings.Contains(err.Error(), "seed:") {
		t.Errorf("host: %v, want ErrShutDown", err)
	}
}

// TestHostStopThatLeavesThingsBehind is #1009: a stop that could not remove
// everything is still a Clean stop (ADR 0064). It says so in the log.
func TestHostStopThatLeavesThingsBehind(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &seedingSvc{stopErr: errors.New("container still there")}
	s.seed = func(ctx context.Context) error { cancel(); <-ctx.Done(); return ctx.Err() }
	o := &rootOpts{factories: seedingFactories(s)}
	var logs bytes.Buffer
	if _, err := o.host(ctx, seedingConfig(t), &logs, false, false); !errors.Is(err, instance.ErrShutDown) {
		t.Errorf("host: %v, want ErrShutDown", err)
	}
	if !strings.Contains(logs.String(), "container still there") {
		t.Errorf("not logged:\n%s", logs.String())
	}
}

// TestHostSeedingFailsWithNoStop: with no stop, a Seeder's error and a seed
// deadline stay Seeding failures.
func TestHostSeedingFailsWithNoStop(t *testing.T) {
	t.Run("a Seeder's error", func(t *testing.T) {
		bad := errors.New("bad zone")
		o := &rootOpts{factories: seedingFactories(&seedingSvc{seed: func(context.Context) error { return bad }})}
		_, err := o.host(context.Background(), seedingConfig(t), &bytes.Buffer{}, false, false)
		if !errors.Is(err, bad) || !strings.HasPrefix(err.Error(), "seed: ") || errors.Is(err, instance.ErrShutDown) {
			t.Errorf("host: %v, want a seed: error", err)
		}
	})
	t.Run("a deadline", func(t *testing.T) {
		o := &rootOpts{factories: seedingFactories(&seedingSvc{seed: func(ctx context.Context) error {
			<-ctx.Done()
			return ctx.Err()
		}})}
		cfg := seedingConfig(t)
		cfg.WaitTimeout = 100 * time.Millisecond
		_, err := o.host(context.Background(), cfg, &bytes.Buffer{}, false, true)
		if !errors.Is(err, context.DeadlineExceeded) || !strings.HasPrefix(err.Error(), "seed: ") || errors.Is(err, instance.ErrShutDown) {
			t.Errorf("host: %v, want a seed: error", err)
		}
	})
}

// failingStartSvc is a gcs whose Start interrupts host, as a signal does, and
// leaves Instance.Start unable to write its runtime files.
type failingStartSvc struct {
	notReadySvc
	dir       string
	interrupt func()
}

func (s failingStartSvc) Start(context.Context) error {
	s.interrupt()
	return os.MkdirAll(filepath.Join(s.dir, instance.EndpointsFile, "taken"), 0o700)
}

// TestHostInterruptedWhileStartFails is #1009: a signal counts as a stop
// however far host has got.
func TestHostInterruptedWhileStartFails(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cfg := hostConfig(t)
	stopped := &atomic.Bool{}
	o := &rootOpts{factories: map[string]instance.Factory{"gcs": func(*emu.Env) emu.Service {
		return failingStartSvc{notReadySvc: notReadySvc{stopped: stopped}, dir: cfg.InstanceDir(), interrupt: cancel}
	}}}
	if _, err := o.host(ctx, cfg, &bytes.Buffer{}, false, true); !errors.Is(err, instance.ErrShutDown) {
		t.Errorf("host: %v, want ErrShutDown", err)
	}
	if !stopped.Load() {
		t.Error("the Instance kept running")
	}
}

// slowStopSvc is a gcs that never becomes ready, is asked to shut down when
// something first waits for it, and takes stopFor to stop.
type slowStopSvc struct {
	waitedSvc
	stopFor time.Duration
}

func (s *slowStopSvc) Stop(ctx context.Context) error {
	time.Sleep(s.stopFor)
	return s.notReadySvc.Stop(ctx)
}

// TestHostDeadlinePassesDuringShutdown is #1009: the wait deadline passing
// while shutdown is in progress is a stop, not a readiness failure.
func TestHostDeadlinePassesDuringShutdown(t *testing.T) {
	cfg := hostConfig(t)
	cfg.WaitTimeout = 500 * time.Millisecond
	stopped := &atomic.Bool{}
	o := &rootOpts{factories: map[string]instance.Factory{"gcs": func(env *emu.Env) emu.Service {
		return &slowStopSvc{stopFor: 3 * cfg.WaitTimeout, waitedSvc: waitedSvc{notReadySvc: notReadySvc{stopped: stopped}, waited: func() {
			if err := postShutdown(env); err != nil {
				t.Errorf("shutdown: %v", err)
			}
		}}}
	}}}
	var logs bytes.Buffer
	if _, err := o.host(context.Background(), cfg, &logs, false, true); !errors.Is(err, instance.ErrShutDown) {
		t.Errorf("host: %v, want ErrShutDown", err)
	}
	if !stopped.Load() {
		t.Error("host returned before the Instance stopped")
	}
	if strings.Contains(logs.String(), "not ready") {
		t.Errorf("reported as a readiness failure:\n%s", logs.String())
	}
}

// TestStartStoppedDuringSeeding is #1009: `gcpemu start` stopped during
// Seeding is a Clean stop, so it exits 0, in CI too.
func TestStartStoppedDuringSeeding(t *testing.T) {
	t.Setenv("CI", "true")
	cmd := New(shutDownWhileSeeding())
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"start", "--data-dir", t.TempDir(), "--config", os.DevNull, "--port", "0", "--services", "gcs", "--seed", seedFile(t), "--wait-timeout", waitTimeout.String()})
	start := time.Now()
	if err := cmd.Execute(); err != nil {
		t.Errorf("start: %v", err)
	}
	promptly(t, start)
}

// TestRunStoppedDuringSeeding is #1009: `gcpemu run` returns ErrShutDown
// without running its command on an Instance stopped during Seeding.
func TestRunStoppedDuringSeeding(t *testing.T) {
	start := time.Now()
	_, marker, err := gcpemuRun(t, shutDownWhileSeeding(), "touch", "--services", "gcs", "--seed", seedFile(t), "--wait-timeout", waitTimeout.String())
	promptly(t, start)
	if !errors.Is(err, instance.ErrShutDown) {
		t.Errorf("run: %v, want ErrShutDown", err)
	}
	if ran(marker) {
		t.Error("the command ran")
	}
}

// TestDetachStoppedDuringSeeding is #1009: `start --detach` of an Instance
// stopped during Seeding, whose child here is a real `gcpemu start`, reports
// ErrShutDown and not an emulator exit.
func TestDetachStoppedDuringSeeding(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CI", "true")
	t.Setenv("GCPEMU_TEST_FAKE", "stopped-seeding")
	oldExe, oldArgs := detachExe, os.Args
	detachExe = func() (string, error) { return exe, nil }
	t.Cleanup(func() { detachExe, os.Args = oldExe, oldArgs })
	cfg := config.Defaults()
	cfg.DataDir = t.TempDir()
	cfg.WaitTimeout = waitTimeout
	os.Args = []string{"gcpemu", "start", "--detach", "--data-dir", cfg.DataDir, "--config", os.DevNull, "--port", "0", "--services", "gcs", "--seed", seedFile(t), "--wait-timeout", waitTimeout.String()}
	cmd := &cobra.Command{}
	cmd.SetOut(&bytes.Buffer{})
	start := time.Now()
	err = startDetached(cmd, &cfg)
	promptly(t, start)
	if !errors.Is(err, instance.ErrShutDown) {
		t.Errorf("start --detach: %v, want ErrShutDown", err)
	}
}
