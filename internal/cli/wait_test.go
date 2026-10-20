package cli

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// waitServer is an Admin API whose /_emu/v1/conditions reports each Wait
// condition with the state states gives it on the nth request (from 1).
// A condition states has no entry for is an unknown kind.
func waitServer(t *testing.T, states func(n int64, cond string) (state, reason string, known bool)) (dir string, calls *atomic.Int64) {
	t.Helper()
	calls = &atomic.Int64{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/_emu/v1/conditions" {
			http.NotFound(w, r)
			return
		}
		n := calls.Add(1)
		type cond struct {
			For    string `json:"for"`
			State  string `json:"state"`
			Reason string `json:"reason,omitempty"`
		}
		out := struct {
			Met        bool   `json:"met"`
			Conditions []cond `json:"conditions"`
		}{Met: true}
		for _, f := range r.URL.Query()["for"] {
			st, reason, ok := states(n, f)
			if !ok {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": `Wait condition "` + f + `": unknown kind`})
				return
			}
			out.Met = out.Met && st == "met"
			out.Conditions = append(out.Conditions, cond{f, st, reason})
		}
		_ = json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(srv.Close)
	return writeEndpoints(t, strings.TrimPrefix(srv.URL, "http://")), calls
}

func writeEndpoints(t *testing.T, gateway string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "endpoints.json"), []byte(`{"gateway":"`+gateway+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func runWait(dir string, args ...string) error {
	root := New(nil)
	root.SetOut(io.Discard)
	root.SetArgs(append([]string{"wait", "--config", filepath.Join(dir, "none.yaml"), "--data-dir", dir}, args...))
	return root.Execute()
}

// TestWait is FR-CI-007: `gcpemu wait` returns once every Wait condition
// is met, and fails naming those that are not when one has failed, the
// timeout passes, the request is malformed or no Instance is running.
func TestWait(t *testing.T) {
	const cluster, lbHealth = "gke/cluster=p/us-central1-a/c1", "lb/health=p/global/api"

	t.Run("met once all are met", func(t *testing.T) {
		dir, calls := waitServer(t, func(n int64, c string) (string, string, bool) {
			if c == cluster && n < 3 {
				return "pending", "status PROVISIONING", true
			}
			return "met", "", true
		})
		if err := runWait(dir, "--for", cluster, "--for", lbHealth); err != nil {
			t.Fatal(err)
		}
		if calls.Load() != 3 {
			t.Errorf("returned after %d polls, want 3", calls.Load())
		}
	})

	t.Run("timeout names the pending ones", func(t *testing.T) {
		dir, _ := waitServer(t, func(_ int64, c string) (string, string, bool) {
			if c == cluster {
				return "pending", "not found", true
			}
			return "met", "", true
		})
		start := time.Now()
		err := runWait(dir, "--for", cluster, "--for", lbHealth, "--timeout", "700ms")
		if took := time.Since(start); took < 700*time.Millisecond || took > 3*time.Second {
			t.Errorf("gave up after %v, want about 700ms", took)
		}
		if err == nil || !strings.Contains(err.Error(), cluster+": not found") || strings.Contains(err.Error(), lbHealth) {
			t.Errorf("err = %v, want it to name only %s as not found", err, cluster)
		}
	})

	t.Run("failed straight away", func(t *testing.T) {
		dir, calls := waitServer(t, func(_ int64, c string) (string, string, bool) {
			if c == cluster {
				return "failed", "status ERROR", true
			}
			return "pending", "group zones/z/networkEndpointGroups/neg has no HEALTHY endpoint", true
		})
		err := runWait(dir, "--for", cluster, "--for", lbHealth, "--timeout", "1m")
		if err == nil || !strings.Contains(err.Error(), cluster+": status ERROR") ||
			!strings.Contains(err.Error(), lbHealth+": group zones/z/networkEndpointGroups/neg has no HEALTHY endpoint") {
			t.Errorf("err = %v, want both conditions with their reasons", err)
		}
		if calls.Load() != 1 {
			t.Errorf("polled %d times, want 1", calls.Load())
		}
	})

	t.Run("timeout while a poll hangs", func(t *testing.T) {
		release := make(chan struct{})
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
		t.Cleanup(srv.Close)
		t.Cleanup(func() { close(release) })
		dir := writeEndpoints(t, strings.TrimPrefix(srv.URL, "http://"))
		start := time.Now()
		err := runWait(dir, "--for", cluster, "--timeout", "500ms")
		if took := time.Since(start); took > 2*time.Second {
			t.Errorf("gave up after %v, want about 500ms", took)
		}
		if err == nil || !strings.Contains(err.Error(), "timed out after 500ms") {
			t.Errorf("err = %v, want a timeout", err)
		}
	})

	t.Run("server error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "boom", http.StatusInternalServerError)
		}))
		t.Cleanup(srv.Close)
		err := runWait(writeEndpoints(t, strings.TrimPrefix(srv.URL, "http://")), "--for", cluster)
		if err == nil || strings.Contains(err.Error(), "not running") || !strings.Contains(err.Error(), "500 boom") {
			t.Errorf("err = %v, want the 500", err)
		}
	})

	t.Run("usage", func(t *testing.T) {
		dir, calls := waitServer(t, func(int64, string) (string, string, bool) { return "", "", false })
		for _, args := range [][]string{nil, {"--for", "nonsense"}, {"--for", "gke/cluster=p/c1"}, {"--for", "sql/instance=p/r/db"}} {
			err := runWait(dir, args...)
			if err == nil || !strings.Contains(err.Error(), "usage: gcpemu wait --for KIND=PROJECT/LOCATION/NAME") {
				t.Errorf("wait %q: %v, want a usage error", args, err)
			}
		}
		if calls.Load() != 1 {
			t.Errorf("asked the Instance %d times, want once (for the unknown kind)", calls.Load())
		}
	})

	t.Run("no instance", func(t *testing.T) {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		stale := writeEndpoints(t, l.Addr().String())
		l.Close()
		for name, dir := range map[string]string{"none": t.TempDir(), "stale endpoints": stale} {
			start := time.Now()
			err := runWait(dir, "--for", cluster)
			if err == nil || !strings.Contains(err.Error(), "not running") {
				t.Errorf("%s: %v, want not running", name, err)
			}
			if took := time.Since(start); took > time.Second {
				t.Errorf("%s: took %v", name, took)
			}
		}
	})
}
