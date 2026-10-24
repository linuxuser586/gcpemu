package instance

import (
	"context"
	"maps"
	"net"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/linuxuser586/gcpemu/internal/gateway"
)

// Delegated APIs (ADR 0027): the APIs no Service serves that --external
// forwards to External emulators. The Instance's readiness never depends
// on them; GET /_emu/v1/external reports whether each one answers.

// delegate has the API gateway forward each --external API, failing when a
// Service serves it.
func (in *Instance) delegate() error {
	for _, api := range slices.Sorted(maps.Keys(in.Config.External)) {
		if err := in.gw.Delegate(api, in.Config.External[api]); err != nil {
			return err
		}
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
// its External emulator accepts a TCP connection now.
func (in *Instance) External(ctx context.Context) []ExternalStatus {
	ds := in.gw.DelegatedAPIs()
	out := make([]ExternalStatus, len(ds))
	var wg sync.WaitGroup
	for i, d := range ds {
		out[i].DelegatedAPI = d
		wg.Go(func() {
			dialer := net.Dialer{Timeout: externalDialTimeout}
			c, err := dialer.DialContext(ctx, "tcp", d.Address)
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
