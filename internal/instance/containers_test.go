package instance

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/runtime"
)

// TestReclaimOrphans is FR-CORE-006: containers labelled with the
// instance's ID that a crashed run left behind are removed when the next
// run connects to the container runtime.
func TestReclaimOrphans(t *testing.T) {
	if testing.Short() {
		t.Skip("needs a container runtime")
	}
	cl, err := runtime.Detect()
	if err != nil {
		t.Skipf("no container runtime: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if _, err := cl.Info(ctx); err != nil {
		t.Skipf("container runtime unavailable: %v", err)
	}
	const image = "busybox:1.36.1"
	if err := cl.EnsureImage(ctx, image, false); err != nil {
		t.Skipf("image %s unavailable: %v", image, err)
	}

	dir := t.TempDir()
	id := "orphan" + emu.NewIDs(false).Hex(3)
	if err := os.WriteFile(filepath.Join(dir, IDFile), []byte(id), 0o600); err != nil {
		t.Fatal(err)
	}
	cid, err := cl.CreateContainer(ctx, runtime.ContainerSpec{Name: "gcpemu-" + id + "-orphan", Image: image, Cmd: []string{"sleep", "300"},
		Labels: map[string]string{runtime.LabelInstance: id, runtime.LabelService: "sql"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cl.RemoveContainer(context.Background(), cid, true) })
	if err := cl.StartContainer(ctx, cid); err != nil {
		t.Fatal(err)
	}
	// Another instance's container is left alone.
	other, err := cl.CreateContainer(ctx, runtime.ContainerSpec{Name: "gcpemu-" + id + "x-keep", Image: image, Cmd: []string{"true"},
		Labels: map[string]string{runtime.LabelInstance: id + "x"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cl.RemoveContainer(context.Background(), other, true) })

	cfg := config.Defaults()
	cfg.DataDir = dir
	cfg.Services = []string{"dns"}
	cfg.Ports[config.AllPorts] = 0
	in, err := New(&cfg, map[string]Factory{"dns": func(*emu.Env) emu.Service { return &fakeSvc{} }}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if in.ID != id {
		t.Fatalf("instance ID %q, want the persisted %q", in.ID, id)
	}
	if err := in.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer in.Shutdown(context.Background())
	if _, err := in.Env.Containers.Runtime(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := cl.InspectContainer(ctx, cid); !errors.Is(err, runtime.ErrNotFound) {
		t.Errorf("orphan still exists: %v", err)
	}
	if _, err := cl.InspectContainer(ctx, other); err != nil {
		t.Errorf("another instance's container was removed: %v", err)
	}
}
