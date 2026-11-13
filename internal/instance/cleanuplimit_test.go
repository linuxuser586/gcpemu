package instance

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/linuxuser586/gcpemu/internal/emu"
)

// cleanupWarning is what the Instance logs when the runtime's Cleanup could
// not finish (#1031).
const cleanupWarning = "could not confirm its containers were removed"

// shortCleanup shortens the limit on removing containers for the test.
func shortCleanup(t *testing.T, d time.Duration) {
	t.Helper()
	old := cleanupTime
	cleanupTime = d
	t.Cleanup(func() { cleanupTime = old })
}

// connect connects in to f, which answers container lists from then on.
func connect(t *testing.T, in *Instance, f *hungRuntime) {
	t.Helper()
	f.answerLists.Store(true)
	if _, err := in.containers.Runtime(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// connectThenHang connects in to f, then has f hang every container list.
func connectThenHang(t *testing.T, in *Instance, f *hungRuntime) {
	t.Helper()
	connect(t, in, f)
	f.answerLists.Store(false)
}

// warned is how many times logs warn that containers may be left.
func warned(logs *syncBuffer) int { return strings.Count(logs.String(), cleanupWarning) }

// TestShutdownOnAHungRuntimeEndsWithinTheRemovalLimit is #1031: a connected
// runtime that hangs holds Shutdown for no longer than its deadline plus the
// limit on removing containers, the error names the step, and the Instance warns once
// that its containers may be left.
func TestShutdownOnAHungRuntimeEndsWithinTheRemovalLimit(t *testing.T) {
	shortCleanup(t, 300*time.Millisecond)
	in, _, f, logs := startOnHungRuntimeLogged(t)
	connectThenHang(t, in, f)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	began := time.Now()
	err := in.Shutdown(ctx)
	if took := time.Since(began); took > 2*time.Second {
		t.Fatalf("Shutdown took %v", took)
	}
	if err == nil || !strings.Contains(err.Error(), "list containers") {
		t.Errorf("Shutdown: %v, want the container list named", err)
	}
	if n := warned(logs); n != 1 {
		t.Errorf("warning logged %d times, want once:\n%s", n, logs.String())
	}
}

// shortRequestLimits scales a requested shutdown's limits down for the test.
func shortRequestLimits(t *testing.T, startWait, teardown, cleanupBy time.Duration) {
	t.Helper()
	oldWait, oldTeardown, oldBy := requestStartWait, requestTeardownTime, requestCleanupBy
	requestStartWait, requestTeardownTime, requestCleanupBy = startWait, teardown, cleanupBy
	t.Cleanup(func() { requestStartWait, requestTeardownTime, requestCleanupBy = oldWait, oldTeardown, oldBy })
}

// TestARequestedShutdownOnAHungRuntimeEndsByItsRemovalLimit is #1031: with
// the Start wait and Teardown both using their full limits, a requested
// shutdown on a hung runtime still closes Done by requestCleanupBy, and
// removing containers gets the time left before it.
func TestARequestedShutdownOnAHungRuntimeEndsByItsRemovalLimit(t *testing.T) {
	shortCleanup(t, 10*time.Second)
	shortRequestLimits(t, 150*time.Millisecond, 300*time.Millisecond, 900*time.Millisecond)
	stuck, release := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() { close(release) })
	svc := &scriptedSvc{name: "dns", journal: &journal{},
		start: func(context.Context) error {
			close(stuck)
			<-release
			return nil
		},
		stop: func(ctx context.Context) { <-ctx.Done() },
	}
	f := &hungRuntime{release: make(chan struct{})}
	in, logs := newOnRuntime(t, f, func(*emu.Env) emu.Service { return svc })
	connectThenHang(t, in, f)
	go func() { _ = in.Start(context.Background()) }()
	<-stuck
	began := time.Now()
	in.RequestShutdown()
	waitDone(t, in)
	took := time.Since(began)
	// Removing containers began after 450ms and ran until 900ms.
	if took < 800*time.Millisecond || took > 2*time.Second {
		t.Errorf("Done closed after %v, want about 900ms", took)
	}
	if n := warned(logs); n != 1 {
		t.Errorf("warning logged %d times, want once:\n%s", n, logs.String())
	}
}

// TestShutdownWithAnEndedCtxStillRemovesContainers: removing them has a limit
// of its own, so a shutdown ctx that has already ended still removes the Instance's
// containers on a healthy runtime.
func TestShutdownWithAnEndedCtxStillRemovesContainers(t *testing.T) {
	in, _, f, logs := startOnHungRuntimeLogged(t)
	connect(t, in, f)
	f.listed.Store(`[{"Id":"c1"}]`)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = in.Shutdown(ctx)
	if n := f.removes.Load(); n != 1 {
		t.Errorf("%d containers removed, want 1", n)
	}
	if warned(logs) != 0 {
		t.Errorf("warned after a Cleanup that finished:\n%s", logs.String())
	}
}

// TestARequestedShutdownGivesASlowRuntimeTheWholeRemovalLimit: a healthy
// runtime that needs longer than the Start wait and Teardown leave over
// before requestCleanupBy, but within cleanupTime, has its containers
// removed when earlier steps finished quickly, and nothing is warned of.
func TestARequestedShutdownGivesASlowRuntimeTheWholeRemovalLimit(t *testing.T) {
	// Scaled from 15s, 30s, 55s and 30s: 200ms are left over, and the
	// removal takes 500ms of cleanupTime's 800ms.
	shortCleanup(t, 800*time.Millisecond)
	shortRequestLimits(t, 300*time.Millisecond, 600*time.Millisecond, 1100*time.Millisecond)
	in, _, f, logs := startOnHungRuntimeLogged(t)
	connect(t, in, f)
	f.listed.Store(`[{"Id":"c1"}]`)
	f.removeDelay.Store(int64(500 * time.Millisecond))
	in.RequestShutdown()
	waitDone(t, in)
	if n := f.removes.Load(); n != 1 {
		t.Errorf("%d containers removed, want 1", n)
	}
	if warned(logs) != 0 {
		t.Errorf("warned after a Cleanup that finished:\n%s", logs.String())
	}
}
