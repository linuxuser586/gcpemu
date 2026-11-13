package instance

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/emu"
)

// slowStopSvc is a gateSvc whose Stop waits until its ctx ends, as
// Pub/Sub's does with a streaming pull in progress (#1117).
type slowStopSvc struct {
	gateSvc
	stopped chan struct{} // closed when Stop begins
}

func (s *slowStopSvc) Stop(ctx context.Context) error {
	close(s.stopped)
	<-ctx.Done()
	return nil
}

// startPersistentGateInstance starts a Persistent Instance serving svc
// through the API gateway, and returns it, its API gateway's URL and its log.
func startPersistentGateInstance(t *testing.T, svc *slowStopSvc) (*Instance, string, *syncBuffer) {
	t.Helper()
	svc.blocked, svc.release = make(chan struct{}), make(chan struct{})
	svc.stopped = make(chan struct{})
	cfg := config.Defaults()
	cfg.DataDir = t.TempDir()
	cfg.Services = []string{"dns"}
	cfg.Ports["gateway"] = 0
	logs := &syncBuffer{}
	in, err := New(&cfg, map[string]Factory{"dns": func(*emu.Env) emu.Service { return svc }}, logs)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := in.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = in.Shutdown(context.Background()) })
	if err := in.WaitReady(ctx); err != nil {
		t.Fatal(err)
	}
	return in, "http://" + in.Env.Endpoints.Get("gateway"), logs
}

// assertShuttingDown fails t unless resp is UNAVAILABLE, saying the
// Instance is shutting down, with no Retry-After.
func assertShuttingDown(t *testing.T, what string, resp *http.Response, body string) {
	t.Helper()
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") != "" ||
		!strings.Contains(body, `"UNAVAILABLE"`) || !strings.Contains(body, "shutting down") {
		t.Errorf("%s: %d Retry-After=%q %s, want 503 UNAVAILABLE saying the Instance is shutting down, with no Retry-After",
			what, resp.StatusCode, resp.Header.Get("Retry-After"), body)
	}
}

// TestTeardownSavesBeforeStoppingServices is #1118: Teardown closes the
// API gateway's gate and saves the Managed emulators' data before it stops
// any Service, so that a Service whose Stop waits out Teardown's ctx
// leaves the save its time, and the API gateway answers no write during it.
func TestTeardownSavesBeforeStoppingServices(t *testing.T) {
	svc := &slowStopSvc{}
	in, base, _ := startPersistentGateInstance(t, svc)
	type answer struct {
		resp *http.Response
		body string
		err  error
	}
	inProgress := make(chan answer, 1)
	go func() {
		resp, err := http.Get(base + "/dns/block")
		if err != nil {
			inProgress <- answer{err: err}
			return
		}
		b, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		inProgress <- answer{resp, string(b), err}
	}()
	<-svc.blocked

	saved := false
	in.hooks.saving = func(ctx context.Context) {
		saved = true
		select {
		case <-svc.stopped:
			t.Error("a Service was stopped before the save")
		default:
		}
		if err := ctx.Err(); err != nil {
			t.Errorf("the save's ctx has already ended: %v", err)
		}
		if !svc.returned.Load() {
			t.Error("saving while a request through the API gateway is still in its handler")
		}
		resp, body := call(t, http.DefaultClient, http.MethodGet, base+"/dns/v1/x", nil)
		assertShuttingDown(t, "request during the save", resp, body)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := in.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if !saved {
		t.Fatal("Teardown did not reach the save")
	}
	if a := <-inProgress; a.err != nil {
		t.Errorf("request cut off: %v, want 503", a.err)
	} else {
		assertShuttingDown(t, "request cut off", a.resp, a.body)
	}
}

// An API gateway handler that ignores its cancellation holds the save back by
// no more than teardownGateWait, with a warning naming how many requests
// still run, and the save still has its time (#1118).
func TestTeardownGateWaitIsLimited(t *testing.T) {
	old := teardownGateWait
	teardownGateWait = 100 * time.Millisecond
	t.Cleanup(func() { teardownGateWait = old })
	svc := &slowStopSvc{gateSvc: gateSvc{ignoreCtx: true}}
	in, base, logs := startPersistentGateInstance(t, svc)
	t.Cleanup(func() { close(svc.release) })
	go func() {
		if resp, err := http.Get(base + "/dns/block"); err == nil {
			resp.Body.Close()
		}
	}()
	<-svc.blocked

	var began time.Time
	saving := make(chan time.Duration, 1)
	in.hooks.saving = func(ctx context.Context) {
		if err := ctx.Err(); err != nil {
			t.Errorf("the save's ctx has already ended: %v", err)
		}
		deadline, _ := ctx.Deadline()
		saving <- time.Until(deadline)
		if d := time.Since(began); d > 5*time.Second {
			t.Errorf("the save began %v after Teardown, want about teardownGateWait", d)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	began = time.Now()
	_ = in.Shutdown(ctx)
	select {
	case left := <-saving:
		if left < time.Second {
			t.Errorf("the save had %v left, want most of Teardown's ctx", left)
		}
	default:
		t.Fatal("Teardown did not reach the save")
	}
	if l := logs.String(); !strings.Contains(l, "level=WARN msg=\"shutting down while requests through the API gateway are still running") || !strings.Contains(l, "running=1") {
		t.Errorf("log = %q, want a warning naming the 1 request still running", l)
	}
}
