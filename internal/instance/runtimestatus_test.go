package instance

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// stallingRuntime is a container runtime that answers until stall is set,
// after which /version waits for release or for the caller to go away.
type stallingRuntime struct {
	stall    atomic.Bool
	release  chan struct{}
	versions atomic.Int64 // /version requests that arrived
	inFlight atomic.Int64 // /version requests not yet over
}

func (f *stallingRuntime) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case strings.HasSuffix(r.URL.Path, "/version"):
		f.versions.Add(1)
		f.inFlight.Add(1)
		defer f.inFlight.Add(-1)
		if f.stall.Load() {
			select {
			case <-f.release:
			case <-r.Context().Done():
				return
			}
		}
		_, _ = io.WriteString(w, `{"Version":"29.0.0","Arch":"amd64"}`)
	case strings.HasSuffix(r.URL.Path, "/info"):
		_, _ = io.WriteString(w, `{"OSType":"linux"}`)
	case strings.HasSuffix(r.URL.Path, "/volumes"):
		_, _ = io.WriteString(w, `{"Volumes":[]}`)
	default:
		_, _ = io.WriteString(w, `[]`)
	}
}

// useStallingRuntime makes it the runtime detection finds.
func useStallingRuntime(t *testing.T) *stallingRuntime {
	t.Helper()
	f := &stallingRuntime{release: make(chan struct{})}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	t.Cleanup(func() {
		select {
		case <-f.release:
		default:
			close(f.release)
		}
	})
	t.Setenv("DOCKER_HOST", "tcp://"+srv.Listener.Addr().String())
	t.Setenv("DOCKER_CONTEXT", "")
	return f
}

// infoRuntime asks the Admin API, as the CLI's check does, and returns the
// runtime it reports.
func infoRuntime(t *testing.T, base string) RuntimeStatus {
	t.Helper()
	c := &http.Client{Timeout: 5 * time.Second}
	resp, err := c.Get(base + "/_emu/v1/info")
	if err != nil {
		t.Fatalf("asking /_emu/v1/info: %v", err)
	}
	defer resp.Body.Close()
	var info struct {
		Runtime RuntimeStatus `json:"runtime"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		t.Fatal(err)
	}
	return info.Runtime
}

// TestInfoAnswersBeforeAStalledRuntime is #1025: the Instance's identity
// does not wait on the runtime, which is not yet known, not absent.
func TestInfoAnswersBeforeAStalledRuntime(t *testing.T) {
	f := useStallingRuntime(t)
	f.stall.Store(true)
	_, base, _ := startAdminToken(t, "127.0.0.1", "")
	got := infoRuntime(t, base)
	if want := (RuntimeStatus{Kind: "unknown"}); got != want {
		t.Fatalf("runtime = %+v, want %+v", got, want)
	}
}

// TestInfoTellsACallerThatAsksOnceAboutAHealthyRuntime: the probe is not
// the request's, but a healthy runtime still answers the first caller.
func TestInfoTellsACallerThatAsksOnceAboutAHealthyRuntime(t *testing.T) {
	useStallingRuntime(t)
	_, base, _ := startAdminToken(t, "127.0.0.1", "")
	got := infoRuntime(t, base)
	if want := (RuntimeStatus{Kind: "docker", Version: "29.0.0", Reachable: true}); got != want {
		t.Fatalf("runtime = %+v, want %+v", got, want)
	}
}

// TestInfoAnswersDuringAConnection is #1025: a connection in progress holds
// containers.mu, through Reclaim, however long the runtime takes.
func TestInfoAnswersDuringAConnection(t *testing.T) {
	useStallingRuntime(t)
	in, base, _ := startAdminToken(t, "127.0.0.1", "")
	in.containers.mu.Lock()
	var once sync.Once
	unlock := func() { once.Do(in.containers.mu.Unlock) }
	t.Cleanup(unlock)
	if got, want := infoRuntime(t, base), (RuntimeStatus{Kind: "unknown"}); got != want {
		t.Fatalf("runtime = %+v, want %+v", got, want)
	}
}

// shortStatusTTL makes the status go stale quickly.
func shortStatusTTL(t *testing.T) {
	t.Helper()
	old := runtimeStatusTTL
	runtimeStatusTTL = 20 * time.Millisecond
	t.Cleanup(func() { runtimeStatusTTL = old })
}

// TestAStalledRuntimeKeepsItsLastStatus: while a probe is out, callers get
// the last answer, not "unknown" again.
func TestAStalledRuntimeKeepsItsLastStatus(t *testing.T) {
	f := useStallingRuntime(t)
	shortStatusTTL(t)
	_, base, _ := startAdminToken(t, "127.0.0.1", "")
	want := infoRuntime(t, base)
	if !want.Reachable {
		t.Fatalf("runtime = %+v, want it reachable", want)
	}
	f.stall.Store(true)
	time.Sleep(50 * time.Millisecond)
	if got := infoRuntime(t, base); got != want {
		t.Fatalf("runtime = %+v, want the last answer %+v", got, want)
	}
}

// TestACallerGoingAwayDoesNotChangeTheStatus is #1025: its failure used to
// be cached for everyone.
func TestACallerGoingAwayDoesNotChangeTheStatus(t *testing.T) {
	f := useStallingRuntime(t)
	shortStatusTTL(t)
	in, base, _ := startAdminToken(t, "127.0.0.1", "")
	if _, err := in.containers.Runtime(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.stall.Store(true)
	impatient := &http.Client{Timeout: 100 * time.Millisecond}
	if resp, err := impatient.Get(base + "/_emu/v1/info"); err == nil {
		resp.Body.Close()
		t.Fatal("the impatient caller was answered, so nothing went away")
	}
	f.stall.Store(false)
	close(f.release)
	var got RuntimeStatus
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		if got = infoRuntime(t, base); got.Error != "" {
			t.Fatalf("runtime = %+v after a caller went away", got)
		}
		if got.Reachable {
			return
		}
	}
	t.Fatalf("runtime = %+v, want it reachable", got)
}

// TestConcurrentCallersShareOneProbe: a stalled runtime is not asked once
// per caller.
func TestConcurrentCallersShareOneProbe(t *testing.T) {
	f := useStallingRuntime(t)
	in, base, _ := startAdminToken(t, "127.0.0.1", "")
	if _, err := in.containers.Runtime(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.stall.Store(true)
	before := f.versions.Load()
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() { infoRuntime(t, base) })
	}
	wg.Wait()
	if got := f.versions.Load() - before; got != 1 {
		t.Fatalf("%d probes, want 1", got)
	}
}

// TestShutdownCancelsAProbe: a stalled probe neither holds up Shutdown nor
// outlives the Instance.
func TestShutdownCancelsAProbe(t *testing.T) {
	f := useStallingRuntime(t)
	f.stall.Store(true)
	in, base, _ := startAdminToken(t, "127.0.0.1", "")
	infoRuntime(t, base)
	start := time.Now()
	if err := in.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("Shutdown took %v with a probe out", d)
	}
	for end := time.Now().Add(time.Second); f.inFlight.Load() != 0; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatal("the probe outlived the Instance")
		}
	}
}
