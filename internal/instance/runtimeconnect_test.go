package instance

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/frontend"
	"github.com/linuxuser586/gcpemu/internal/runtime"
)

// hungRuntime is a container runtime that answers detection and hangs when
// asked to list containers, which is the first thing Reclaim does, until
// release closes or the caller goes away.
type hungRuntime struct {
	release  chan struct{}
	requests atomic.Int64
	lists    atomic.Int64
	// hangVersion hangs detection too, and versions counts what reached it.
	hangVersion atomic.Bool
	versions    atomic.Int64
	// answerLists makes container lists answer at once, or with listStatus
	// if set.
	answerLists atomic.Bool
	listStatus  atomic.Int64
	// self is the Instance's container; inspecting it hangs, and inspects
	// counts what reached that.
	self     string
	inspects atomic.Int64
}

func (f *hungRuntime) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.requests.Add(1)
	switch {
	case strings.HasSuffix(r.URL.Path, "/version"):
		f.versions.Add(1)
		if f.hangVersion.Load() && !f.hang(r) {
			return
		}
		_, _ = io.WriteString(w, `{"Version":"29.0.0","Arch":"amd64"}`)
	case strings.HasSuffix(r.URL.Path, "/info"):
		_, _ = io.WriteString(w, `{"OSType":"linux"}`)
	case strings.HasSuffix(r.URL.Path, "/containers/json"):
		f.lists.Add(1)
		if st := f.listStatus.Load(); st != 0 {
			w.WriteHeader(int(st))
			return
		}
		if !f.answerLists.Load() && !f.hang(r) {
			return
		}
		_, _ = io.WriteString(w, `[]`)
	case f.self != "" && strings.HasSuffix(r.URL.Path, "/containers/"+f.self+"/json"):
		f.inspects.Add(1)
		f.hang(r)
	case strings.HasSuffix(r.URL.Path, "/containers/me/json"):
		_, _ = io.WriteString(w, `{"Id":"`+f.self+`"}`)
	default:
		_, _ = io.WriteString(w, `{}`)
	}
}

// hang waits for release, and reports false if the caller went away first.
func (f *hungRuntime) hang(r *http.Request) bool {
	select {
	case <-f.release:
		return true
	case <-r.Context().Done():
		return false
	}
}

// connectingSvc connects to the runtime on a request's context, as the
// Build daemon's prune does.
type connectingSvc struct {
	adminSvc
	env    *emu.Env
	joined atomic.Int64
}

// SelfJoined implements emu.SelfJoiner.
func (s *connectingSvc) SelfJoined(map[string]string) { s.joined.Add(1) }

func (s *connectingSvc) Register(r emu.Router) error {
	if err := s.adminSvc.Register(r); err != nil {
		return err
	}
	r.Handle("POST /_emu/v1/fake/connect", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := s.env.Containers.Runtime(r.Context()); err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
		}
	}))
	return nil
}

// startOnHungRuntime starts an Instance that has not connected to f.
func startOnHungRuntime(t *testing.T) (*Instance, string, *hungRuntime) {
	t.Helper()
	f := &hungRuntime{release: make(chan struct{})}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	t.Setenv("DOCKER_HOST", "tcp://"+srv.Listener.Addr().String())
	t.Setenv("DOCKER_CONTEXT", "")
	cfg := config.Defaults()
	cfg.Ephemeral = true
	cfg.DataDir = t.TempDir()
	cfg.Services = []string{"dns"}
	cfg.Ports["gateway"] = 0
	cfg.Ports[frontend.Endpoint] = 0
	cfg.Bind = "127.0.0.1"
	cfg.NoRegistryRelay = true
	in, err := New(&cfg, map[string]Factory{"dns": func(e *emu.Env) emu.Service { return &connectingSvc{env: e} }}, &syncBuffer{})
	if err != nil {
		t.Fatal(err)
	}
	if err := in.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		close(f.release)
		_ = in.Shutdown(context.Background())
	})
	_, port, _ := net.SplitHostPort(in.Env.Endpoints.Get("gateway"))
	return in, "http://127.0.0.1:" + port, f
}

// connectOverARequest sends a request that connects to the runtime, and
// returns once the connection has reached Reclaim. Its client stays
// connected until the test ends.
func connectOverARequest(t *testing.T, base string, f *hungRuntime) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/_emu/v1/fake/connect", nil)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		if resp, err := http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
		}
	}()
	reached(t, &f.lists)
}

// reached waits for a runtime to get a request it counts in n.
func reached(t *testing.T, n *atomic.Int64) {
	t.Helper()
	for end := time.Now().Add(10 * time.Second); n.Load() == 0; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatal("the connection did not get that far")
		}
	}
}

// connecting connects to the runtime without a request and returns what
// that ends with.
func connecting(in *Instance) <-chan error {
	got := make(chan error, 1)
	go func() {
		_, err := in.containers.Runtime(context.Background())
		got <- err
	}()
	return got
}

// endsWithShutDown fails unless got, the end of a connection that shutdown
// has just cut short, says so.
func endsWithShutDown(t *testing.T, got <-chan error) {
	t.Helper()
	select {
	case err := <-got:
		if !errors.Is(err, ErrShutDown) {
			t.Fatalf("Runtime: %v, want ErrShutDown", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the connection is still running after shutdown began")
	}
}

// shutdownPromptly fails unless Shutdown, on a short deadline, returns well
// before anything that would have to wait out a hung runtime.
func shutdownPromptly(t *testing.T, in *Instance) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = in.Shutdown(ctx)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown is still waiting behind the connection, past its deadline")
	}
}

