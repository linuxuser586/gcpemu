//go:build prerelease

package prerelease

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/linuxuser586/gcpemu/internal/vmrelay"
)

// vmRelay is step 7 of both suites: the VM relay's own tests, which CI
// runs only on Linux with GCPEMU_VM_RELAY forcing the relay on (#653).
// With GCPEMU_VM_RELAY cleared, gcpemu on the host must decide the relay
// is on, not forced, as a user's Instance would (ADR 0056), and
// every test of the package must pass against the runtime: a skip means
// it didn't run.
func vmRelay(t *testing.T, ctx context.Context) {
	requireTools(t, "go")
	requireBin(t)
	t.Setenv(vmrelay.Env, "")
	if err := relayProbedOn(doctor(t)); err != nil {
		t.Fatal(err)
	}
	for _, f := range packageFailures(goTest(ctx, "./internal/vmrelay")) {
		t.Error(f)
	}
}

// relayProbedOn reports why doctor's output doesn't say the VM relay is on,
// not forced, or nil when it does.
func relayProbedOn(doctorOut string) error {
	for line := range strings.Lines(doctorOut) {
		status, d, ok := strings.Cut(strings.TrimSpace(line), "VM relay: ")
		if !ok {
			continue
		}
		if status != "[ok  ] " || !strings.HasPrefix(d, "on (") || strings.Contains(d, "forced") {
			return fmt.Errorf("gcpemu doctor reports the VM relay %s, not on and unforced:\n%s", d, doctorOut)
		}
		return nil
	}
	return fmt.Errorf("gcpemu doctor does not report the VM relay:\n%s", doctorOut)
}

// packageFailures reads go test -json's output for a package and says why
// it doesn't pass: each top-level test that failed or skipped, with its
// output, or go test's whole output when no test ran or the run failed
// outside every test.
func packageFailures(out string) []string {
	runs := testRuns(out)
	pkg := runs[""]
	delete(runs, "")
	if len(runs) == 0 {
		return []string{"no test ran:\n" + out}
	}
	var failures []string
	for _, name := range slices.Sorted(maps.Keys(runs)) {
		switch r := runs[name]; r.ended {
		case "pass":
		case "fail", "skip":
			failures = append(failures, fmt.Sprintf("%s ended with %s:\n%s", name, r.ended, r.log))
		default:
			failures = append(failures, fmt.Sprintf("%s didn't end:\n%s", name, r.log))
		}
	}
	if len(failures) == 0 && pkg.ended != "pass" {
		failures = append(failures, "go test failed outside every test:\n"+out)
	}
	return failures
}
