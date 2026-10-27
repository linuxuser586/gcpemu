package instance

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/registryrelay"
	"github.com/linuxuser586/gcpemu/internal/runtime"
	"github.com/linuxuser586/gcpemu/internal/vmrelay"
)

// The Registry relay (ADR 0043) gives VM-side clients, such as a VM
// runtime's own `docker push`, the registry at the address Host clients
// use. An Instance on the host starts it in the background once its
// Services have started, so that its host listeners hold the registry's
// port first: OrbStack and Colima forward a port the relay takes in the
// VM to the host, and whichever side binds first keeps it. It starts
// again after Reset, which removes the VM relay it forwards through.

// Registry relay states (RegistryRelayStatus.State).
const (
	relayOff      = "off"
	relayStarting = "starting"
	relayRunning  = "running"
	// relaySkipped: the port is taken inside the VM.
	relaySkipped = "skipped"
	relayFailed  = "failed"
)

// RegistryRelayStatus is what GET /_emu/v1/registry-relay reports.
type RegistryRelayStatus struct {
	State string `json:"state"`
	// Reason says why it is off, skipped or failed.
	Reason string `json:"reason,omitempty"`
	// Listen is the address it listens on inside the runtime's VM.
	Listen string `json:"listen,omitempty"`
	// Fix is what the user can do when it is skipped.
	Fix string `json:"fix,omitempty"`
	// Podman is the step Podman machine users take, as Podman does not
	// treat localhost registries as insecure.
	Podman string `json:"podman,omitempty"`
}

// registryRelay runs the Registry relay's starts.
type registryRelay struct {
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	// run serialises starts: Reset may start one while another runs.
	run sync.Mutex

	mu sync.Mutex
	st RegistryRelayStatus
}

func newRegistryRelay() *registryRelay {
	r := &registryRelay{st: RegistryRelayStatus{State: relayOff, Reason: "not started"}}
	r.ctx, r.cancel = context.WithCancel(context.Background())
	return r
}

func (r *registryRelay) status() RegistryRelayStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.st
}

func (r *registryRelay) set(st RegistryRelayStatus) {
	r.mu.Lock()
	r.st = st
	r.mu.Unlock()
}

// stop ends a start in progress and waits for it, so that none creates a
// container once the Instance's others are removed.
func (r *registryRelay) stop() {
	r.cancel()
	r.wg.Wait()
}

// relayDeps is what a Registry relay start needs from the Instance.
type relayDeps struct {
	// runtime connects to the container runtime.
	runtime func(ctx context.Context) (*runtime.Manager, error)
	// registry is the registry's host endpoint, "" when it isn't running.
	registry func() string
	// decide says whether the runtime's host network is a VM's.
	decide func(ctx context.Context, rt *runtime.Manager) vmrelay.Decision
	// target is the registry's address on the services network, through
	// the VM relay.
	target func(ctx context.Context) (string, error)
	// start starts the relay container.
	start func(ctx context.Context, rt *runtime.Manager, o registryrelay.Options) error
	log   *slog.Logger
}

// startRegistryRelay starts the Registry relay if the Instance needs one
// and reports what happened.
func startRegistryRelay(ctx context.Context, d relayDeps) RegistryRelayStatus {
	rt, err := d.runtime(ctx)
	if errors.Is(err, emu.ErrNoRuntime) {
		return RegistryRelayStatus{State: relayOff, Reason: "no container runtime"}
	}
	if err != nil {
		return RegistryRelayStatus{State: relayFailed, Reason: "connecting to the container runtime: " + err.Error()}
	}
	if rt.Self() != "" {
		return RegistryRelayStatus{State: relayOff, Reason: "a Containerized Instance has no Registry relay yet (#835)"}
	}
	reg := d.registry()
	if reg == "" {
		return RegistryRelayStatus{State: relayOff, Reason: "Artifact Registry is not running"}
	}
	_, ps, err := net.SplitHostPort(reg)
	port, perr := strconv.Atoi(ps)
	if err != nil || perr != nil {
		return RegistryRelayStatus{State: relayFailed, Reason: "the registry endpoint " + reg + " has no port"}
	}
	dec := d.decide(ctx, rt)
	switch {
	case !dec.VM:
		return RegistryRelayStatus{State: relayOff, Reason: "the runtime's daemon shares the host's loopback"}
	case !dec.On:
		return RegistryRelayStatus{State: relayOff, Reason: "the VM relay is " + dec.Reason}
	}
	listen := net.JoinHostPort("127.0.0.1", ps)
	st := RegistryRelayStatus{Listen: listen}
	if rt.Info.Name == "podman" {
		st.Podman = podmanStep(ps)
	}
	target, err := d.target(ctx)
	if err != nil {
		st.State, st.Reason = relayFailed, "the registry on the services network: "+err.Error()
		return st
	}
	err = d.start(ctx, rt, registryrelay.Options{Port: port, Target: target, Log: d.log})
	switch {
	case errors.Is(err, registryrelay.ErrPortTaken):
		st.State, st.Reason = relaySkipped, fmt.Sprintf("%s is taken inside the runtime's VM", listen)
		st.Fix = fmt.Sprintf("stop whatever listens on %s in the VM (a container with host networking), or start with --port ar=PORT, then restart the Instance", listen)
	case err != nil:
		st.State, st.Reason = relayFailed, err.Error()
	default:
		st.State = relayRunning
	}
	return st
}

