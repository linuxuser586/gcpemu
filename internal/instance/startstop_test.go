package instance

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/hostmode"
)

// journal records, in order, what happened to the scriptedSvcs that share
// it, for a test to compare with what should have.
type journal struct {
	mu      sync.Mutex
	entries []string
}

func (j *journal) add(entry string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.entries = append(j.entries, entry)
}

func (j *journal) all() []string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return slices.Clone(j.entries)
}

// scriptedSvc is a Service that keeps a journal and whose Start runs start.
type scriptedSvc struct {
	name    string
	journal *journal
	ready   error
	start   func(ctx context.Context) error
}

func (s *scriptedSvc) Name() string              { return s.name }
func (s *scriptedSvc) Register(emu.Router) error { return nil }
func (s *scriptedSvc) Ready() error              { return s.ready }

func (s *scriptedSvc) Start(ctx context.Context) error {
	s.journal.add(s.name + " Start")
	defer s.journal.add(s.name + " Start returned")
	if s.start != nil {
		return s.start(ctx)
	}
	return nil
}

func (s *scriptedSvc) Stop(context.Context) error {
	s.journal.add(s.name + " Stop")
	return nil
}

// newScripted returns an Instance running svcs, not yet started, and its
// log. An Ephemeral Instance writes no PID file or endpoints file to check.
func newScripted(t *testing.T, ephemeral bool, svcs ...*scriptedSvc) (*Instance, *syncBuffer) {
	t.Helper()
	cfg := config.Defaults()
	cfg.Ephemeral = ephemeral
	cfg.DataDir = t.TempDir()
	cfg.Ports["gateway"] = 0
	factories := map[string]Factory{}
	for _, s := range svcs {
		cfg.Services = append(cfg.Services, s.name)
		factories[s.name] = func(*emu.Env) emu.Service { return s }
	}
	logs := &syncBuffer{}
	in, err := New(&cfg, factories, logs)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = in.Shutdown(ctx)
	})
	return in, logs
}

// waitDone fails t unless in's Done closes soon.
func waitDone(t *testing.T, in *Instance) {
	t.Helper()
	select {
	case <-in.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("Done did not close")
	}
}

