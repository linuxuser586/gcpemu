package emutestx

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"testing"

	"github.com/linuxuser586/gcpemu/emutest"
	"github.com/linuxuser586/gcpemu/internal/instance"
	"github.com/linuxuser586/gcpemu/internal/runtime"
)

// hostNetworkClient skips the test without a container runtime and returns
// a client for it.
func hostNetworkClient(t *testing.T) *runtime.Client {
	t.Helper()
	emutest.RequireRuntime(t)
	cl, err := runtime.Detect()
	if err != nil {
		t.Fatal(err)
	}
	return cl
}

// testNetName is a fresh name for a network a test makes.
func testNetName() string { return fmt.Sprintf("gcpemu-test-hostnet-%d", rand.Uint32()) }

// tagged returns what FindTagged lists for the Instance ID that tags the
// network name, failing the test when the network carries none.
func tagged(t *testing.T, cl *runtime.Client, name string) instance.Tagged {
	t.Helper()
	ctx := context.Background()
	n, err := cl.InspectNetwork(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	id := n.Labels[runtime.LabelInstance]
	if id == "" {
		t.Fatalf("network %s carries no %s label: %v", name, runtime.LabelInstance, n.Labels)
	}
	all, err := instance.FindTagged(ctx, cl)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range all {
		if a.ID == id {
			return a
		}
	}
	t.Fatalf("FindTagged does not list Instance ID %s of network %s", id, name)
	return instance.Tagged{}
}

// networkExists reports whether the runtime has a network named name.
func networkExists(t *testing.T, cl *runtime.Client, name string) bool {
	t.Helper()
	_, err := cl.InspectNetwork(context.Background(), name)
	return err == nil
}

// TestHostNetworkTagged: a network HostNetwork makes is tagged through a
// Test data dir that holds its Instance ID under an owner lock the test
// holds, so a reclaim leaves it alone while the test runs (ADR 0076).
func TestHostNetworkTagged(t *testing.T) {
	cl := hostNetworkClient(t)
	t.Parallel()
	name := testNetName()
	if err := HostNetwork(t, cl, runtime.NetworkSpec{Name: name, Labels: map[string]string{"x-test": "kept"}}); err != nil {
		t.Fatal(err)
	}
	n, err := cl.InspectNetwork(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	if n.Labels["x-test"] != "kept" {
		t.Errorf("the caller's label is lost: %v", n.Labels)
	}
	a := tagged(t, cl, name)
	dir := n.Labels[runtime.LabelDir]
	if len(a.Dirs) != 1 || a.Dirs[0] != dir || !filepath.IsAbs(dir) {
		t.Fatalf("Instance ID %s records dirs %v, want the absolute %q", a.ID, a.Dirs, dir)
	}
	if got := instance.ReadID(dir); got != a.ID {
		t.Errorf("dir %s holds ID %q, want %q", dir, got, a.ID)
	}
	if _, err := os.Stat(filepath.Join(dir, instance.OwnerLockFile)); err != nil {
		t.Errorf("dir has no owner lock file: %v", err)
	}
	if len(a.Networks) != 1 || a.Networks[0].Name != name {
		t.Errorf("Instance ID %s tags networks %v, want only %s", a.ID, a.Networks, name)
	}
	reclaimed, err := instance.ReclaimEnded(context.Background(), cl, a, dir)
	if err != nil || reclaimed {
		t.Fatalf("ReclaimEnded while the test holds the lock = %v, %v; want false, nil", reclaimed, err)
	}
	if !networkExists(t, cl, name) {
		t.Fatalf("ReclaimEnded removed network %s while the test holds its lock", name)
	}
}

// TestHostNetworkKilledRun: a run killed before its cleanups leaves the
// network and its dir, whose lock the OS has dropped; ReclaimEnded then
// removes both.
func TestHostNetworkKilledRun(t *testing.T) {
	cl := hostNetworkClient(t)
	t.Parallel()
	name := testNetName()
	t.Cleanup(func() { _ = cl.RemoveNetwork(context.Background(), name) })
	var release func()
	killed := func(_, rel func()) { release = rel } // the network is never removed
	if err := hostNetwork(t, cl, runtime.NetworkSpec{Name: name}, killed); err != nil {
		t.Fatal(err)
	}
	release() // as the OS does when the process dies
	a := tagged(t, cl, name)
	dir := a.Dirs[0]
	reclaimed, err := instance.ReclaimEnded(context.Background(), cl, a, dir)
	if err != nil || !reclaimed {
		t.Fatalf("ReclaimEnded after a killed run = %v, %v; want true, nil", reclaimed, err)
	}
	if networkExists(t, cl, name) {
		t.Errorf("network %s is still there after the reclaim", name)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("dir %s is still there after the reclaim: %v", dir, err)
	}
}

// TestHostNetworkRemovedAtEnd: a test that ends normally removes its
// network.
func TestHostNetworkRemovedAtEnd(t *testing.T) {
	cl := hostNetworkClient(t)
	t.Parallel()
	name := testNetName()
	t.Cleanup(func() { _ = cl.RemoveNetwork(context.Background(), name) })
	t.Run("make", func(t *testing.T) {
		if err := HostNetwork(t, cl, runtime.NetworkSpec{Name: name}); err != nil {
			t.Fatal(err)
		}
	})
	if networkExists(t, cl, name) {
		t.Errorf("network %s is still there after its test ended", name)
	}
}
