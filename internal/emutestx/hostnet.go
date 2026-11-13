package emutestx

import (
	"context"
	"maps"
	"os"
	"path/filepath"
	"testing"

	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/instance"
	"github.com/linuxuser586/gcpemu/internal/runtime"
)

// HostNetwork creates the network spec describes outside any Instance, for
// a test that needs one on the host, such as one that overlaps a range it
// then declares, and removes it when t ends. The network is tagged through
// a Test data dir of its own (ADR 0076): a fresh dir under t.TempDir(),
// whose owner lock t holds, holding a fresh Instance ID that labels the
// network (runtime.LabelInstance) with the dir (runtime.LabelDir), added
// to spec.Labels. A killed run's network is then an Abandoned Instance ID's:
// doctor reports it, and `gcpemu prune` and the emutest sweep of a later
// test process that starts an Instance remove it. A running test's
// network, whose lock is held, is left alone.
//
// It returns the runtime's error unwrapped, so that callers can match it,
// and fails t only when it cannot set up the dir.
func HostNetwork(t testing.TB, cl *runtime.Client, spec runtime.NetworkSpec) error {
	return hostNetwork(t, cl, spec, func(remove, release func()) {
		t.Cleanup(func() { remove(); release() })
	})
}

// hostNetwork is HostNetwork with its end step as a parameter: atEnd is
// handed the step that removes the network and the one that releases the
// dir's owner lock, and arranges for them to run, so a test can simulate
// a killed run by releasing the lock alone.
func hostNetwork(t testing.TB, cl *runtime.Client, spec runtime.NetworkSpec, atEnd func(remove, release func())) error {
	t.Helper()
	dir := t.TempDir()
	release, err := instance.HoldOwnerLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	id := emu.NewIDs(false).Hex(6)
	if err := os.WriteFile(filepath.Join(dir, instance.IDFile), []byte(id), 0o600); err != nil {
		release()
		t.Fatal(err)
	}
	labels := maps.Clone(spec.Labels)
	if labels == nil {
		labels = map[string]string{}
	}
	labels[runtime.LabelInstance] = id
	labels[runtime.LabelDir] = dir
	spec.Labels = labels
	netID, err := cl.CreateNetwork(context.Background(), spec)
	if err != nil && netID == "" {
		release()
		return err
	}
	// CreateNetwork may fail after it has created the network.
	atEnd(func() { _ = cl.RemoveNetwork(context.Background(), spec.Name) }, release)
	return err
}
