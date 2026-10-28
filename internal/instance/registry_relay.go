package instance

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
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
// again after Reset, which removes it and the VM relay it forwards
// through.

// RegistryRelayState is the Registry relay's state, as GET
// /_emu/v1/registry-relay reports it.
type RegistryRelayState string

const (
	RegistryRelayOff      RegistryRelayState = "off"
	RegistryRelayStarting RegistryRelayState = "starting"
	RegistryRelayRunning  RegistryRelayState = "running"
	// RegistryRelaySkipped: the port is taken inside the VM.
	RegistryRelaySkipped RegistryRelayState = "skipped"
	RegistryRelayFailed  RegistryRelayState = "failed"
)

// RegistryRelayStatus is what GET /_emu/v1/registry-relay reports.
type RegistryRelayStatus struct {
	State RegistryRelayState `json:"state"`
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
	r := &registryRelay{st: RegistryRelayStatus{State: RegistryRelayOff, Reason: "not started"}}
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

// registryRelayDeps is what a Registry relay start needs from the
// Instance.
type registryRelayDeps struct {
	// registry is the registry's host endpoint, "" when it isn't running.
	registry func() string
	// runtime connects to the container runtime.
	runtime func(ctx context.Context) (*runtime.Manager, error)
	// probe guesses whether the runtime's host network is a VM's from the
	// default bridge network, before the services network exists, so that
	// an Instance elsewhere creates none.
	probe func(ctx context.Context, rt *runtime.Manager) vmrelay.Decision
	// target is the registry's address on the services network, and the
	// netplane's own VM relay decision, made with that network.
	target func(ctx context.Context) (string, vmrelay.Decision, error)
	// start starts the relay container.
	start func(ctx context.Context, rt *runtime.Manager, o registryrelay.Options) error
	log   *slog.Logger
}

// startRegistryRelay starts the Registry relay if the Instance needs one
// and reports what happened. It asks for the registry first, so that an
// Instance without one never connects to the runtime for it.
func startRegistryRelay(ctx context.Context, d registryRelayDeps) RegistryRelayStatus {
	reg := d.registry()
	if reg == "" {
		return RegistryRelayStatus{State: RegistryRelayOff, Reason: "Artifact Registry is not running"}
	}
	_, ps, err := net.SplitHostPort(reg)
	port, perr := strconv.Atoi(ps)
	if err != nil || perr != nil {
		return RegistryRelayStatus{State: RegistryRelayFailed, Reason: "the registry endpoint " + reg + " has no port"}
	}
	rt, err := d.runtime(ctx)
	if errors.Is(err, emu.ErrNoRuntime) {
		return RegistryRelayStatus{State: RegistryRelayOff, Reason: "no container runtime"}
	}
	if err != nil {
		return RegistryRelayStatus{State: RegistryRelayFailed, Reason: "connecting to the container runtime: " + err.Error()}
	}
	if rt.Self() != "" {
		return RegistryRelayStatus{State: RegistryRelayOff, Reason: "a Containerized Instance has no Registry relay yet (#835)"}
	}
	if off := vmSideOff(d.probe(ctx, rt)); off != "" {
		return RegistryRelayStatus{State: RegistryRelayOff, Reason: off}
	}
	listen := net.JoinHostPort("127.0.0.1", ps)
	st := RegistryRelayStatus{Listen: listen}
	if rt.Info.Name == "podman" {
		st.Podman = podmanStep(ps)
	}
	target, dec, err := d.target(ctx)
	if err != nil {
		st.State, st.Reason = RegistryRelayFailed, "the registry on the services network: "+err.Error()
		return st
	}
	if off := vmSideOff(dec); off != "" {
		return RegistryRelayStatus{State: RegistryRelayOff, Reason: off}
	}
	err = d.start(ctx, rt, registryrelay.Options{Port: port, Target: target, Log: d.log})
	switch {
	case errors.Is(err, registryrelay.ErrPortTaken):
		st.State, st.Reason = RegistryRelaySkipped, fmt.Sprintf("%s is taken inside the runtime's VM", listen)
		st.Fix = fmt.Sprintf("stop whatever listens on %s in the VM (a container with host networking), or start with --port ar=PORT, then restart the Instance", listen)
	case err != nil:
		st.State, st.Reason = RegistryRelayFailed, err.Error()
	default:
		st.State = RegistryRelayRunning
	}
	return st
}

// vmSideOff says why VM-side clients need no Registry relay under the VM
// relay decision d, or "" when they need one: only a runtime in a VM,
// with the VM relay on, does.
func vmSideOff(d vmrelay.Decision) string {
	switch {
	case !d.VM:
		return "the runtime's daemon shares the host's loopback"
	case !d.On:
		return "the VM relay is " + d.String()
	}
	return ""
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
		r.set(RegistryRelayStatus{State: RegistryRelayOff, Reason: "turned off for tests"})
		return
	}
	r.set(RegistryRelayStatus{State: RegistryRelayStarting})
	r.wg.Go(func() {
		r.run.Lock()
		defer r.run.Unlock()
		log := in.Env.Log.With("component", "registryrelay")
		st := startRegistryRelay(r.ctx, registryRelayDeps{
			registry: func() string { return in.Env.Endpoints.Get("ar") },
			runtime:  in.containers.Runtime,
			probe: func(ctx context.Context, rt *runtime.Manager) vmrelay.Decision {
				return vmrelay.Decide(false, vmrelay.DefaultGateway(ctx, rt.Client))
			},
			target: func(ctx context.Context) (string, vmrelay.Decision, error) {
				plane, err := in.containers.Netplane(ctx)
				if err != nil {
					return "", vmrelay.Decision{}, err
				}
				addr, err := plane.Addr(ctx, "ar")
				if err != nil {
					return "", vmrelay.Decision{}, err
				}
				d, _ := plane.RelayDecision()
				return addr, d, nil
			},
			start: registryrelay.Start,
			log:   log,
		})
		if r.ctx.Err() != nil {
			return
		}
		r.set(st)
		switch st.State {
		case RegistryRelayRunning:
			log.Info("VM-side clients reach the registry at " + st.Listen + " inside the runtime's VM")
			if st.Podman != "" {
				log.Warn("Podman machine pushes to the registry over HTTPS by default; "+st.Podman, "registry", st.Listen)
			}
		case RegistryRelaySkipped:
			log.Warn("the runtime's own docker push can't reach the registry: "+st.Reason, "fix", st.Fix)
		case RegistryRelayFailed:
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
	dir, err := filepath.Abs(in.Dir)
	if err != nil {
		dir = in.Dir
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	cts, err := registryrelay.Leftovers(ctx, cl, dir)
	if err != nil {
		in.Env.Log.Debug("listing leftover Registry relays", "err", err)
		return
	}
	for _, ct := range cts {
		if err := cl.RemoveContainer(ctx, ct.ID, true); err != nil {
			in.Env.Log.Debug("removing a leftover Registry relay", "container", ct.Name, "err", err)
		}
	}
}

// serveRegistryRelay serves GET /_emu/v1/registry-relay.
func (in *Instance) serveRegistryRelay(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, in.registryRelay.status())
}
