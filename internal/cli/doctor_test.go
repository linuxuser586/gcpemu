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
		if tc.goos == "darwin" && strings.Contains(out.String(), "Linux-only") {
			t.Errorf("darwin still calls something Linux-only: %q", out.String())
		}
		if tc.goos != "windows" && strings.Contains(out.String(), "WSL2") {
			t.Errorf("%s mentions WSL2: %q", tc.goos, out.String())
		}
	}
}

// Each OS is told its own command for listing addresses (#175).
func TestListAddrsCmd(t *testing.T) {
	for goos, want := range map[string]string{"linux": "`ip -br addr`", "darwin": "`ifconfig`", "windows": "`ipconfig`"} {
		if got := listAddrsCmd(goos); got != want {
			t.Errorf("%s: %s, want %s", goos, got, want)
		}
	}
}

// A runtime VM of another architecture is a note naming the fix for this
// Mac (#175); elsewhere it is only reported.
func TestCheckRuntimeArch(t *testing.T) {
	for _, tc := range []struct {
		goos, host, rt string
		want, not      []string
	}{
		{goos: "darwin", host: "arm64", rt: "arm64", want: []string{"[ok  ]", "linux/arm64 matches"}},
		{goos: "darwin", host: "arm64", rt: "amd64", want: []string{"[--  ]", "linux/amd64", "this Mac is arm64", "emulated", "colima start --arch aarch64"}},
		{goos: "darwin", host: "amd64", rt: "arm64", want: []string{"[--  ]", "colima start --arch x86_64"}},
		{goos: "linux", host: "amd64", rt: "arm64", want: []string{"[--  ]", "linux/arm64", "linux/amd64"}, not: []string{"colima", "Mac", "emulated"}},
		{goos: "darwin", host: "arm64", rt: ""},
	} {
		var out strings.Builder
		checkRuntimeArch(&out, tc.goos, tc.host, tc.rt)
		got := out.String()
		if len(tc.want) == 0 && got != "" {
			t.Errorf("%+v: %q, want nothing", tc, got)
		}
		for _, w := range tc.want {
			if !strings.Contains(got, w) {
				t.Errorf("%s host %s runtime %s: %q lacks %q", tc.goos, tc.host, tc.rt, got, w)
			}
		}
		for _, n := range tc.not {
			if strings.Contains(got, n) {
				t.Errorf("%s host %s runtime %s: %q has %q", tc.goos, tc.host, tc.rt, got, n)
			}
		}
	}
}
