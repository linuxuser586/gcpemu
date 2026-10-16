package emutest

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"
)

// ClusterOpTimeout is how long tests wait for a GKE cluster operation. It
// is longer than the emulator's own bring-up waits added up (the k3s admin
// kubeconfig, the API server, the nodes and the add-ons' CRDs: 9m), with
// room for image setup, which has no deadline, so a slow bring-up fails
// with the emulator's error rather than the test's (#396).
const ClusterOpTimeout = 12 * time.Minute

// ClusterSlotsEnv sets how many tests of the user's test binaries may run
// GKE clusters at once (default 2): every k3s server competes for CPU and
// disk, and too many on a CI runner crash the API server (#396).
const ClusterSlotsEnv = "GCPEMU_TEST_CLUSTERS"

const defaultClusterSlots = 2

var slotHolders sync.Map // testing.TB → struct{}

// HoldClusterSlot waits for one of the cluster slots shared by the user's
// test binaries (ClusterSlotsEnv), up to the test's deadline, and holds it
// for the rest of the test. Tests that create GKE clusters call it before
// starting their emulator, and after t.Parallel, so a paused test holds
// none. Calling it again in the same test does nothing; a subtest that
// calls it takes a slot of its own.
func HoldClusterSlot(t testing.TB) {
	t.Helper()
	if _, held := slotHolders.LoadOrStore(t, struct{}{}); held {
		return
	}
	n := defaultClusterSlots
	if v := os.Getenv(ClusterSlotsEnv); v != "" {
		var err error
		if n, err = strconv.Atoi(v); err != nil || n < 1 {
			t.Fatalf("emutest: %s=%q: want a positive number", ClusterSlotsEnv, v)
		}
	}
	paths := make([]string, n)
	for i := range paths {
		paths[i] = filepath.Join(os.TempDir(), fmt.Sprintf("gcpemu-test-cluster-%d.lock", i))
	}
	release, err := acquireLock(paths, testDeadline(t))
	if errors.Is(err, errLockHeld) {
		err = fmt.Errorf("all %d cluster slots are still held by other tests (%s sets how many)", n, ClusterSlotsEnv)
	}
	if err != nil {
		slotHolders.Delete(t)
		t.Fatalf("emutest: %v", err)
	}
	t.Cleanup(func() {
		release()
		slotHolders.Delete(t)
	})
}

// testDeadline is t's deadline, or zero when it has none.
func testDeadline(t testing.TB) time.Time {
	var deadline time.Time
	if d, ok := t.(interface{ Deadline() (time.Time, bool) }); ok {
		deadline, _ = d.Deadline()
	}
	return deadline
}
