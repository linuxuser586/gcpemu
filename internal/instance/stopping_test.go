package instance

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/emu"
)

// stoppingSvc is a dns whose Stop holds the Instance's shutdown in progress
// until the test ends, and whose Seeder runs seed.
type stoppingSvc struct {
	ready    error
	start    func() error
	seed     func(ctx context.Context) error
	stopping chan struct{} // closed when shutdown reaches Stop
	release  chan struct{} // ends Stop
	once     sync.Once
}

func (s *stoppingSvc) Name() string              { return "dns" }
func (s *stoppingSvc) Register(emu.Router) error { return nil }
func (s *stoppingSvc) Ready() error              { return s.ready }
func (s *stoppingSvc) Start(context.Context) error {
	if s.start != nil {
		return s.start()
	}
	return nil
}

func (s *stoppingSvc) Stop(context.Context) error {
	close(s.stopping)
	<-s.release
	return nil
}

func (s *stoppingSvc) ApplySeed(ctx context.Context, _ *yaml.Node, _ string) error {
	return s.seed(ctx)
}

// startStopping returns a started Instance running svc.
func startStopping(t *testing.T, svc *stoppingSvc) *Instance {
	t.Helper()
	in := newStopping(t, svc)
	if err := in.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	return in
}

// newStopping returns an Instance running svc, not yet started.
func newStopping(t *testing.T, svc *stoppingSvc) *Instance {
	t.Helper()
	svc.stopping, svc.release = make(chan struct{}), make(chan struct{})
	cfg := config.Defaults()
	cfg.Ephemeral = true
	cfg.DataDir = t.TempDir()
	cfg.Services = []string{"dns"}
	cfg.Ports["gateway"] = 0
	in, err := New(&cfg, map[string]Factory{"dns": func(*emu.Env) emu.Service { return svc }}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		svc.endStop()
		in.RequestShutdown()
		select {
		case <-in.Done():
		case <-time.After(30 * time.Second):
			t.Error("the Instance did not shut down")
		}
	})
	return in
}

// endStop lets a shutdown in progress finish.
func (s *stoppingSvc) endStop() { s.once.Do(func() { close(s.release) }) }

// beginShutdown starts in's shutdown and leaves it in progress.
func beginShutdown(t *testing.T, in *Instance, svc *stoppingSvc) {
	t.Helper()
	in.RequestShutdown()
	select {
	case <-svc.stopping:
	case <-time.After(10 * time.Second):
		t.Fatal("shutdown did not begin")
	}
}

// TestWaitReadyDuringShutdown is #1009: from the moment shutdown begins,
// well before Done closes, WaitReady says so. It does not call an Instance
// ready that is shutting down, nor one past its deadline a readiness
// failure.
func TestWaitReadyDuringShutdown(t *testing.T) {
	for name, ready := range map[string]error{"ready": nil, "not ready": errors.New("not ready")} {
		t.Run(name, func(t *testing.T) {
			svc := &stoppingSvc{ready: ready}
			in := startStopping(t, svc)
			beginShutdown(t, in, svc)
			ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
			defer cancel()
			<-ctx.Done()
			if err := in.WaitReady(ctx); !errors.Is(err, ErrShutDown) {
				t.Errorf("WaitReady: %v, want ErrShutDown", err)
			}
		})
	}
}

