package instance

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"testing"

	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/registryrelay"
	"github.com/linuxuser586/gcpemu/internal/runtime"
	"github.com/linuxuser586/gcpemu/internal/vmrelay"
)

// TestStartRegistryRelay: an Instance on the host starts the Registry
// relay only on a VM runtime with the VM relay on, as the netplane decides
// it, and the registry running, on 127.0.0.1 at the registry's own port,
// forwarding to the registry's services-network address; a taken port
// skips it. Without the registry it never connects to the runtime.
func TestStartRegistryRelay(t *testing.T) {
	cl, err := runtime.New("tcp://127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}
	docker := &runtime.Manager{Client: cl, Info: runtime.Info{Name: "docker"}}
	podman := &runtime.Manager{Client: cl, Info: runtime.Info{Name: "podman"}}
	vm := vmrelay.Decision{On: true, VM: true, Reason: "gateway not bindable"}
	bindable := vmrelay.Decision{Reason: "gateway bindable"}

	tests := []struct {
		name      string
		rt        *runtime.Manager
		rtErr     error
		registry  string
		probe     vmrelay.Decision
		plane     *vmrelay.Decision // the netplane's decision; nil: as probe
		startErr  error
		want      RegistryRelayState
		connected bool
		started   bool
		reason    string // in Reason
	}{
		{name: "VM runtime", rt: docker, registry: "127.0.0.1:4561", probe: vm, want: RegistryRelayRunning, connected: true, started: true},
		{name: "moved registry port", rt: docker, registry: "127.0.0.1:5000", probe: vm, want: RegistryRelayRunning, connected: true, started: true},
		{name: "no registry", rt: docker, probe: vm, want: RegistryRelayOff, reason: "Artifact Registry is not running"},
		{name: "no runtime", rtErr: emu.NoRuntime(errors.New("no socket")), registry: "127.0.0.1:4561", probe: vm, want: RegistryRelayOff, connected: true, reason: "no container runtime"},
		{name: "runtime error", rtErr: errors.New("timeout"), registry: "127.0.0.1:4561", probe: vm, want: RegistryRelayFailed, connected: true, reason: "timeout"},
		{name: "Docker on Linux", rt: docker, registry: "127.0.0.1:4561", probe: bindable, want: RegistryRelayOff, connected: true, reason: "shares the host's loopback"},
		{name: "VM relay disabled", rt: docker, registry: "127.0.0.1:4561", probe: vmrelay.Decision{VM: true, Forced: true, Reason: "disabled by GCPEMU_VM_RELAY"}, want: RegistryRelayOff, connected: true, reason: "disabled by GCPEMU_VM_RELAY"},
		{name: "netplane decides otherwise", rt: docker, registry: "127.0.0.1:4561", probe: vm, plane: &bindable, want: RegistryRelayOff, connected: true, reason: "shares the host's loopback"},
		{name: "port taken in the VM", rt: docker, registry: "127.0.0.1:4561", probe: vm, startErr: registryrelay.ErrPortTaken, want: RegistryRelaySkipped, connected: true, started: true, reason: "127.0.0.1:4561 is taken"},
		{name: "start fails", rt: docker, registry: "127.0.0.1:4561", probe: vm, startErr: errors.New("boom"), want: RegistryRelayFailed, connected: true, started: true, reason: "boom"},
		{name: "Podman machine", rt: podman, registry: "127.0.0.1:4561", probe: vm, want: RegistryRelayRunning, connected: true, started: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plane := tt.probe
			if tt.plane != nil {
				plane = *tt.plane
			}
			connected := false
			var got *registryrelay.Options
			st := startRegistryRelay(context.Background(), registryRelayDeps{
				registry: func() string { return tt.registry },
				runtime: func(context.Context) (*runtime.Manager, error) {
					connected = true
					return tt.rt, tt.rtErr
				},
				probe:  func(context.Context, *runtime.Manager) vmrelay.Decision { return tt.probe },
				target: func(context.Context) (string, vmrelay.Decision, error) { return "10.89.0.2:4561", plane, nil },
				start: func(_ context.Context, _ *runtime.Manager, o registryrelay.Options) error {
					got = &o
					return tt.startErr
				},
				log: slog.Default(),
			})
			if st.State != tt.want {
				t.Errorf("state = %q (%s); want %q", st.State, st.Reason, tt.want)
			}
			if !strings.Contains(st.Reason, tt.reason) {
				t.Errorf("reason = %q; want it to contain %q", st.Reason, tt.reason)
			}
			if connected != tt.connected {
				t.Errorf("connected to the runtime = %v; want %v", connected, tt.connected)
			}
			if (got != nil) != tt.started {
				t.Fatalf("started = %v; want %v", got != nil, tt.started)
			}
			if got == nil {
				return
			}
			_, port, _ := strings.Cut(tt.registry, ":")
			if want := "127.0.0.1:" + port; st.Listen != want {
				t.Errorf("listen = %q; want %q", st.Listen, want)
			}
			if got.Target != "10.89.0.2:4561" || port != strconv.Itoa(got.Port) {
				t.Errorf("started with %+v; want the registry's port and the services-network target", *got)
			}
			if (st.Podman != "") != (tt.rt == podman) {
				t.Errorf("podman step = %q; want one only on Podman", st.Podman)
			}
			if tt.want == RegistryRelaySkipped && st.Fix == "" {
				t.Error("a skipped relay has no fix")
			}
		})
	}
}
