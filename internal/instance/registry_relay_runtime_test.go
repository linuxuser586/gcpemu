package instance_test

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/linuxuser586/gcpemu/emutest"
	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/registryrelay"
	"github.com/linuxuser586/gcpemu/internal/runtime"
	"github.com/linuxuser586/gcpemu/internal/vmrelay"
)

// TestStartRemovesLeftoverRegistryRelay: a Registry relay that a previous
// run of an Instance left behind, found by its data dir, is gone once the
// Instance has started (ADR 0043). Start removes it before the registry
// binds its host port, which on OrbStack and Colima the leftover would
// otherwise hold.
func TestStartRemovesLeftoverRegistryRelay(t *testing.T) {
	emutest.RequireRuntime(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cl, err := runtime.Detect()
	if err != nil {
		t.Fatal(err)
	}
	if cl.Self() != "" {
		t.Skip("the emulator runs in a container: no Registry relay")
	}
	dir, err := filepath.Abs(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m := &runtime.Manager{Client: cl, InstanceID: "leftoverrelay", Dir: dir, Ephemeral: true, Log: slog.Default()}
	t.Cleanup(func() { _ = m.Cleanup(context.Background(), true) })
	if err := m.EnsureImage(ctx, vmrelay.Image, false); err != nil {
		t.Fatal(err)
	}
	id, err := m.CreateContainer(ctx, runtime.ContainerSpec{
		Name: m.Name(registryrelay.Suffix), Image: vmrelay.Image, Entrypoint: []string{"sleep", "300"},
		Labels: m.Labels("core", registryrelay.Suffix, "relay"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.StartContainer(ctx, id); err != nil {
		t.Fatal(err)
	}

	inst := emutest.Start(t, []string{"ar"}, emutest.WithTestDataDir(t, dir),
		emutest.WithConfig(func(c *config.Config) { c.NoRegistryRelay = false }))
	defer inst.Close(context.Background())
	if _, err := cl.InspectContainer(ctx, id); !errors.Is(err, runtime.ErrNotFound) {
		t.Errorf("inspecting the leftover Registry relay after start: %v; want not found", err)
	}
}
