package gke

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"cloud.google.com/go/container/apiv1/containerpb"

	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/netplane"
	"github.com/linuxuser586/gcpemu/internal/runtime"
)

// fakeServer drives a serverSupervisor: each exit sent on exits ends the
// current wait, and restarts answers each restart in turn (nil starts a
// new container).
type fakeServer struct {
	t        *testing.T
	exits    chan error
	restarts []error
	mu       sync.Mutex
	now      time.Time
	waited   []string // container IDs waited on
	started  int
	warned   []error
	degraded []error
	done     chan struct{}
}

func newFakeServer(t *testing.T, restarts ...error) *fakeServer {
	return &fakeServer{t: t, exits: make(chan error), restarts: restarts,
		now: time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC), done: make(chan struct{})}
}

func (f *fakeServer) supervisor() *serverSupervisor {
	return &serverSupervisor{
		now: func() time.Time {
			f.mu.Lock()
			defer f.mu.Unlock()
			return f.now
		},
		wait: func(ctx context.Context, id string) error {
			f.mu.Lock()
			f.waited = append(f.waited, id)
			f.mu.Unlock()
			select {
			case <-ctx.Done():
				return nil
			case err := <-f.exits:
				return err
			}
		},
		restart: func(ctx context.Context) (string, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.started++
			if f.started > len(f.restarts) {
				f.t.Errorf("restart %d; want at most %d", f.started, len(f.restarts))
				return "", errors.New("unexpected restart")
			}
			if err := f.restarts[f.started-1]; err != nil {
				return "", err
			}
			return "cp" + string(rune('1'+f.started)), nil
		},
		warn: func(err error) {
			f.mu.Lock()
			f.warned = append(f.warned, err)
			f.mu.Unlock()
		},
		degrade: func(err error) {
			f.mu.Lock()
			f.degraded = append(f.degraded, err)
			f.mu.Unlock()
		},
	}
}

// run starts the supervisor on container cp1 until the test ends.
func (f *fakeServer) run() {
	ctx, cancel := context.WithCancel(context.Background())
	go func() { defer close(f.done); f.supervisor().run(ctx, "cp1") }()
	f.t.Cleanup(func() { cancel(); <-f.done })
}

func (f *fakeServer) advance(d time.Duration) {
	f.mu.Lock()
	f.now = f.now.Add(d)
	f.mu.Unlock()
}