// TestShutdownEndsAConnectionInProgress is #1028: a request connecting to a
// hung runtime does not hold Teardown, however long its client stays, and
// what it left is not then cleaned up on that same runtime.
func TestShutdownEndsAConnectionInProgress(t *testing.T) {
	in, base, f := startOnHungRuntime(t)
	connectOverARequest(t, base, f)
	before := f.requests.Load()
	shutdownPromptly(t, in)
	if got := f.requests.Load() - before; got != 0 {
		t.Fatalf("Teardown made %d requests of the hung runtime", got)
	}
}

// TestShutdownEndsDetectionWithoutRulingOutTheRuntime: a runtime that detection
// did not get an answer from because shutdown began is not a missing runtime,
// which would be repeated to every later caller.
func TestShutdownEndsDetectionWithoutRulingOutTheRuntime(t *testing.T) {
	in, _, f := startOnHungRuntime(t)
	f.hangVersion.Store(true)
	got := connecting(in)
	reached(t, &f.versions)
	in.beginStop()
	endsWithShutDown(t, got)
	if _, err := in.containers.Runtime(context.Background()); !errors.Is(err, ErrShutDown) {
		t.Fatalf("Runtime afterwards: %v, want ErrShutDown", err)
	}
}

// TestACallerGoingAwayDoesNotRuleOutTheRuntime: detection is the Instance's and
// not the caller's, so one client leaving is not cached for everyone.
func TestACallerGoingAwayDoesNotRuleOutTheRuntime(t *testing.T) {
	in, _, _ := startOnHungRuntime(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := in.containers.Runtime(ctx); err == nil || errors.Is(err, emu.ErrNoRuntime) {
		t.Fatalf("Runtime: %v, want a transient error", err)
	}
}

// TestShutdownEndsAConnectionAheadOfAQueuedRegistryRelay: the Registry relay
// cannot stop waiting for the lock a connection holds, so Teardown's wait for
// the relay ends only because the connection does.
func TestShutdownEndsAConnectionAheadOfAQueuedRegistryRelay(t *testing.T) {
	in, base, f := startOnHungRuntime(t)
	in.Env.Endpoints.Set("ar", "127.0.0.1:1")
	connectOverARequest(t, base, f)
	in.Config.NoRegistryRelay = false
	in.startRegistryRelay()
	time.Sleep(100 * time.Millisecond) // lets the relay reach the lock
	shutdownPromptly(t, in)
}

// TestShutdownCutsAConnectionShortOfItsSelfAddresses: shutdown beginning while
// the Instance inspects its own container, after Reclaim, leaves no connection.
func TestShutdownCutsAConnectionShortOfItsSelfAddresses(t *testing.T) {
	in, _, f := startOnHungRuntime(t)
	t.Setenv(runtime.SelfEnv, "me") // after Start, which would connect for it
	f.self = "self-id"
	f.answerLists.Store(true)
	got := connecting(in)
	reached(t, &f.inspects)
	in.beginStop()
	endsWithShutDown(t, got)
}

// TestAConnectionCutShortTellsServicesNothing: SelfJoined writes the store,
// and it has no addresses to give.
func TestAConnectionCutShortTellsServicesNothing(t *testing.T) {
	in, base, f := startOnHungRuntime(t)
	connectOverARequest(t, base, f)
	shutdownPromptly(t, in)
	if got := in.services[0].(*connectingSvc).joined.Load(); got != 0 {
		t.Fatalf("SelfJoined called %d times", got)
	}
}

// TestNoConnectionStartsOnceShutdownHasBegun: a request during Teardown would
// otherwise connect after containers.shutdown, and write to a closed store.
func TestNoConnectionStartsOnceShutdownHasBegun(t *testing.T) {
	in, _, f := startOnHungRuntime(t)
	in.beginStop()
	before := f.requests.Load()
	if _, err := in.containers.Runtime(context.Background()); !errors.Is(err, ErrShutDown) {
		t.Fatalf("Runtime: %v, want ErrShutDown", err)
	}
	if got := f.requests.Load() - before; got != 0 {
		t.Fatalf("%d requests reached the runtime", got)
	}
}

// TestAConnectedInstanceKeepsItsRuntimeDuringShutdown: Teardown removes what
// the Instance made through it.
func TestAConnectedInstanceKeepsItsRuntimeDuringShutdown(t *testing.T) {
	in, _, f := startOnHungRuntime(t)
	f.answerLists.Store(true)
	want, err := in.containers.Runtime(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	in.beginStop()
	if got, err := in.containers.Runtime(context.Background()); err != nil || got != want {
		t.Fatalf("Runtime: %v, %v, want the connected runtime", got, err)
	}
}

// TestAReclaimThatFailsStillConnects: only shutdown makes a failed Reclaim end
// the connection. A runtime that refuses it is usable, and it is logged.
func TestAReclaimThatFailsStillConnects(t *testing.T) {
	in, _, f := startOnHungRuntime(t)
	f.listStatus.Store(http.StatusInternalServerError)
	if rt, err := in.containers.Runtime(context.Background()); err != nil || rt == nil {
		t.Fatalf("Runtime: %v, %v, want a connection", rt, err)
	}
}
