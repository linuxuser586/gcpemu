//go:build compat

package compat

import (
	"slices"
	"testing"
)

// flowCases are the steps sdk/flows.py and sdk/flows.js print, in order.
var flowCases = []string{
	"storage-create-bucket", "storage-write", "storage-read", "storage-list", "storage-delete", "storage-gone",
	"pubsub-create-topic", "pubsub-create-subscription", "pubsub-publish", "pubsub-receive", "pubsub-close",
}

// canaryHosts are what the flows' canaries must send to the Escape guard:
// Cloud Storage ignores the universe, Pub/Sub does not.
var canaryHosts = []string{"storage.googleapis.com", "pubsub.gcpemu.invalid"}

// testFlows covers #601 (IF-008): the Python and Node Cloud Storage and
// Pub/Sub libraries write, read, list and delete an object and publish
// and stream-pull a message, configured only by `gcpemu env` and the
// scheme its STORAGE_EMULATOR_HOST leaves out (see the README). Both run
// behind the Escape guard, and each has a canary: with the emulator
// variables removed, the guard must see the requests, so a library that
// ignored the proxy can't pass.
func testFlows(t *testing.T, e *emulator) {
	// A home without gcloud's application-default credentials and no key,
	// so the libraries find only what `gcpemu env` set.
	envWithoutADC := func(t *testing.T, set map[string]string) []string {
		return withEnv(e.env, withHome(set, t.TempDir()), adcEnvVar)
	}
	run := func(t *testing.T, env []string, bin string, args ...string) {
		t.Helper()
		got, escaped := probeResults(t, e, env, bin, append(args, project, "1")...)
		for _, c := range flowCases {
			expectProbe(t, got, c, "OK")
		}
		if len(escaped) > 0 {
			t.Errorf("the Escape guard refused %v", escaped)
		}

		got, escaped = probeResults(t, e, env, bin, append(args, project, "1", "canary")...)
		t.Logf("canary: %v", got)
		if !slices.Equal(escaped, canaryHosts) {
			t.Errorf("canary: the Escape guard refused %v, want %v", escaped, canaryHosts)
		}
	}

	t.Run("python", func(t *testing.T) {
		py := pythonLibs(t)
		env := envWithoutADC(t, map[string]string{
			// The Python storage client needs a scheme here, which
			// `gcpemu env` leaves out.
			"STORAGE_EMULATOR_HOST": "http://" + e.vars["STORAGE_EMULATOR_HOST"],
		})
		run(t, env, py, "-I", "sdk/flows.py") // exec adds the Escape guard
	})

	t.Run("node", func(t *testing.T) {
		node, modules := nodeLibs(t)
		// sdk/flows.js turns STORAGE_EMULATOR_HOST into an apiEndpoint.
		env := envWithoutADC(t, map[string]string{"NODE_PATH": modules})
		run(t, append(env, e.guard.Env()...), node, "sdk/flows.js")
	})
}