// waitFor polls cond until it holds or a second passes.
func (f *fakeServer) waitFor(what string, cond func() bool) {
	f.t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		f.mu.Lock()
		ok := cond()
		f.mu.Unlock()
		if ok {
			return
		}
		if time.Now().After(deadline) {
			f.t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// exit makes the watched container exit and waits for the supervisor to
// wait on another container or give up.
func (f *fakeServer) exit(code int, logs string) {
	f.t.Helper()
	f.mu.Lock()
	n := len(f.waited)
	f.mu.Unlock()
	f.exits <- &serverStoppedError{code: code, logs: logs}
	f.waitFor("the supervisor to settle", func() bool {
		return len(f.waited) > n || len(f.degraded) > 0
	})
}

// A Cluster server that exits is started again, the exit is logged with
// its code and log tail, and the new container is watched (#840).
func TestSupervisorRestartsExitedServer(t *testing.T) {
	f := newFakeServer(t, nil)
	f.run()
	f.waitFor("the first wait", func() bool { return len(f.waited) == 1 })
	f.exit(1, `level=fatal msg="Transaction commit failed"`)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.started != 1 {
		t.Errorf("restarted %d times; want 1", f.started)
	}
	if want := []string{"cp1", "cp2"}; strings.Join(f.waited, ",") != strings.Join(want, ",") {
		t.Errorf("waited on %v; want %v", f.waited, want)
	}
	if len(f.warned) != 1 {
		t.Fatalf("warned %d times; want 1", len(f.warned))
	}
	for _, want := range []string{"code 1", "Transaction commit failed"} {
		if !strings.Contains(f.warned[0].Error(), want) {
			t.Errorf("warning %q; want it to contain %q", f.warned[0], want)
		}
	}
	if len(f.degraded) != 0 {
		t.Errorf("degraded: %v", f.degraded)
	}
}

// The third exit within 10 minutes is not restarted: the supervisor gives
// up with that exit, which holds the log tail (#840).
func TestSupervisorGivesUpAfterCrashLoop(t *testing.T) {
	f := newFakeServer(t, nil, nil)
	f.run()
	f.waitFor("the first wait", func() bool { return len(f.waited) == 1 })
	f.exit(1, "first")
	f.advance(4 * time.Minute)
	f.exit(1, "second")
	f.advance(4 * time.Minute)
	f.exit(1, "third")
	select {
	case <-f.done:
	case <-time.After(time.Second):
		t.Fatal("supervisor still running after giving up")
	}
	if f.started != 2 {
		t.Errorf("restarted %d times; want 2", f.started)
	}
	if len(f.degraded) != 1 || !strings.Contains(f.degraded[0].Error(), "third") {
		t.Errorf("degraded with %v; want the third exit", f.degraded)
	}
}

// Exits spaced more than 10 minutes apart never reach the limit.
func TestSupervisorSpacedExits(t *testing.T) {
	f := newFakeServer(t, nil, nil, nil, nil, nil)
	f.run()
	f.waitFor("the first wait", func() bool { return len(f.waited) == 1 })
	for range 5 {
		f.exit(1, "exit")
		f.advance(6 * time.Minute)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.started != 5 || len(f.degraded) != 0 {
		t.Errorf("restarted %d times and degraded %v; want 5 restarts and no degrade", f.started, f.degraded)
	}
}

// A restart that fails, as one that never becomes ready does, counts as
// another exit: two failed restarts after an exit make three.
func TestSupervisorFailedRestartCounts(t *testing.T) {
	notReady := &serverExitedError{code: 255, logs: "never ready"}
	f := newFakeServer(t, notReady, errors.New("pulling image"))
	f.run()
	f.waitFor("the first wait", func() bool { return len(f.waited) == 1 })
	f.exits <- &serverStoppedError{code: 1, logs: "first"}
	select {
	case <-f.done:
	case <-time.After(time.Second):
		t.Fatal("supervisor still running after three exits")
	}
	if f.started != 2 {
		t.Errorf("restarted %d times; want 2", f.started)
	}
	if len(f.warned) != 2 || !strings.Contains(f.warned[1].Error(), "never ready") {
		t.Errorf("warned %v; want the exit, then the failed restart", f.warned)
	}
	// The last k3s log tail seen is kept when the last failure has none.
	if len(f.degraded) != 1 || !strings.Contains(f.degraded[0].Error(), "pulling image") || !strings.Contains(f.degraded[0].Error(), "never ready") {
		t.Errorf("degraded with %v; want the last failed restart and the last log tail", f.degraded)
	}
}

// A wait that ends because the watch was cancelled is not an exit.
func TestSupervisorCancelled(t *testing.T) {
	f := newFakeServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { defer close(f.done); f.supervisor().run(ctx, "cp1") }()
	f.waitFor("the first wait", func() bool { return len(f.waited) == 1 })
	cancel()
	<-f.done
	if f.started != 0 || len(f.warned) != 0 || len(f.degraded) != 0 {
		t.Errorf("restarted %d, warned %v, degraded %v; want nothing", f.started, f.warned, f.degraded)
	}
}

// watched is a Cluster server container that a test kills, as an exit
// or as a stop the Service makes on purpose.
type watched struct {
	killed   chan struct{}
	once     sync.Once
	mu       sync.Mutex
	restarts int
	fail     error // returned by each restart when set
}

func (w *watched) kill() { w.once.Do(func() { close(w.killed) }) }

func (w *watched) restarted() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.restarts
}

// setServer records the cluster's ready Cluster server container.
func (rt *clusterRT) setServer(id string) {
	rt.mu.Lock()
	rt.server = id
	rt.mu.Unlock()
}

// watching reports whether the cluster's Cluster server is watched.
func (rt *clusterRT) watching() bool {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.watch != nil
}

// watchTestServer gives a RUNNING test cluster a Cluster server, cp1,
// whose watch uses w instead of the runtime, and starts watching it.
func watchTestServer(t *testing.T, s *Service, w *watched) string {
	t.Helper()
	key := putTestCluster(t, s, containerpb.Cluster_RUNNING)
	w.killed = make(chan struct{})
	s.serverWait = func(ctx context.Context, id string) error {
		w.mu.Lock()
		killed := w.killed
		w.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil
		case <-killed:
			return &serverStoppedError{code: 137, logs: `level=fatal msg="Transaction commit failed"`}
		}
	}
	s.serverRestart = func(ctx context.Context, key string) (string, error) {
		w.mu.Lock()
		defer w.mu.Unlock()
		w.restarts++
		if w.fail != nil {
			return "", w.fail
		}
		w.killed, w.once = make(chan struct{}), sync.Once{}
		return "cp-new", nil
	}
	s.rtFor(key).setServer("cp1")
	s.watchServer(s.bg.ctx, nil, key)
	return key
}

// eventually polls cond until it holds or two seconds pass.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A Cluster server the Service started again leaves the cluster RUNNING
// and the Service ready (#840).
func TestRestartedServerStaysReady(t *testing.T) {
	s := newUnitService(t)
	w := &watched{}
	key := watchTestServer(t, s, w)
	w.kill()
	eventually(t, "the restart", func() bool { return w.restarted() == 1 })
	if c := clusterStatus(t, s, key); c.Status != containerpb.Cluster_RUNNING {
		t.Errorf("status = %v; want RUNNING", c.Status)
	}
	if err := s.Ready(); err != nil {
		t.Errorf("Ready = %v; want ready", err)
	}
}

// A Cluster server that keeps exiting leaves its cluster DEGRADED, with
// the k3s log tail, and the Service not ready, naming the cluster (#840).
func TestCrashLoopDegradesCluster(t *testing.T) {
	s := newUnitService(t)
	w := &watched{fail: &serverExitedError{code: 1, logs: `level=fatal msg="Transaction commit failed"`}}
	key := watchTestServer(t, s, w)
	w.kill()
	eventually(t, "DEGRADED", func() bool { return clusterStatus(t, s, key).Status == containerpb.Cluster_DEGRADED })
	if n := w.restarted(); n != serverExitLimit-1 {
		t.Errorf("restarted %d times; want %d", n, serverExitLimit-1)
	}
	if msg := clusterStatus(t, s, key).StatusMessage; !strings.Contains(msg, "Transaction commit failed") || !strings.Contains(msg, "Cluster server") {
		t.Errorf("statusMessage = %q; want the Cluster server's exits and log tail", msg)
	}
	err := s.Ready()
	if err == nil || !strings.Contains(err.Error(), "projects/p/locations/us-central1-a/clusters/c") {
		t.Errorf("Ready = %v; want it to name the cluster", err)
	}
	if n := w.restarted(); n != serverExitLimit-1 {
		t.Errorf("restarted %d times after giving up; want %d", n, serverExitLimit-1)
	}
}

// The stops the Service makes on purpose cancel the watch first, so the
// server they stop is not started again (#840).
func TestPurposefulStopsAreNotExits(t *testing.T) {
	for _, tt := range []struct {
		name string
		stop func(t *testing.T, s *Service, w *watched, key string)
	}{
		// The network policy restart and a version upgrade both recreate
		// the server through startServer.
		{"startServer", func(t *testing.T, s *Service, w *watched, key string) {
			s.beforeServerStart = func(context.Context, string) { w.kill() }
			if err := s.startServer(context.Background(), nil, key, emu.SubnetNet{}, "no-such-version"); err == nil {
				t.Fatal("startServer at an unknown version succeeded")
			}
		}},
		{"delete", func(t *testing.T, s *Service, w *watched, key string) {
			_ = s.destroyDataPlane(context.Background(), key)
			w.kill()
		}},
		// A State snapshot restore runs Reset first.
		{"Reset", func(t *testing.T, s *Service, w *watched, key string) {
			s.beforeResetTeardown = w.kill
			// Fails tearing down: the unit Service has no runtime.
			_ = s.Reset(context.Background())
		}},
		{"Stop", func(t *testing.T, s *Service, w *watched, key string) {
			if err := s.Stop(context.Background()); err != nil {
				t.Fatal(err)
			}
			w.kill()
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := newUnitService(t)
			w := &watched{}
			key := watchTestServer(t, s, w)
			tt.stop(t, s, w, key)
			time.Sleep(50 * time.Millisecond)
			if n := w.restarted(); n != 0 {
				t.Errorf("restarted %d times; want 0", n)
			}
			if s.rtFor(key).watching() {
				t.Error("still watching the server")
			}
		})
	}
}

// A wait that fails while the Cluster server still runs, as when the
// runtime's API drops a connection, is not an exit: the watch waits again
// and reports the exit that follows, with its code and log tail.
func TestAwaitServerExitRetriesRuntimeError(t *testing.T) {
	var waits atomic.Int32
	docker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		exited := waits.Load() >= 2
		switch {
		case strings.HasSuffix(r.URL.Path, "/containers/cp1/wait"):
			if waits.Add(1) == 1 {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"message":"connection reset"}`))
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"StatusCode": 1})
		case strings.HasSuffix(r.URL.Path, "/containers/cp1/json"):
			status := map[bool]string{false: "running", true: "exited"}[exited]
			_ = json.NewEncoder(w).Encode(map[string]any{"Id": "cp1", "Name": "/cp1",
				"State": map[string]any{"Status": status, "Running": !exited, "ExitCode": 1}})
		case strings.HasSuffix(r.URL.Path, "/containers/cp1/logs"):
			msg := `level=fatal msg="Transaction commit failed"` + "\n"
			hdr := make([]byte, 8)
			hdr[0] = 2
			binary.BigEndian.PutUint32(hdr[4:], uint32(len(msg)))
			_, _ = w.Write(append(hdr, msg...))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer docker.Close()
	cl, err := runtime.New("tcp://" + strings.TrimPrefix(docker.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	d := &deps{rt: &runtime.Manager{Client: cl, InstanceID: "i1"}}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err = awaitServerExit(ctx, d, "cp1")
	var stopped *serverStoppedError
	if !errors.As(err, &stopped) || stopped.code != 1 || !strings.Contains(stopped.logs, "Transaction commit failed") {
		t.Fatalf("awaitServerExit = %v; want the exit with code 1 and the log tail", err)
	}
	if n := waits.Load(); n != 2 {
		t.Errorf("waited %d times; want 2", n)
	}
}

// Serving the API server again at the same target keeps the listeners and
// their ports, so a restarted Cluster server's endpoint neither unbinds
// nor moves (#840); a new target is served anew.
func TestServeAPIKeepsListenerForSameTarget(t *testing.T) {
	s := newUnitService(t)
	s.env.Config.GKEBind = []string{"127.0.0.1"}
	d := &deps{np: netplane.New(nil, nil, "127.0.0.1", slog.New(slog.NewTextHandler(io.Discard, nil)))}
	const key = "p/us-central1-a/c"
	rt := s.rtFor(key)
	t.Cleanup(func() { s.stopRT(rt) })
	addr, err := s.serveAPI(d, key, 0, "10.0.0.2:443")
	if err != nil {
		t.Fatal(err)
	}
	lan := s.serveLAN(context.Background(), d, key, "c", 0, "10.0.0.2:443")
	api, lanEx := rt.api, rt.lan[0]
	if again, err := s.serveAPI(d, key, 0, "10.0.0.2:443"); err != nil || again != addr || rt.api != api {
		t.Errorf("serveAPI again = %s, %v; want the same listener at %s", again, err, addr)
	}
	if again := s.serveLAN(context.Background(), d, key, "c", lan, "10.0.0.2:443"); again != lan || rt.lan[0] != lanEx {
		t.Errorf("serveLAN again = %d; want the same listener at %d", again, lan)
	}
	if _, err := s.serveAPI(d, key, 0, "10.0.0.3:443"); err != nil || rt.api == api {
		t.Errorf("serveAPI at a new target = %v; want a new listener", err)
	}
	if s.serveLAN(context.Background(), d, key, "c", lan, "10.0.0.3:443"); rt.lan[0] == lanEx {
		t.Error("serveLAN at a new target kept the old listener")
	}
}
