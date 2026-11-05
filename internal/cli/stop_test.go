package cli

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linuxuser586/gcpemu/internal/instance"
)

// fakeProcess starts a process that stands in for an Instance's. It is
// killed when t ends.
func fakeProcess(t *testing.T) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), "GCPEMU_TEST_FAKE=sleep")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	t.Cleanup(func() { _ = cmd.Process.Kill(); <-done })
	return cmd
}

// deadPID returns the pid of a process that has exited.
func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), "GCPEMU_TEST_FAKE=exit")
	_ = cmd.Run()
	return cmd.Process.Pid
}

// stopGateway is an Instance's gateway and data dir, counting the shutdowns
// it accepted.
type stopGateway struct {
	dir       string
	shutdowns atomic.Int32
}

// newStopGateway serves info at /_emu/v1/info and runs onShutdown for each
// shutdown, from a data dir that holds the ID "want".
func newStopGateway(t *testing.T, info string, onShutdown func()) *stopGateway {
	t.Helper()
	g := &stopGateway{dir: t.TempDir()}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /_emu/v1/info":
			_, _ = io.WriteString(w, info)
		case "POST /_emu/v1/shutdown":
			g.shutdowns.Add(1)
			w.WriteHeader(http.StatusAccepted)
			_, _ = io.WriteString(w, `{"status":"shutting down"}`)
			if onShutdown != nil {
				onShutdown()
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	writeDataFile(t, g.dir, instance.EndpointsFile, `{"gateway":"`+strings.TrimPrefix(srv.URL, "http://")+`"}`)
	writeDataFile(t, g.dir, instance.IDFile, "want")
	return g
}

func writeDataFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// stop runs `gcpemu stop` on dir, returning its stdout.
func stop(dir string) (string, error) {
	var out bytes.Buffer
	root := New(nil)
	root.SetOut(&out)
	root.SetErr(io.Discard)
	root.SetArgs([]string{"stop", "--config", filepath.Join(dir, "none.yaml"), "--data-dir", dir})
	err := root.Execute()
	return out.String(), err
}

// infoBody is an /_emu/v1/info body for the Instance "want".
func infoBody(pid int, pidScope string) string {
	return fmt.Sprintf(`{"id":"want","pid":%d,"pidScope":%q}`, pid, pidScope)
}

// TestStopWaitsForTheInstanceItReached is #998: stop waits for the process
// the Instance reports, whatever the PID file says: one a crashed Instance
// left names a dead process, and a racing stop may find none.
func TestStopWaitsForTheInstanceItReached(t *testing.T) {
	for name, pidFile := range map[string]func(t *testing.T, dir string){
		"stale PID file": func(t *testing.T, dir string) {
			writeDataFile(t, dir, instance.PIDFile, strconv.Itoa(deadPID(t)))
		},
		"PID file already removed": func(*testing.T, string) {},
	} {
		t.Run(name, func(t *testing.T) {
			proc := fakeProcess(t)
			var killed atomic.Bool
			g := newStopGateway(t, infoBody(proc.Process.Pid, instance.PIDScope()), func() {
				time.AfterFunc(300*time.Millisecond, func() {
					killed.Store(true)
					_ = proc.Process.Kill()
				})
			})
			pidFile(t, g.dir)
			out, err := stop(g.dir)
			if err != nil {
				t.Fatalf("stop: %v", err)
			}
			if !killed.Load() {
				t.Error("stop returned while the Instance's process ran")
			}
			if !strings.Contains(out, "stopped") {
				t.Errorf("stop printed %q; want it to say the instance stopped", out)
			}
		})
	}
}

// TestStopTimeoutNamesPID is #999: a stop that gives up says which process
// it was still waiting on. This test's own process plays the one that will
// not exit, and so also shows that stop leaves it running.
func TestStopTimeoutNamesPID(t *testing.T) {
	was := stopTimeout
	stopTimeout = 300 * time.Millisecond
	t.Cleanup(func() { stopTimeout = was })
	pid := os.Getpid()
	g := newStopGateway(t, infoBody(pid, instance.PIDScope()), nil)
	out, err := stop(g.dir)
	if err == nil {
		t.Fatalf("stop succeeded with %q and the process still running", out)
	}
	if want := fmt.Sprintf("PID %d is still running", pid); !strings.Contains(err.Error(), want) {
		t.Errorf("error = %q, want it to contain %q", err, want)
	}
}

// TestStopUnconfirmed is #998 and ADR 0064: when this host cannot watch the
// Instance's pid, stop still asks it to shut down, then fails and says the
// exit is unconfirmed, without taking the pid, here a live process, for
// the Instance's.
func TestStopUnconfirmed(t *testing.T) {
	for name, tc := range map[string]struct{ info, why string }{
		"another PID scope": {infoBody(os.Getpid(), "pid:[1] boot"), "Containerized Instance"},
		// stop runs in the container, and ends with it (#998).
		"its own PID 1": {infoBody(1, instance.PIDScope()), "Containerized Instance"},
		"no pid":        {infoBody(0, instance.PIDScope()), "Containerized Instance"},
		// An Instance too old to report one may be on this host or not.
		"no PID scope": {fmt.Sprintf(`{"id":"want","pid":%d}`, os.Getpid()), "upgrade or restart it"},
	} {
		t.Run(name, func(t *testing.T) {
			g := newStopGateway(t, tc.info, nil)
			start := time.Now()
			out, err := stop(g.dir)
			if err == nil || !strings.Contains(err.Error(), "unconfirmed") || !strings.Contains(err.Error(), tc.why) {
				t.Errorf("stop = %v; want an error saying the exit is unconfirmed, and %q", err, tc.why)
			}
			if d := time.Since(start); d > 10*time.Second {
				t.Errorf("stop took %v; want no wait for a process it cannot watch", d)
			}
			if strings.Contains(out, "stopped") {
				t.Errorf("stop printed %q; want no claim that it stopped", out)
			}
			if n := g.shutdowns.Load(); n != 1 {
				t.Errorf("stop asked for %d shutdowns; want 1", n)
			}
		})
	}
}

// TestStopAnotherInstance is #998: a port that another Instance took since
// the data dir's endpoints were written is left alone.
func TestStopAnotherInstance(t *testing.T) {
	g := newStopGateway(t, `{"id":"other","pid":1}`, nil)
	if _, err := stop(g.dir); err == nil || !strings.Contains(err.Error(), "other") {
		t.Errorf("stop = %v; want an error naming the other Instance", err)
	}
	if n := g.shutdowns.Load(); n != 0 {
		t.Errorf("stop asked for %d shutdowns; want 0", n)
	}
}

// TestStopRefused: an Instance that refuses the shutdown has not accepted
// it, and one that no longer answers, as for a stop that lost a race, is
// not confirmed to have exited.
func TestStopRefused(t *testing.T) {
	t.Run("refused", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost {
				http.Error(w, "bad token", http.StatusUnauthorized)
				return
			}
			_, _ = io.WriteString(w, infoBody(os.Getpid(), instance.PIDScope()))
		}))
		defer srv.Close()
		dir := t.TempDir()
		writeDataFile(t, dir, instance.EndpointsFile, `{"gateway":"`+strings.TrimPrefix(srv.URL, "http://")+`"}`)
		start := time.Now()
		if _, err := stop(dir); err == nil || !strings.Contains(err.Error(), "401") {
			t.Errorf("stop = %v; want the refusal", err)
		}
		if d := time.Since(start); d > 10*time.Second {
			t.Errorf("stop took %v to report a refusal; want no wait", d)
		}
	})
	t.Run("no answer", func(t *testing.T) {
		srv := httptest.NewServer(http.NotFoundHandler())
		addr := strings.TrimPrefix(srv.URL, "http://")
		srv.Close()
		dir := t.TempDir()
		writeDataFile(t, dir, instance.EndpointsFile, `{"gateway":"`+addr+`"}`)
		if out, err := stop(dir); err == nil {
			t.Errorf("stop succeeded with %q; want an error", out)
		}
	})
}
