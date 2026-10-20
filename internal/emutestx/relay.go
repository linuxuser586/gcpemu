// Package emutestx holds test helpers about gcpemu's own internals, for
// gcpemu's tests only: they inspect an emutest.Instance's VM relay.
package emutestx

import (
	"context"
	"strings"
	"testing"

	"github.com/linuxuser586/gcpemu/emutest"
)

// RelayStreams returns the VM relay's per-target stream counts
// (vmrelay.Relay.Streams), or nil when the Instance runs no relay. Tests
// use it to check that the relay carried a path.
func RelayStreams(t testing.TB, inst *emutest.Instance) map[string]int64 {
	t.Helper()
	p, err := inst.Internal().Env.Containers.Netplane(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	r := p.Relay()
	if r == nil {
		return nil
	}
	return r.Streams()
}

// RequireRelayStream fails the test when the Instance runs a VM relay and
// no stream it carried matches: a target equal to key, or ending in key
// when key starts with ":" (a port).
func RequireRelayStream(t testing.TB, inst *emutest.Instance, what, key string) {
	t.Helper()
	st := RelayStreams(t, inst)
	if st == nil {
		return
	}
	for k, n := range st {
		if n > 0 && (k == key || strings.HasPrefix(key, ":") && strings.HasSuffix(k, key)) {
			return
		}
	}
	t.Errorf("the VM relay carried no stream for %s (%s): %v", what, key, st)
}

// VMRuntime reports whether the Instance's container runtime is a VM
// runtime (vmrelay.Decision.VM), whatever GCPEMU_VM_RELAY forces; it is
// false when the emulator runs in a container. It creates the services
// network if it does not exist yet.
func VMRuntime(t testing.TB, inst *emutest.Instance) bool {
	t.Helper()
	ctx := context.Background()
	p, err := inst.Internal().Env.Containers.Netplane(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Services(ctx); err != nil {
		t.Fatal(err)
	}
	d, ok := p.RelayDecision()
	if !ok {
		t.Fatal("emutestx: no VM relay decision after creating the services network")
	}
	return d.VM
}
