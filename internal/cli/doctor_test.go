package cli

import (
	"strings"
	"testing"
)

// Native Windows is supported, with the Runtime Services and host mode
// left to WSL2 and the runtime not checked (ADR 0008).
func TestCheckPlatform(t *testing.T) {
	for _, tc := range []struct {
		goos       string
		ok, checks bool
		note       string
	}{
		{goos: "linux", ok: true, checks: true},
		{goos: "darwin", ok: true, checks: true},
		{goos: "windows", ok: true, note: "Cloud SQL, GKE, Cloud NAT and host mode: the container runtime is not usable on native Windows; run gcpemu in WSL2"},
		{goos: "plan9", checks: true},
	} {
		var out strings.Builder
		ok := true
		report := func(pass bool, what, fix string) { ok = ok && pass }
		checks := checkPlatform(&out, report, tc.goos, "amd64")
		if ok != tc.ok || checks != tc.checks || !strings.Contains(out.String(), tc.note) {
			t.Errorf("%s: ok=%v checks=%v %q", tc.goos, ok, checks, out.String())
		}
		if tc.goos != "windows" && strings.Contains(out.String(), "WSL2") {
			t.Errorf("%s mentions WSL2: %q", tc.goos, out.String())
		}
	}
}
