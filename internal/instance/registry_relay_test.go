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
// relay only on a VM runtime with the VM relay on and the registry
// running, on 127.0.0.1 at the registry's own port, forwarding to the
// registry's services-network address; a taken port skips it.
func TestStartRegistryRelay(t *testing.T) {
	cl, err := runtime.New("tcp://127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}
	docker := &runtime.Manager{Client: cl, Info: runtime.Info{Name: "docker"}}
	podman := &runtime.Manager{Client: cl, Info: runtime.Info{Name: "podman"}}
	vm := vmrelay.Decision{On: true, VM: true, Reason: "gateway not bindable"}

	tests := []struct {
		name     string
		rt       *runtime.Manager
		rtErr    error
		registry string
		dec      vmrelay.Decision
		startErr error
		want     string // state
		started  bool
		reason   string // in Reason
	}{
		{name: "VM runtime", rt: docker, registry: "127.0.0.1:4561", dec: vm, want: relayRunning, started: true},
		{name: "moved registry port", rt: docker, registry: "127.0.0.1:5000", dec: vm, want: relayRunning, started: true},
		{name: "no runtime", rtErr: emu.NoRuntime(errors.New("no socket")), registry: "127.0.0.1:4561", dec: vm, want: relayOff, reason: "no container runtime"},
		{name: "runtime error", rtErr: errors.New("timeout"), registry: "127.0.0.1:4561", dec: vm, want: relayFailed, reason: "timeout"},
		{name: "no registry", rt: docker, dec: vm, want: relayOff, reason: "Artifact Registry is not running"},
		{name: "Docker on Linux", rt: docker, registry: "127.0.0.1:4561", dec: vmrelay.Decision{Reason: "gateway bindable"}, want: relayOff, reason: "shares the host's loopback"},
		{name: "VM relay disabled", rt: docker, registry: "127.0.0.1:4561", dec: vmrelay.Decision{VM: true, Forced: true, Reason: "disabled by GCPEMU_VM_RELAY"}, want: relayOff, reason: "disabled by GCPEMU_VM_RELAY"},
		{name: "port taken in the VM", rt: docker, registry: "127.0.0.1:4561", dec: vm, startErr: registryrelay.ErrPortTaken, want: relaySkipped, started: true, reason: "127.0.0.1:4561 is taken"},
		{name: "start fails", rt: docker, registry: "127.0.0.1:4561", dec: vm, startErr: errors.New("boom"), want: relayFailed, started: true, reason: "boom"},
		{name: "Podman machine", rt: podman, registry: "127.0.0.1:4561", dec: vm, want: relayRunning, started: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got *registryrelay.Options
			st := startRegistryRelay(context.Background(), relayDeps{
				runtime:  func(context.Context) (*runtime.Manager, error) { return tt.rt, tt.rtErr },
				registry: func() string { return tt.registry },
				decide:   func(context.Context, *runtime.Manager) vmrelay.Decision { return tt.dec },
				target:   func(context.Context) (string, error) { return "10.89.0.2:4561", nil },
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
			if got.Target != "10.89.0.2:4561" || got.Port == 0 || !strings.HasSuffix(tt.registry, ":"+strconv.Itoa(got.Port)) {
				t.Errorf("started with %+v; want the registry's port and the services-network target", *got)
			}
			if (st.Podman != "") != (tt.rt == podman) {
				t.Errorf("podman step = %q; want one only on Podman", st.Podman)
			}
			if tt.want == relaySkipped && st.Fix == "" {
				t.Error("a skipped relay has no fix")
			}
		})
	}
}
