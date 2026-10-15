package cli

import (
	"strings"
	"testing"

	emuruntime "github.com/linuxuser586/gcpemu/internal/runtime"
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

// In a Containerized Instance doctor prints the VM-runtime decision, and
// on a VM runtime warns when no --port-range port is published (ADR 0012).
func TestReportVMRuntime(t *testing.T) {
	on := emuruntime.VMDecision{On: true, Reason: "runtime OS Docker Desktop"}
	off := emuruntime.VMDecision{Reason: "runtime OS Ubuntu"}
	ranged := map[int]string{4510: "127.0.0.1:4510", 20000: "127.0.0.1:20000", 20001: "127.0.0.1:20001"}
	for _, tc := range []struct {
		name      string
		d         emuruntime.VMDecision
		portRange string
		published map[int]string
		want, not []string
	}{
		{"off", off, "", nil, []string{"[ok  ] VM runtime: no (runtime OS Ubuntu)"}, []string{"WARN"}},
		{"published", on, "20000-20019", ranged, []string{"[ok  ] VM runtime: yes (runtime OS Docker Desktop)", "2 --port-range port(s) published"}, []string{"WARN"}},
		{"no range", on, "", ranged, []string{"[WARN]", "--port-range", "-p 127.0.0.1:"}, nil},
		{"range unpublished", on, "21000-21019", ranged, []string{"[WARN]", "-p 127.0.0.1:21000-21019:21000-21019"}, nil},
	} {
		var out strings.Builder
		reportVMRuntime(&out, tc.d, tc.portRange, tc.published)
		for _, w := range tc.want {
			if !strings.Contains(out.String(), w) {
				t.Errorf("%s: output lacks %q:\n%s", tc.name, w, out.String())
			}
		}
		for _, n := range tc.not {
			if strings.Contains(out.String(), n) {
				t.Errorf("%s: output has %q:\n%s", tc.name, n, out.String())
			}
		}
	}
}

// A cluster whose servicelb setting does not match the VM-runtime decision
// is flagged: it was last started under another decision.
func TestReportServiceLB(t *testing.T) {
	server := func(name, why string, off bool) emuruntime.Container {
		ct := emuruntime.Container{Labels: map[string]string{emuruntime.LabelResource: name}, Cmd: []string{"server", "--disable", "traefik"}}
		if off {
			ct.Cmd = append(ct.Cmd, "--disable", "servicelb")
		}
		if why != "" {
			ct.Labels[emuruntime.LabelHostLB] = why
		}
		return ct
	}
	servers := []emuruntime.Container{
		server("p/l/old", "", false),
		server("p/l/trans", emuruntime.HostLBTranslated, true),
		server("p/l/vm", emuruntime.HostLBVM, true),
		server("p/l/legacy-trans", "", true),
	}
	var out strings.Builder
	reportServiceLB(&out, true, servers)
	if got := out.String(); strings.Count(got, "[WARN]") != 1 || !strings.Contains(got, "p/l/old") || !strings.Contains(got, "restart the emulator") {
		t.Errorf("on a VM runtime:\n%s", got)
	}
	out.Reset()
	reportServiceLB(&out, false, servers)
	if got := out.String(); strings.Count(got, "[WARN]") != 1 || !strings.Contains(got, "p/l/vm") {
		t.Errorf("off a VM runtime:\n%s", got)
	}
	out.Reset()
	reportServiceLB(&out, true, servers[1:3])
	if got := out.String(); strings.Contains(got, "WARN") {
		t.Errorf("all matching:\n%s", got)
	}
}

// doctor reads start's flags from the command its container ran.
func TestStartFlag(t *testing.T) {
	cmd := []string{"start", "--ephemeral", "--port-range", "20000-20019", "--vm-runtime=true"}
	if got := startFlag(cmd, "port-range"); got != "20000-20019" {
		t.Errorf("port-range = %q", got)
	}
	if got := startFlag(cmd, "vm-runtime"); got != "true" {
		t.Errorf("vm-runtime = %q", got)
	}
	if got := startFlag(cmd, "bind"); got != "" {
		t.Errorf("bind = %q, want none", got)
	}
}

// A running Containerized Instance lists the forwarding rules with no
// published port, each with what to publish (ADR 0012); the others, and
// every rule of an Instance on the host, go unmentioned.
func TestReportUnpublished(t *testing.T) {
	body := `{"listeners":[
		{"forwardingRule":"projects/p/global/forwardingRules/a","port":8080,"listener":"127.0.0.5:8080","published":"127.0.0.1:28080"},
		{"forwardingRule":"projects/p/global/forwardingRules/b","port":8080,"listener":"127.0.0.6:8080","unpublished":"publish more of --port-range 20000-20019"},
		{"forwardingRule":"projects/p/global/forwardingRules/c","port":80,"listener":"127.0.0.7:80"}]}`
	var out strings.Builder
	reportUnpublished(&out, []byte(body))
	got := out.String()
	if !strings.Contains(got, "[WARN] forwarding rule projects/p/global/forwardingRules/b (port 8080) has no published port") ||
		!strings.Contains(got, "fix: publish more of --port-range 20000-20019") {
		t.Errorf("output lacks rule b and its fix:\n%s", got)
	}
	if strings.Contains(got, "forwardingRules/a") || strings.Contains(got, "forwardingRules/c") {
		t.Errorf("output names a served rule:\n%s", got)
	}
}
