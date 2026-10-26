package instance

import (
	"context"
	"maps"
	"net"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/gateway"
)

// Delegated APIs (ADR 0027): the APIs no Service serves that --external
// forwards to External emulators. The Instance's readiness never depends
// on one configured by address, only on its Managed emulators (ADR 0029,
// managed.go); GET /_emu/v1/external reports whether each one answers.

// delegate has the API gateway forward each --external API, failing when a
// Service serves it.
func (in *Instance) delegate() error {
	for _, api := range slices.Sorted(maps.Keys(in.Config.External)) {
		if addr := in.Config.External[api]; addr != config.Managed {
			if err := in.gw.Delegate(api, addr); err != nil {
				return err
			}
			continue
		}
		m := newManagedEmulator(api, in.containers)
		if err := in.gw.DelegateManaged(api, m.dial); err != nil {
			return err
		}
		in.managed[api] = m
	}
	return nil
}

// externalEnvVars points the variable the client libraries read for each
// Delegated API's emulator (FIRESTORE_EMULATOR_HOST, SPANNER_EMULATOR_HOST,
// ...) at the API gateway gw.
func (in *Instance) externalEnvVars(out map[string]string, gw string) {
	for _, d := range in.gw.DelegatedAPIs() {
		if v := gateway.EmulatorHostVar(d.API); v != "" {
			out[v] = gw
		}
	}
}

// ExternalStatus is a Delegated API and whether its External emulator
// accepts connections.
type ExternalStatus struct {
	gateway.DelegatedAPI
	Reachable bool   `json:"reachable"`
	Error     string `json:"error,omitempty"`
}

// externalDialTimeout bounds each reachability check.
const externalDialTimeout = 2 * time.Second

// External returns each Delegated API, sorted by API name, with whether
// its External emulator accepts a TCP connection now. A Managed emulator's
// address is its own on the services network, once it runs.
func (in *Instance) External(ctx context.Context) []ExternalStatus {
	ds := in.gw.DelegatedAPIs()
	out := make([]ExternalStatus, len(ds))
	var wg sync.WaitGroup
	for i, d := range ds {
		out[i].DelegatedAPI = d
		wg.Go(func() {
			ctx, cancel := context.WithTimeout(ctx, externalDialTimeout)
			defer cancel()
			var c net.Conn
			var err error
			if m := in.managed[d.API]; d.Managed && m != nil {
				out[i].Address, _ = m.addr()
				c, err = m.dial(ctx, true)
			} else {
				dialer := net.Dialer{}
				c, err = dialer.DialContext(ctx, "tcp", d.Address)
			}
			if err != nil {
				out[i].Error = err.Error()
				return
			}
			c.Close()
			out[i].Reachable = true
		})
	}
	wg.Wait()
	return out
}

// serveExternal is GET /_emu/v1/external.
func (in *Instance) serveExternal(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"external": in.External(r.Context())})
}
