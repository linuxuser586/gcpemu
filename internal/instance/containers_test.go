package instance

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
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
	if err := runtime.Unusable(); err != nil {
		t.Skip(err)
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

// TestReadyReportsDisruptedContainers is #318: when the runtime stops or
// restarts a Managed container behind a running Instance's back (a daemon
// restart, `docker stop`, `docker restart`), /_emu/v1/ready fails and
// names it, so `status --ready` does too.
func TestReadyReportsDisruptedContainers(t *testing.T) {
	if testing.Short() {
		t.Skip("needs a container runtime")
	}
	if err := runtime.Unusable(); err != nil {
		t.Skip(err)
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

	cfg := config.Defaults()
	cfg.DataDir = t.TempDir()
	cfg.Ephemeral = true
	cfg.Services = []string{"dns"}
	cfg.Ports[config.AllPorts] = 0
	in, err := New(&cfg, map[string]Factory{"dns": func(*emu.Env) emu.Service { return &fakeSvc{} }}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if err := in.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer in.Shutdown(context.Background())
	rt, err := in.Env.Containers.Runtime(ctx)
	if err != nil {
		t.Fatal(err)
	}
	run := func(resource string) (name, id string) {
		t.Helper()
		name = rt.Name("sql", resource)
		id, err := rt.CreateContainer(ctx, runtime.ContainerSpec{Name: name, Image: image, Cmd: []string{"sleep", "300"},
			Labels: rt.Labels("sql", resource, "")})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = cl.RemoveContainer(context.Background(), id, true) })
		if err := rt.StartContainer(ctx, id); err != nil {
			t.Fatal(err)
		}
		return name, id
	}
	stopped, stoppedID := run("stopped")
	restarted, restartedID := run("restarted")
	_, ownID := run("own")

	// ready polls past the cache of the last answer.
	ready := func(want int) string {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for {
			resp, err := http.Get("http://" + in.Env.Endpoints.Get("gateway") + "/_emu/v1/ready")
			if err != nil {
				t.Fatal(err)
			}
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode == want || time.Now().After(deadline) {
				if resp.StatusCode != want {
					t.Fatalf("ready = %d %s, want %d", resp.StatusCode, b, want)
				}
				return string(b)
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
	ready(http.StatusOK)
	// The Instance's own stops and restarts are no disruption.
	if err := rt.RestartContainer(ctx, ownID, time.Second); err != nil {
		t.Fatal(err)
	}
	if err := rt.StopContainer(ctx, ownID, time.Second); err != nil {
		t.Fatal(err)
	}
	time.Sleep(disruptedTTL)
	ready(http.StatusOK)

	// cl stands in for `docker stop` and `docker restart`.
	if err := cl.StopContainer(ctx, stoppedID, time.Second); err != nil {
		t.Fatal(err)
	}
	if err := cl.RestartContainer(ctx, restartedID, time.Second); err != nil {
		t.Fatal(err)
	}
	body := ready(http.StatusServiceUnavailable)
	if !strings.Contains(body, `"containers"`) || !strings.Contains(body, stopped+" (exited)") || !strings.Contains(body, restarted+" (restarted)") {
		t.Fatalf("ready with disrupted containers = %s", body)
	}
}