// noRuntimeFiles fails t if in's PID file or endpoints file exists.
func noRuntimeFiles(t *testing.T, in *Instance) {
	t.Helper()
	for _, name := range []string{PIDFile, EndpointsFile} {
		if _, err := os.Stat(filepath.Join(in.Dir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s left behind: %v", name, err)
		}
	}
}

// awaitCancel returns when ctx ends, failing t if it does not end soon.
func awaitCancel(t *testing.T, ctx context.Context) {
	t.Helper()
	select {
	case <-ctx.Done():
	case <-time.After(10 * time.Second):
		t.Error("the context given to Start was not cancelled when shutdown began")
	}
}

// postShutdown makes the Admin API's shutdown request.
func postShutdown(t *testing.T, in *Instance) {
	t.Helper()
	resp, err := http.Post("http://"+in.Env.Endpoints.Get("gateway")+"/_emu/v1/shutdown", "", nil)
	if err != nil {
		t.Error(err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Errorf("shutdown request: %s", resp.Status)
	}
}

// TestShutdownRequestDuringStart is #1011: a request made during a Service's
// Start, as the Admin API can take one, waits for Start. The first Service's
// context is cancelled, the second never starts, teardown follows Start and
// nothing Start writes is left behind.
func TestShutdownRequestDuringStart(t *testing.T) {
	var in *Instance
	j := &journal{}
	first := &scriptedSvc{name: "dns", journal: j, start: func(ctx context.Context) error {
		postShutdown(t, in)
		awaitCancel(t, ctx)
		return nil
	}}
	second := &scriptedSvc{name: "gcs", journal: j}
	var logs *syncBuffer
	in, logs = newScripted(t, false, first, second)
	if err := in.Start(context.Background()); !errors.Is(err, ErrShutDown) {
		t.Errorf("Start: %v, want ErrShutDown", err)
	}
	waitDone(t, in)
	want := []string{"dns Start", "dns Start returned", "gcs Stop", "dns Stop"}
	if got := j.all(); !slices.Equal(got, want) {
		t.Errorf("events = %q, want %q", got, want)
	}
	noRuntimeFiles(t, in)
	if strings.Contains(logs.String(), "gcpemu started") {
		t.Error(`a stopped Start logged "gcpemu started"`)
	}
}

// TestRequestShutdownBeginsAtOnce is #1011: a shutdown has begun by the time
// RequestShutdown returns, so a Service's Start that requests one and returns
// at once is the last to start.
func TestRequestShutdownBeginsAtOnce(t *testing.T) {
	var in *Instance
	j := &journal{}
	first := &scriptedSvc{name: "dns", journal: j, start: func(context.Context) error {
		in.RequestShutdown()
		return nil
	}}
	in, _ = newScripted(t, true, first, &scriptedSvc{name: "gcs", journal: j})
	if err := in.Start(context.Background()); !errors.Is(err, ErrShutDown) {
		t.Errorf("Start: %v, want ErrShutDown", err)
	}
	waitDone(t, in)
	if slices.Contains(j.all(), "gcs Start") {
		t.Errorf("a second Service started: %q", j.all())
	}
}

// TestWaitReadyDuringStart is #1011: WaitReady hears of a shutdown while
// Start is still running, without waiting for Start to return, though
// teardown does.
func TestWaitReadyDuringStart(t *testing.T) {
	var in *Instance
	release := make(chan struct{})
	waited := make(chan error, 1)
	svc := &scriptedSvc{name: "dns", journal: &journal{}, ready: errors.New("starting"), start: func(context.Context) error {
		go func() { waited <- in.WaitReady(context.Background()) }()
		in.RequestShutdown()
		<-release
		return nil
	}}
	in, _ = newScripted(t, true, svc)
	started := make(chan error, 1)
	go func() { started <- in.Start(context.Background()) }()
	select {
	case err := <-waited:
		if !errors.Is(err, ErrShutDown) {
			t.Errorf("WaitReady: %v, want ErrShutDown", err)
		}
	case <-time.After(10 * time.Second):
		t.Error("WaitReady still waiting 10s after shutdown began")
	}
	select {
	case <-in.Done():
		t.Error("Done closed with Start still running")
	default:
	}
	close(release)
	if err := <-started; !errors.Is(err, ErrShutDown) {
		t.Errorf("Start: %v, want ErrShutDown", err)
	}
	waitDone(t, in)
}

// TestShutdownOutlastsAStuckStart is #1011: a Start that never returns can't
// hang a stop. Shutdown tears down once its ctx ends, and says so. When that
// Start does return, it has nothing more to start.
func TestShutdownOutlastsAStuckStart(t *testing.T) {
	var in *Instance
	j := &journal{}
	stuck, release := make(chan struct{}), make(chan struct{})
	first := &scriptedSvc{name: "dns", journal: j, start: func(context.Context) error {
		close(stuck)
		<-release
		return nil
	}}
	second := &scriptedSvc{name: "gcs", journal: j}
	var logs *syncBuffer
	in, logs = newScripted(t, false, first, second)
	started := make(chan error, 1)
	go func() { started <- in.Start(context.Background()) }()
	<-stuck
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_ = in.Shutdown(ctx)
	waitDone(t, in)
	if !strings.Contains(logs.String(), "Start is still running") {
		t.Errorf("no warning in the log:\n%s", logs)
	}
	close(release)
	if err := <-started; !errors.Is(err, ErrShutDown) {
		t.Errorf("Start: %v, want ErrShutDown", err)
	}
	want := []string{"dns Start", "gcs Stop", "dns Stop", "dns Start returned"}
	if got := j.all(); !slices.Equal(got, want) {
		t.Errorf("events = %q, want %q", got, want)
	}
	noRuntimeFiles(t, in)
}

// TestShutdownOfAStartedInstanceDoesNotWarn: with Start long returned, a ctx
// that has already ended is no reason to say Start is running.
func TestShutdownOfAStartedInstanceDoesNotWarn(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// Repeated as Shutdown's wait may see either event first.
	for range 20 {
		in, logs := newScripted(t, true, &scriptedSvc{name: "dns", journal: &journal{}})
		if err := in.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		_ = in.Shutdown(ctx)
		if strings.Contains(logs.String(), "Start is still running") {
			t.Fatalf("warned of a Start that had returned:\n%s", logs)
		}
	}
}

// TestShutdownWithoutStart is #1011: with no Start to wait for, Shutdown
// waits for nothing, even under a ctx that never ends.
func TestShutdownWithoutStart(t *testing.T) {
	in, logs := newScripted(t, true, &scriptedSvc{name: "dns", journal: &journal{}})
	finished := make(chan error, 1)
	go func() { finished <- in.Shutdown(context.Background()) }()
	select {
	case err := <-finished:
		if err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Shutdown waited for a Start that was never called")
	}
	waitDone(t, in)
	if strings.Contains(logs.String(), "Start is still running") {
		t.Errorf("warned of a Start that was never called:\n%s", logs)
	}
}

// TestStartAfterShutdown is #1011: a Start that comes after shutdown has
// begun does nothing at all, whether or not shutdown has finished.
func TestStartAfterShutdown(t *testing.T) {
	check := func(t *testing.T, in *Instance, j *journal) {
		t.Helper()
		if err := in.Start(context.Background()); !errors.Is(err, ErrShutDown) {
			t.Errorf("Start: %v, want ErrShutDown", err)
		}
		if slices.Contains(j.all(), "dns Start") {
			t.Error("a Service was started")
		}
		if addr := in.Env.Endpoints.Get("gateway"); addr != "" {
			t.Errorf("the gateway listener was opened on %s", addr)
		}
	}
	t.Run("once it has begun", func(t *testing.T) {
		j := &journal{}
		svc := &stoppingSvc{start: func() error { j.add("dns Start"); return nil }}
		in := newSvc(t, svc)
		in.RequestShutdown()
		<-svc.stopping
		check(t, in, j)
	})
	t.Run("once it has finished", func(t *testing.T) {
		j := &journal{}
		in, _ := newScripted(t, true, &scriptedSvc{name: "dns", journal: j})
		if err := in.Shutdown(context.Background()); err != nil {
			t.Fatal(err)
		}
		check(t, in, j)
	})
}

// hostModeOn turns on host mode for in, which New refuses outside Linux,
// with no container runtime for it to use: it fails at once in the
// background, as it needs no privileges to.
func hostModeOn(in *Instance) {
	in.Config.HostMode = true
	in.Env.Containers = nil
}

// TestHostModeReadDuringStart is #1011, for the race detector: the Admin API
// reads host mode's Manager while Start sets it, as does a shutdown.
func TestHostModeReadDuringStart(t *testing.T) {
	in, _ := newScripted(t, true, &scriptedSvc{name: "dns", journal: &journal{}})
	hostModeOn(in)
	end := make(chan struct{})
	var readers sync.WaitGroup
	readers.Go(func() {
		for {
			select {
			case <-end:
				return
			default:
				_ = in.HostMode()
			}
		}
	})
	err := in.Start(context.Background())
	if err != nil {
		t.Error(err)
	}
	if got := in.HostMode().State; got == hostmode.StateDisabled {
		t.Errorf("host mode state after Start = %s", got)
	}
	close(end)
	readers.Wait()
}

// TestHostModeNotStartedAfterTeardown is #1011: a Start that outlasts
// Shutdown's deadline does not start host mode, which nothing would stop.
func TestHostModeNotStartedAfterTeardown(t *testing.T) {
	stuck, release := make(chan struct{}), make(chan struct{})
	in, _ := newScripted(t, true, &scriptedSvc{name: "dns", journal: &journal{}, start: func(context.Context) error {
		close(stuck)
		<-release
		return nil
	}})
	hostModeOn(in)
	started := make(chan error, 1)
	go func() { started <- in.Start(context.Background()) }()
	<-stuck
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = in.Shutdown(ctx)
	close(release)
	<-started
	if got := in.HostMode().State; got != hostmode.StateDisabled {
		t.Errorf("host mode state = %s, want %s", got, hostmode.StateDisabled)
	}
}