// podmanStep is what Podman machine users do to push to the registry at
// port: Podman sends localhost registries HTTPS unless told otherwise.
func podmanStep(port string) string {
	return fmt.Sprintf("run `podman push --tls-verify=false ...`, or add `[[registry]]` with `location = \"localhost:%s\"` and `insecure = true` to /etc/containers/registries.conf in the machine (`podman machine ssh`)", port)
}

// startRegistryRelay starts the Registry relay in the background (Start,
// Reset).
func (in *Instance) startRegistryRelay() {
	r := in.registryRelay
	if r.ctx.Err() != nil {
		return
	}
	if in.Config.NoRegistryRelay {
		r.set(RegistryRelayStatus{State: relayOff, Reason: "turned off for tests"})
		return
	}
	r.set(RegistryRelayStatus{State: relayStarting})
	r.wg.Go(func() {
		r.run.Lock()
		defer r.run.Unlock()
		log := in.Env.Log.With("component", "registryrelay")
		st := startRegistryRelay(r.ctx, relayDeps{
			runtime:  in.containers.Runtime,
			registry: func() string { return in.Env.Endpoints.Get("ar") },
			decide: func(ctx context.Context, rt *runtime.Manager) vmrelay.Decision {
				return vmrelay.Decide(false, vmrelay.DefaultGateway(ctx, rt.Client))
			},
			target: func(ctx context.Context) (string, error) {
				plane, err := in.containers.Netplane(ctx)
				if err != nil {
					return "", err
				}
				return plane.Addr(ctx, "ar")
			},
			start: registryrelay.Start,
			log:   log,
		})
		if r.ctx.Err() != nil {
			return
		}
		r.set(st)
		switch st.State {
		case relayRunning:
			log.Info("VM-side clients reach the registry at " + st.Listen + " inside the runtime's VM")
			if st.Podman != "" {
				log.Warn("Podman machine pushes to the registry over HTTPS by default; "+st.Podman, "registry", st.Listen)
			}
		case relaySkipped:
			log.Warn("the runtime's own docker push can't reach the registry: "+st.Reason, "fix", st.Fix)
		case relayFailed:
			log.Warn("the runtime's own docker push can't reach the registry: the Registry relay failed to start", "err", st.Reason)
		default:
			log.Debug("no Registry relay: " + st.Reason)
		}
	})
}

// removeLeftoverRegistryRelay removes a Registry relay a previous run of
// the Instance left behind, before the registry binds its host port: on
// OrbStack and Colima the leftover would hold it on the host. It gives
// up quietly when no runtime answers in time.
func (in *Instance) removeLeftoverRegistryRelay(ctx context.Context) {
	if in.Config.NoRegistryRelay || !in.hasService("ar") {
		return
	}
	cl, err := runtime.Detect()
	if err != nil || cl.Self() != "" {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	name := (&runtime.Manager{InstanceID: in.ID}).Name(registryrelay.Suffix)
	if err := cl.RemoveContainer(ctx, name, true); err != nil {
		in.Env.Log.Debug("removing a leftover Registry relay", "container", name, "err", err)
	}
}

func (in *Instance) hasService(name string) bool {
	for _, s := range in.services {
		if s.Name() == name {
			return true
		}
	}
	return false
}

// serveRegistryRelay serves GET /_emu/v1/registry-relay.
func (in *Instance) serveRegistryRelay(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, in.registryRelay.status())
}
