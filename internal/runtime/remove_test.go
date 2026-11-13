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

const inProgressMsg = "removal of container x is already in progress"

// reply is one scripted daemon answer.
type reply struct {
	status int
	msg    string
}

// scriptDaemon fakes the container DELETE and inspect (GET …/json): each
// answers from its own script, repeating the last entry once the script
// runs out. It returns the client and the DELETE and inspect counts.
func scriptDaemon(t *testing.T, del, inspect []reply) (*Client, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	var nd, ni atomic.Int32
	answer := func(w http.ResponseWriter, script []reply, n int32) {
		r := script[min(int(n), len(script))-1]
		w.WriteHeader(r.status)
		if r.msg != "" {
			_, _ = io.WriteString(w, `{"message":`+strconv.Quote(r.msg)+`}`)
		}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/containers/"):
			answer(w, del, nd.Add(1))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/json"):
			answer(w, inspect, ni.Add(1))
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	return &Client{http: srv.Client(), base: srv.URL + "/" + apiVersion, Endpoint: srv.URL}, &nd, &ni
}

// fastAwaitGone shortens the wait for an in-progress removal for one test.
func fastAwaitGone(t *testing.T, limit time.Duration) {
	t.Helper()
	poll, lim := awaitGonePoll, awaitGoneLimit
	awaitGonePoll, awaitGoneLimit = 10*time.Millisecond, limit
	t.Cleanup(func() { awaitGonePoll, awaitGoneLimit = poll, lim })
}

var (
	gone       = reply{404, "No such container: x"}
	present    = reply{200, ""}
	inProgress = reply{409, inProgressMsg}
	exitRace   = reply{500, exitRaceMsg}
)

// TestRemoveContainerAwaitsInProgress: another remove of the same container
// is under way (#1148); waiting until it is gone is success, with no second
// DELETE.
func TestRemoveContainerAwaitsInProgress(t *testing.T) {
	fastAwaitGone(t, 5*time.Second)
	c, nd, ni := scriptDaemon(t, []reply{inProgress}, []reply{present, present, gone})
	if err := c.RemoveContainer(context.Background(), "x", true); err != nil {
		t.Fatalf("RemoveContainer: %v", err)
	}
	if d, i := nd.Load(), ni.Load(); d != 1 || i != 3 {
		t.Fatalf("daemon saw %d DELETEs and %d inspects, want 1 and 3", d, i)
	}
}

// TestRemoveContainerInProgressNeverGone: a removal that never finishes is
// returned, with the runtime's message, once the wait runs out.
func TestRemoveContainerInProgressNeverGone(t *testing.T) {
	fastAwaitGone(t, 100*time.Millisecond)
	c, _, _ := scriptDaemon(t, []reply{inProgress}, []reply{present})
	err := c.RemoveContainer(context.Background(), "x", true)
	if err == nil || !strings.Contains(err.Error(), "already in progress") {
		t.Fatalf("RemoveContainer = %v, want the runtime's in-progress error", err)
	}
}

// TestRemoveContainerInProgressHonoursContext: the caller's deadline ends
// the wait before the bound does.
func TestRemoveContainerInProgressHonoursContext(t *testing.T) {
	fastAwaitGone(t, time.Minute)
	c, _, _ := scriptDaemon(t, []reply{inProgress}, []reply{present})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := c.RemoveContainer(ctx, "x", true)
	if err == nil || !strings.Contains(err.Error(), "already in progress") {
		t.Fatalf("RemoveContainer = %v, want the runtime's in-progress error", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("RemoveContainer took %v after the context ended", d)
	}
}

// TestRemoveContainerExitRaceThenInProgress: the retry of an exit race can
// find the first remove still under way; that is waited out too.
func TestRemoveContainerExitRaceThenInProgress(t *testing.T) {
	fastAwaitGone(t, 5*time.Second)
	c, nd, _ := scriptDaemon(t, []reply{exitRace, inProgress}, []reply{gone})
	if err := c.RemoveContainer(context.Background(), "x", true); err != nil {
		t.Fatalf("RemoveContainer: %v", err)
	}
	if d := nd.Load(); d != 2 {
		t.Fatalf("daemon saw %d DELETEs, want 2", d)
	}
}

// TestRemoveContainerOtherConflictNotAwaited: other 409s (a running
// container without force, say) are returned after one DELETE.
func TestRemoveContainerOtherConflictNotAwaited(t *testing.T) {
	msg := "cannot remove container x: container is running: stop the container before removing or force remove"
	c, nd, ni := scriptDaemon(t, []reply{{409, msg}}, []reply{gone})
	err := c.RemoveContainer(context.Background(), "x", false)
	if err == nil || !strings.Contains(err.Error(), "container is running") {
		t.Fatalf("RemoveContainer = %v, want the runtime's conflict", err)
	}
	if d, i := nd.Load(), ni.Load(); d != 1 || i != 0 {
		t.Fatalf("daemon saw %d DELETEs and %d inspects, want 1 and 0", d, i)
	}
}
