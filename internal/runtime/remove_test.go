package runtime

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const exitRaceMsg = "cannot remove container \"x\": could not kill container: tried to kill container, but did not receive an exit event"

// removeDaemon fakes the container DELETE: the first fail calls answer
// 500 with msg, the rest 204. It returns the client and the call count.
func removeDaemon(t *testing.T, fail int, msg string) (*Client, *atomic.Int32) {
	t.Helper()
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || !strings.Contains(r.URL.Path, "/containers/") {
			w.WriteHeader(404)
			return
		}
		if int(n.Add(1)) <= fail {
			w.WriteHeader(500)
			_, _ = io.WriteString(w, `{"message":`+strconv.Quote(msg)+`}`)
			return
		}
		w.WriteHeader(204)
	}))
	t.Cleanup(srv.Close)
	return &Client{http: srv.Client(), base: srv.URL + "/" + apiVersion, Endpoint: srv.URL}, &n
}

// TestRemoveContainerRetriesExitRace: Docker's kill/exit race on a forced
// remove is transient; the remove is retried and succeeds (#1142).
func TestRemoveContainerRetriesExitRace(t *testing.T) {
	c, n := removeDaemon(t, 1, exitRaceMsg)
	if err := c.RemoveContainer(context.Background(), "x", true); err != nil {
		t.Fatalf("RemoveContainer: %v", err)
	}
	if got := n.Load(); got != 2 {
		t.Fatalf("daemon saw %d DELETEs, want 2", got)
	}
}

// TestRemoveContainerExitRaceGivesUp: a race that never clears is returned
// after the bounded attempts, with the runtime's message.
func TestRemoveContainerExitRaceGivesUp(t *testing.T) {
	c, n := removeDaemon(t, 1<<30, exitRaceMsg)
	err := c.RemoveContainer(context.Background(), "x", true)
	if err == nil || !strings.Contains(err.Error(), "did not receive an exit event") {
		t.Fatalf("RemoveContainer = %v, want the runtime's exit-event error", err)
	}
	if got := n.Load(); got != 3 {
		t.Fatalf("daemon saw %d DELETEs, want 3", got)
	}
}

// TestRemoveContainerOtherServerErrorNotRetried: other runtime faults (such
// as a full disk, #993) are returned at once, not masked by retries.
func TestRemoveContainerOtherServerErrorNotRetried(t *testing.T) {
	c, n := removeDaemon(t, 1<<30, "no space left on device")
	err := c.RemoveContainer(context.Background(), "x", true)
	if err == nil || !strings.Contains(err.Error(), "no space left on device") {
		t.Fatalf("RemoveContainer = %v, want the runtime's disk error", err)
	}
	if got := n.Load(); got != 1 {
		t.Fatalf("daemon saw %d DELETEs, want 1", got)
	}
}

// TestRemoveContainerExitRaceThenGone: a container that is gone by the
// retry (404) was removed; that is success.
func TestRemoveContainerExitRaceThenGone(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			w.WriteHeader(500)
			_, _ = io.WriteString(w, `{"message":`+strconv.Quote(exitRaceMsg)+`}`)
			return
		}
		w.WriteHeader(404)
		_, _ = io.WriteString(w, `{"message":"No such container: x"}`)
	}))
	t.Cleanup(srv.Close)
	c := &Client{http: srv.Client(), base: srv.URL + "/" + apiVersion, Endpoint: srv.URL}
	if err := c.RemoveContainer(context.Background(), "x", true); err != nil {
		t.Fatalf("RemoveContainer: %v", err)
	}
}

// TestRemoveContainerKeepsRuntimeError: when a retry fails another way
// (here the caller's deadline cuts it off), the runtime's original
// exit-race error is what the caller sees.
func TestRemoveContainerKeepsRuntimeError(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			w.WriteHeader(500)
			_, _ = io.WriteString(w, `{"message":`+strconv.Quote(exitRaceMsg)+`}`)
			return
		}
		<-r.Context().Done() // the retry hangs until the client gives up
	}))
	t.Cleanup(srv.Close)
	c := &Client{http: srv.Client(), base: srv.URL + "/" + apiVersion, Endpoint: srv.URL}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := c.RemoveContainer(ctx, "x", true)
	if err == nil || !strings.Contains(err.Error(), "did not receive an exit event") {
		t.Fatalf("RemoveContainer = %v, want the runtime's exit-event error", err)
	}
}
