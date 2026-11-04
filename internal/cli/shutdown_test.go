package cli

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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
			resp, err := http.Post("http://"+env.Endpoints.Get("gateway")+"/_emu/v1/shutdown", "", nil)
			if err != nil {
				t.Errorf("shutdown: %v", err)
				return
			}
			resp.Body.Close()
		}}
	}}
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
