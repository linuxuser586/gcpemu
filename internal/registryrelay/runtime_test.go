package registryrelay_test

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/linuxuser586/gcpemu/emutest"
	"github.com/linuxuser586/gcpemu/internal/registryrelay"
	"github.com/linuxuser586/gcpemu/internal/runtime"
	"github.com/linuxuser586/gcpemu/internal/vmrelay"
)

// relayPort is the port the tests listen on inside the runtime's host
// network namespace (the VM's on a VM runtime, the host's otherwise).
const relayPort = 45613

func manager(t *testing.T, id string) (context.Context, *runtime.Manager) {
	t.Helper()
	emutest.RequireRuntime(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	cl, err := runtime.Detect()
	if err != nil {
		t.Fatal(err)
	}
	if cl.Self() != "" {
		t.Skip("the emulator runs in a container: no Registry relay")
	}
	info, err := cl.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	m := &runtime.Manager{Client: cl, Info: info, InstanceID: id, Ephemeral: true, Log: slog.Default()}
	t.Cleanup(func() { _ = m.Cleanup(context.Background(), true) })
	return ctx, m
}

// run starts a container in the runtime's host network namespace and
// returns its ID.
func run(t *testing.T, ctx context.Context, m *runtime.Manager, name string, cmd ...string) string {
	t.Helper()
	if err := m.EnsureImage(ctx, vmrelay.Image, false); err != nil {
		t.Fatal(err)
	}
	id, err := m.CreateContainer(ctx, runtime.ContainerSpec{
		Name: m.Name(name), Image: vmrelay.Image, Entrypoint: cmd,
		Labels: m.Labels("test", name, ""), HostNetwork: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.StartContainer(ctx, id); err != nil {
		t.Fatal(err)
	}
	return id
}

// fetch GETs url from a container in the runtime's host network
// namespace: a VM-side client on a VM runtime.
func fetch(t *testing.T, ctx context.Context, m *runtime.Manager, url string) string {
	t.Helper()
	id := run(t, ctx, m, "fetch", "wget", "-T", "5", "-qO-", url)
	defer func() { _ = m.RemoveContainer(context.Background(), id, true) }()
	if _, err := m.WaitContainer(ctx, id); err != nil {
		t.Fatal(err)
	}
	logs, err := m.Logs(ctx, id, 20)
	if err != nil {
		t.Fatal(err)
	}
	return logs
}

// TestRegistryRelayServesVMLoopback: a VM-side client reaches the target,
// a container on an internal network as the services network is, at
// 127.0.0.1 on the relay's port.
func TestRegistryRelayServesVMLoopback(t *testing.T) {
	ctx, m := manager(t, "registryrelayok")
	svc := m.Name("svc")
	if _, err := m.CreateNetwork(ctx, runtime.NetworkSpec{Name: svc, Internal: true, Labels: m.Labels("test", "svc", "network")}); err != nil {
		t.Fatal(err)
	}
	if err := m.EnsureImage(ctx, vmrelay.Image, false); err != nil {
		t.Fatal(err)
	}
	id, err := m.CreateContainer(ctx, runtime.ContainerSpec{
		Name: m.Name("registry"), Image: vmrelay.Image, Entrypoint: []string{"httpd", "-f", "-p", "8080", "-h", "/www"},
		Labels: m.Labels("test", "registry", ""), Networks: []runtime.Attachment{{Network: svc}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.CopyTo(ctx, id, "/", []runtime.File{{Name: "www/v2/index.html", Data: []byte("hello-from-registry")}}); err != nil {
		t.Fatal(err)
	}
	if err := m.StartContainer(ctx, id); err != nil {
		t.Fatal(err)
	}
	ct, err := m.InspectContainer(ctx, id)
	if err != nil {
		t.Fatal(err)
	}

	target := net.JoinHostPort(ct.IPs[svc], "8080")
	if err := registryrelay.Start(ctx, m, registryrelay.Options{Port: relayPort, Target: target, Log: slog.Default()}); err != nil {
		t.Fatal(err)
	}
	if got := fetch(t, ctx, m, "http://127.0.0.1:"+strconv.Itoa(relayPort)+"/v2/"); !strings.Contains(got, "hello-from-registry") {
		t.Errorf("VM-side GET through the Registry relay = %q; want hello-from-registry", got)
	}

	// Starting again replaces the relay, as after a crash.
	if err := registryrelay.Start(ctx, m, registryrelay.Options{Port: relayPort, Target: target, Log: slog.Default()}); err != nil {
		t.Fatalf("second Start: %v", err)
	}
	if err := registryrelay.Remove(ctx, m); err != nil {
		t.Fatal(err)
	}
	if _, err := m.InspectContainer(ctx, m.Name(registryrelay.Suffix)); !errors.Is(err, runtime.ErrNotFound) {
		t.Errorf("after Remove, inspecting the Registry relay: %v; want not found", err)
	}
}

// TestRegistryRelayPortTaken: when something in the runtime's host network
// namespace holds 127.0.0.1 at the port, Start reports ErrPortTaken and
// leaves no Registry relay.
func TestRegistryRelayPortTaken(t *testing.T) {
	ctx, m := manager(t, "registryrelaytaken")
	port := strconv.Itoa(relayPort + 1)
	run(t, ctx, m, "holder", "httpd", "-f", "-p", "127.0.0.1:"+port, "-h", "/")
	// httpd binds as it starts; give it a moment.
	time.Sleep(time.Second)

	n, _ := strconv.Atoi(port)
	err := registryrelay.Start(ctx, m, registryrelay.Options{Port: n, Target: "127.0.0.1:1", Log: slog.Default()})
	if !errors.Is(err, registryrelay.ErrPortTaken) {
		t.Fatalf("Start = %v; want ErrPortTaken", err)
	}
	if _, err := m.InspectContainer(ctx, m.Name(registryrelay.Suffix)); !errors.Is(err, runtime.ErrNotFound) {
		t.Errorf("inspecting the Registry relay after ErrPortTaken: %v; want not found", err)
	}
}
