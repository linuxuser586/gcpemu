package frontend

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/linuxuser586/gcpemu/internal/edgeagent"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/loopback"
)

// The LAN front-end relay (#469): when an unprivileged emulator on Linux
// may not bind port 443 on a --gfe-bind address (ip_unprivileged_port_start
// above it), the gfe-lan Managed container, on the host network with
// NET_BIND_SERVICE, listens there instead. It runs the Edge relay's
// Sidecar agent in LAN mode, as lb-lan does for --lb-bind, and relays each
// connection as plain TCP, with a PROXY v2 header, to an ingress on
// 127.0.0.1: TLS is terminated here, so the instance CA's key never enters
// the container. The ingress closes every connection without the header,
// then serves the --gfe-bind listeners' TLS configuration and handler, so
// a relayed request gets the checks a direct one does, with the LAN
// client's address.
//
// There is no relay off Linux, with --port gfe-lan set, inside a
// container, without a container runtime or on a VM runtime, whose host
// network is the VM's: the address is skipped with the sysctl advice. The
// container lives until the Instance stops; if the runtime stops or
// restarts it, /_emu/v1/ready names it as Disrupted.

// lanRelay is the LAN front-end relay (edgeagent.LANRelay).
type lanRelay interface {
	// Set makes the relay listen on exactly addrs and relay to ingress.
	Set(ctx context.Context, ingress string, addrs []string) error
	// Close removes the relay.
	Close(ctx context.Context)
}

// LANRelayName names the LAN front-end relay's container,
// gcpemu-<id>-gfe-lan.
const LANRelayName = "gfe-lan"

// relayRetry are the waits between runtime checks after a transient
// failure, about as long as the lb Service's backoff (#313).
var relayRetry = []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second}

// relayable reports whether a --gfe-bind address whose bind failed with
// err can be relayed: the bind was refused because the port is privileged,
// on Linux, by an emulator that is not in a container.
func (f *Frontend) relayable(err error) bool {
	return f.goos == "linux" && loopback.RefusedOn(f.goos, err) == loopback.Privileged && !f.inContainer()
}

// startRelay serves addrs through the LAN front-end relay once a usable
// container runtime is found, or skips them. It runs until the relay
// serves or ctx is done.
func (f *Frontend) startRelay(ctx context.Context, srv func() *http.Server, addrs []string) {
	skip := func(err error) {
		for _, a := range addrs {
			f.lanSkipped(a, err)
		}
	}
	vm, err := f.relayRuntime(ctx)
	for i := 0; err != nil && !errors.Is(err, emu.ErrNoRuntime) && i < len(f.relayRetry); i++ {
		f.env.Log.Debug("Google front end: container runtime not reachable for the gfe-lan relay; trying again", "err", err, "retryAfter", f.relayRetry[i])
		select {
		case <-ctx.Done():
			return
		case <-time.After(f.relayRetry[i]):
		}
		vm, err = f.relayRuntime(ctx)
	}
	switch {
	case ctx.Err() != nil:
		return
	case err != nil:
		skip(fmt.Errorf("privileged port and no container runtime to relay it: %w", err))
		return
	case vm:
		skip(errors.New("privileged port, and the gfe-lan relay on a VM runtime would listen in the VM"))
		return
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		skip(fmt.Errorf("gfe-lan relay ingress: %w", err))
		return
	}
	relay := f.newRelay()
	err = relay.Set(ctx, ln.Addr().String(), addrs)
	f.mu.Lock()
	stopped := ctx.Err() != nil || f.closed
	if err == nil && !stopped {
		f.relay = relay
	}
	f.mu.Unlock()
	if err != nil || stopped {
		ln.Close()
		closeRelay(relay)
		if !stopped {
			skip(fmt.Errorf("gfe-lan relay: %w", err))
		}
		return
	}
	f.serveLAN(srv(), edgeagent.ProxiedListener(ln), "relay", addrs...)
}

// closeRelay removes relay, waiting up to 30s.
func closeRelay(relay lanRelay) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	relay.Close(ctx)
}

// stopRelay stops a relay still starting and waits for it.
func (f *Frontend) stopRelay() {
	f.mu.Lock()
	stop, done := f.relayStop, f.relayDone
	f.relayStop, f.relayDone = nil, nil
	f.mu.Unlock()
	if stop != nil {
		stop()
		<-done
	}
}

// containerRuntime reports whether the Instance's container runtime is a
// VM runtime (the VM relay's decision), or an error: one matching
// emu.ErrNoRuntime if it has none for good.
func (f *Frontend) containerRuntime(ctx context.Context) (bool, error) {
	if f.env.Containers == nil {
		return false, emu.ErrNoRuntime
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if _, err := f.env.Containers.Runtime(ctx); err != nil {
		return false, err
	}
	np, err := f.env.Containers.Netplane(ctx)
	if err != nil {
		return false, err
	}
	if _, err := np.Services(ctx); err != nil {
		return false, err
	}
	dec, ok := np.RelayDecision()
	return ok && dec.VM, nil
}

// containerRelay is the gfe-lan Managed container, which the Instance owns
// (service core).
func (f *Frontend) containerRelay() lanRelay {
	return &edgeagent.LANRelay{Runtime: f.env.Containers.Runtime, Name: LANRelayName,
		Service: "core", Resource: LANRelayName, Role: "lan", Offline: f.env.Config.Offline, Log: f.env.Log}
}
