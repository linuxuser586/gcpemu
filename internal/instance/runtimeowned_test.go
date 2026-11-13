package instance

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linuxuser586/gcpemu/internal/emu"
)

// reclaimWarning is what the Instance logs when a Reclaim does not finish.
const reclaimWarning = "reclaiming orphaned containers"

// shortReclaim shortens the limit on a Reclaim for the test.
func shortReclaim(t *testing.T, d time.Duration) {
	t.Helper()
	old := reclaimTime
	reclaimTime = d
	t.Cleanup(func() { reclaimTime = old })
}

// stillConnecting calls Runtime with a budget of d and fails unless it gives up
// within a few times d, with a transient error.
func stillConnecting(t *testing.T, in *Instance, d time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	start := time.Now()
	rt, err := in.containers.Runtime(ctx)
	if took := time.Since(start); took > d+time.Second {
		t.Fatalf("Runtime with a %v budget took %v", d, took)
	}
	if err == nil || rt != nil || errors.Is(err, emu.ErrNoRuntime) {
		t.Fatalf("Runtime: %v, %v, want a transient error", rt, err)
	}
	if !strings.Contains(err.Error(), "in progress") {
		t.Fatalf("Runtime: %v, want it to say the connection is in progress", err)
	}
}

// TestAReclaimOutlastsTheCallerThatStartedIt is #1033: a caller with a short
// budget stops waiting for the connection, which goes on, Reclaim and all,
// and is the one a later caller gets.
func TestAReclaimOutlastsTheCallerThatStartedIt(t *testing.T) {
	in, _, f, logs := startOnHungRuntimeLogged(t)
	f.listDelay.Store(int64(500 * time.Millisecond))
	stillConnecting(t, in, 50*time.Millisecond)
	reached(t, &f.lists)
	versions := f.versions.Load()
	rt, err := in.containers.Runtime(context.Background())
	if err != nil || rt == nil {
		t.Fatalf("Runtime afterwards: %v, %v, want the connection", rt, err)
	}
	if got := f.lists.Load(); got != 1 {
		t.Fatalf("%d Reclaims, want 1", got)
	}
	if got := f.versions.Load(); got != versions {
		t.Fatalf("detection ran %d more times, want 0", got-versions)
	}
	if strings.Contains(logs.String(), reclaimWarning) {
		t.Fatalf("the Reclaim was cut short:\n%s", logs)
	}
	if again, err := in.containers.Runtime(context.Background()); err != nil || again != rt {
		t.Fatalf("Runtime once connected: %v, %v, want the same connection", again, err)
	}
}

// TestCallersWithShortBudgetsShareOneConnection: however many callers give up
// on a slow connection, it detects the runtime and reclaims once.
func TestCallersWithShortBudgetsShareOneConnection(t *testing.T) {
	in, _, f := startOnHungRuntime(t)
	f.listDelay.Store(int64(time.Second))
	versions := f.versions.Load()
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			stillConnecting(t, in, 50*time.Millisecond)
		}()
	}
	wg.Wait()
	reached(t, &f.lists)
	stillConnecting(t, in, 50*time.Millisecond)
	if _, err := in.containers.Runtime(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := f.lists.Load(); got != 1 {
		t.Fatalf("%d Reclaims, want 1", got)
	}
	// Detection asks for the version, and so does Info.
	if got := f.versions.Load() - versions; got > 2 {
		t.Fatalf("%d version requests, want those of one connection", got)
	}
}

// TestAReclaimPastItsLimitStillConnects: a runtime that hangs during a
// Reclaim holds the connection for the limit, which warns and connects.
func TestAReclaimPastItsLimitStillConnects(t *testing.T) {
	shortReclaim(t, 200*time.Millisecond)
	in, _, _, logs := startOnHungRuntimeLogged(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	rt, err := in.containers.Runtime(ctx)
	if err != nil || rt == nil {
		t.Fatalf("Runtime: %v, %v, want a connection", rt, err)
	}
	if took := time.Since(start); took < 200*time.Millisecond || took > 3*time.Second {
		t.Fatalf("connected after %v, want soon after the Reclaim's 200ms limit", took)
	}
	if !strings.Contains(logs.String(), reclaimWarning) {
		t.Fatalf("no warning that the Reclaim was cut short:\n%s", logs)
	}
}

// readyOnRuntimeSvc is ready once the runtime is connected, asking with a
// short budget, as Build's readiness does.
type readyOnRuntimeSvc struct {
	connectingSvc
}

func (s *readyOnRuntimeSvc) Ready() error {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := s.env.Containers.Runtime(ctx)
	return err
}

// TestNotReadyWhileConnecting: /_emu/v1/ready fails while the connection is in
// progress and passes once it has connected.
func TestNotReadyWhileConnecting(t *testing.T) {
	f := &hungRuntime{release: make(chan struct{})}
	f.listDelay.Store(int64(time.Second))
	in, _ := newOnRuntime(t, f, func(e *emu.Env) emu.Service {
		return &readyOnRuntimeSvc{connectingSvc{env: e}}
	})
	if err := in.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	ready := func() (int, string) {
		resp, err := http.Get("http://" + in.Env.Endpoints.Get("gateway") + "/_emu/v1/ready")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, body := ready(); code != http.StatusServiceUnavailable || !strings.Contains(body, "in progress") {
		t.Fatalf("ready while connecting = %d %s, want 503 naming the connection", code, body)
	}
	for end := time.Now().Add(10 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		code, body := ready()
		if code == http.StatusOK {
			break
		}
		if time.Now().After(end) {
			t.Fatalf("ready after connecting = %d %s, want 200", code, body)
		}
	}
}