// TestWaitReadyEndsWhenShutdownBegins is #1009: a wait in progress ends when
// shutdown begins, not when its deadline does.
func TestWaitReadyEndsWhenShutdownBegins(t *testing.T) {
	svc := &stoppingSvc{ready: errors.New("not ready")}
	in := startStopping(t, svc)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	waited := make(chan error, 1)
	go func() { waited <- in.WaitReady(ctx) }()
	beginShutdown(t, in, svc)
	select {
	case err := <-waited:
		if !errors.Is(err, ErrShutDown) {
			t.Errorf("WaitReady: %v, want ErrShutDown", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("WaitReady still waiting 10s after shutdown began")
	}
}

// TestWaitReadyWithNoStop: a readiness timeout with no stop stays one.
func TestWaitReadyWithNoStop(t *testing.T) {
	in := startStopping(t, &stoppingSvc{ready: errors.New("not ready")})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err := in.WaitReady(ctx)
	if err == nil || errors.Is(err, ErrShutDown) {
		t.Errorf("WaitReady: %v, want a readiness failure", err)
	}
}

// seedDoc is a seed file with a section for stoppingSvc.
var seedDoc = []byte("dns: {}\n")

// TestSeedingEndsWhenShutdownBegins is #1009: shutdown beginning cancels the
// Seeder's context, and what Seeding returns is that the Instance stopped.
func TestSeedingEndsWhenShutdownBegins(t *testing.T) {
	seeding := make(chan struct{})
	svc := &stoppingSvc{seed: func(ctx context.Context) error {
		close(seeding)
		<-ctx.Done()
		return ctx.Err()
	}}
	in := startStopping(t, svc)
	seeded := make(chan error, 1)
	go func() { seeded <- in.ApplySeedData(context.Background(), seedDoc, t.TempDir()) }()
	<-seeding
	beginShutdown(t, in, svc)
	select {
	case err := <-seeded:
		if !errors.Is(err, ErrShutDown) {
			t.Errorf("ApplySeedData: %v, want ErrShutDown", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("still Seeding 10s after shutdown began")
	}
}

// TestAStopWinsOverSeeding is #1009: once a stop has begun, how the Seeder
// ended does not matter, and a seed file that cannot even be read is no
// different.
func TestAStopWinsOverSeeding(t *testing.T) {
	for name, result := range map[string]error{"a Seeder's error": errors.New("bad zone"), "success": nil} {
		t.Run(name, func(t *testing.T) {
			var in *Instance
			svc := &stoppingSvc{}
			svc.seed = func(context.Context) error {
				beginShutdown(t, in, svc)
				return result
			}
			in = startStopping(t, svc)
			path := filepath.Join(t.TempDir(), "seed.yaml")
			if err := os.WriteFile(path, seedDoc, 0o600); err != nil {
				t.Fatal(err)
			}
			err := in.ApplySeed(context.Background(), path)
			if !errors.Is(err, ErrShutDown) {
				t.Fatalf("ApplySeed: %v, want ErrShutDown", err)
			}
			if err.Error() != ErrShutDown.Error() {
				t.Errorf("ApplySeed: %q says more than that the Instance stopped", err)
			}
		})
	}
	t.Run("an unreadable seed file", func(t *testing.T) {
		svc := &stoppingSvc{}
		in := startStopping(t, svc)
		beginShutdown(t, in, svc)
		if err := in.ApplySeed(context.Background(), filepath.Join(t.TempDir(), "absent")); !errors.Is(err, ErrShutDown) {
			t.Errorf("ApplySeed: %v, want ErrShutDown", err)
		}
	})
}

// TestSeedingWithNoStop: with no stop, Seeding's own failures stay what they
// are, a deadline included.
func TestSeedingWithNoStop(t *testing.T) {
	bad := errors.New("bad zone")
	in := startStopping(t, &stoppingSvc{seed: func(ctx context.Context) error {
		if _, ok := ctx.Deadline(); !ok {
			return bad
		}
		<-ctx.Done()
		return ctx.Err()
	}})
	if err := in.ApplySeedData(context.Background(), seedDoc, ""); !errors.Is(err, bad) || errors.Is(err, ErrShutDown) {
		t.Errorf("a Seeder's error: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := in.ApplySeedData(ctx, seedDoc, ""); !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrShutDown) {
		t.Errorf("a deadline: %v", err)
	}
}

// TestSeedUploadDuringAStop is #1009: POST /_emu/v1/seed shares Seeding's
// code, so a seed in progress is cut off and fails, saying why, and a
// seed sent to a stopping Instance is refused.
func TestSeedUploadDuringAStop(t *testing.T) {
	seeding := make(chan struct{})
	var once sync.Once
	svc := &stoppingSvc{seed: func(ctx context.Context) error {
		once.Do(func() { close(seeding) })
		<-ctx.Done()
		return ctx.Err()
	}}
	in := startStopping(t, svc)
	url := "http://" + in.Env.Endpoints.Get("gateway") + "/_emu/v1/seed"
	post := func() (int, string) {
		resp, err := http.Post(url, "application/yaml", bytes.NewReader(seedDoc))
		if err != nil {
			t.Error(err)
			return 0, ""
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	type result struct {
		code int
		body string
	}
	cut := make(chan result, 1)
	go func() { c, b := post(); cut <- result{c, b} }()
	<-seeding
	beginShutdown(t, in, svc)
	var got result
	select {
	case got = <-cut:
	case <-time.After(10 * time.Second):
		t.Fatal("the seed still running 10s after shutdown began")
	}
	if got.code != http.StatusServiceUnavailable || !strings.Contains(got.body, ErrShutDown.Error()) {
		t.Errorf("seed in progress = %d %s", got.code, got.body)
	}
	if code, body := post(); code != http.StatusServiceUnavailable || !strings.Contains(body, ErrShutDown.Error()) {
		t.Errorf("seed sent during the stop = %d %s", code, body)
	}
}

// TestStartDuringShutdown is #1009: Start failing once shutdown has begun is
// the stop, not a startup failure.
func TestStartDuringShutdown(t *testing.T) {
	var in *Instance
	svc := &stoppingSvc{}
	svc.start = func() error {
		beginShutdown(t, in, svc)
		// Start can no longer write its runtime files.
		return os.MkdirAll(filepath.Join(in.Dir, EndpointsFile, "taken"), 0o700)
	}
	in = newStopping(t, svc)
	if err := in.Start(context.Background()); !errors.Is(err, ErrShutDown) {
		t.Errorf("Start: %v, want ErrShutDown", err)
	}
}

// TestStartFailsWithNoStop: with no stop, Start's own failure stays one.
func TestStartFailsWithNoStop(t *testing.T) {
	var in *Instance
	svc := &stoppingSvc{}
	svc.start = func() error { return os.MkdirAll(filepath.Join(in.Dir, EndpointsFile, "taken"), 0o700) }
	in = newStopping(t, svc)
	if err := in.Start(context.Background()); err == nil || errors.Is(err, ErrShutDown) {
		t.Errorf("Start: %v, want its own failure", err)
	}
}
