//go:build prerelease

package prerelease

import (
	"strings"
	"testing"
)

// The judgments of the VM relay check run on any host: they read output,
// not the runtime.

func TestRelayProbedOn(t *testing.T) {
	const others = "[ok  ] container runtime: Docker Desktop (chosen by Docker context \"desktop-linux\")\n"
	if err := relayProbedOn(others + "[ok  ] VM relay: on (gateway 172.17.0.1 not bindable)\n"); err != nil {
		t.Errorf("probed on: %v", err)
	}
	for name, line := range map[string]string{
		"off":          "[ok  ] VM relay: off (gateway 172.17.0.1 bindable)\n",
		"forced":       "[ok  ] VM relay: on (forced by GCPEMU_VM_RELAY)\n",
		"undetermined": "[--  ] VM relay: undetermined (no default bridge network to probe)\n",
		"missing":      "",
	} {
		if err := relayProbedOn(others + line); err == nil {
			t.Errorf("%s: relayProbedOn = nil, want an error", name)
		}
	}
}

// goTestJSON is go test -json's output for a run of TestA, which passes
// with a subtest that skips, and of TestB, which ends with ended.
func goTestJSON(ended string) string {
	return `{"Action":"start","Package":"p"}
{"Action":"run","Package":"p","Test":"TestA"}
{"Action":"run","Package":"p","Test":"TestA/sub"}
{"Action":"output","Package":"p","Test":"TestA/sub","Output":"    a_test.go:9: not here\n"}
{"Action":"skip","Package":"p","Test":"TestA/sub"}
{"Action":"pass","Package":"p","Test":"TestA"}
{"Action":"run","Package":"p","Test":"TestB"}
{"Action":"output","Package":"p","Test":"TestB","Output":"    b_test.go:7: the reason\n"}
{"Action":"` + ended + `","Package":"p","Test":"TestB"}
{"Action":"output","Package":"p","Output":"ok  \tp\t1.0s\n"}
{"Action":"pass","Package":"p"}
`
}

func TestPackageFailures(t *testing.T) {
	if got := packageFailures(goTestJSON("pass")); len(got) != 0 {
		t.Errorf("every test passed: packageFailures = %q", got)
	}
	for _, ended := range []string{"fail", "skip"} {
		got := packageFailures(goTestJSON(ended))
		if len(got) != 1 || !strings.Contains(got[0], "TestB") || !strings.Contains(got[0], "the reason") {
			t.Errorf("TestB ended with %s: packageFailures = %q, want TestB and its output", ended, got)
		}
	}
	build := `{"Action":"output","Package":"p","Output":"FAIL\tp [build failed]\n"}
{"Action":"fail","Package":"p"}
`
	if got := packageFailures(build); len(got) != 1 || !strings.Contains(got[0], "build failed") {
		t.Errorf("no test ran: packageFailures = %q, want go test's output", got)
	}
	// After every test passed, the test binary panicked.
	panicked := strings.Replace(goTestJSON("pass"), `{"Action":"pass","Package":"p"}`, `{"Action":"output","Package":"p","Output":"panic: late\n"}
{"Action":"fail","Package":"p"}`, 1)
	if got := packageFailures(panicked); len(got) != 1 || !strings.Contains(got[0], "panic: late") {
		t.Errorf("go test failed after the tests: packageFailures = %q, want go test's output", got)
	}
	// A subtest's output is its top-level test's.
	sub := strings.Replace(goTestJSON("pass"), `{"Action":"pass","Package":"p","Test":"TestA"}`, `{"Action":"fail","Package":"p","Test":"TestA"}`, 1)
	if got := packageFailures(sub); len(got) != 1 || !strings.Contains(got[0], "not here") {
		t.Errorf("TestA failed in a subtest: packageFailures = %q, want the subtest's output", got)
	}
}
