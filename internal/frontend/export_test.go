package frontend

import (
	"context"
	"net"
	"time"
)

// LANRelay is the LAN front-end relay as StartLAN uses it.
type LANRelay = lanRelay

// LANHooks replace what StartLAN asks the host and the container runtime.
type LANHooks struct {
	GOOS string
	// Containerized decides whether the Instance is a Containerized
	// Instance; nil asks emu.Env.Containerized.
	Containerized func(ctx context.Context) bool
	// Listen binds a --gfe-bind address.
	Listen func(network, addr string) (net.Listener, error)
	// Runtime reports whether the runtime is a VM runtime, or why it
	// can't be used.
	Runtime func(ctx context.Context) (vm bool, err error)
	// Relay makes the LAN front-end relay.
	Relay func() LANRelay
	// Retry are the waits between runtime checks.
	Retry []time.Duration
}

// SetLANHooks installs h on f before StartLAN.
func SetLANHooks(f *Frontend, h LANHooks) {
	f.goos = h.GOOS
	if h.Containerized != nil {
		f.containerized = h.Containerized
	}
	f.listen = h.Listen
	f.relayRuntime = h.Runtime
	f.newRelay = h.Relay
	f.relayRetry = h.Retry
}
