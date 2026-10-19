package cli

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/linuxuser586/gcpemu/internal/loopback"
	emuruntime "github.com/linuxuser586/gcpemu/internal/runtime"
)

// Linux, macOS and native Windows are supported, Windows with the Runtime
// Services on a VM runtime like macOS (ADR 0015).
func TestCheckPlatform(t *testing.T) {
	for goos, want := range map[string]bool{"linux": true, "darwin": true, "windows": true, "plan9": false} {
		ok := true
		var whats []string
		report := func(pass bool, what, fix string) { ok = ok && pass; whats = append(whats, what) }
		checkPlatform(report, goos, "amd64")
		if ok != want || len(whats) != 1 || whats[0] != "platform "+goos+"/amd64" {
			t.Errorf("%s: ok=%v %q", goos, ok, whats)
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
// on a VM runtime the loopback-alias decision, and warns when no
// --port-range port is published (ADR 0012).
func TestReportVMRuntime(t *testing.T) {
	aliases := emuruntime.AliasDecision{Reason: "disabled by GCPEMU_LOOPBACK_ALIASES"}
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
		{"off", off, "", nil, []string{"[ok  ] VM runtime: no (runtime OS Ubuntu)"}, []string{"WARN", "loopback aliases"}},
		{"published", on, "20000-20019", ranged, []string{"[ok  ] VM runtime: yes (runtime OS Docker Desktop)", "2 --port-range port(s) published",
			"[ok  ] loopback aliases for Cloud SQL: no (disabled by GCPEMU_LOOPBACK_ALIASES)"}, []string{"WARN"}},
		{"no range", on, "", ranged, []string{"[WARN]", "--port-range", "-p 127.0.0.1:"}, nil},
		{"range unpublished", on, "21000-21019", ranged, []string{"[WARN]", "-p 127.0.0.1:21000-21019:21000-21019"}, nil},
	} {
		var out strings.Builder
		reportVMRuntime(&out, tc.d, aliases, tc.portRange, tc.published)
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

// A running Instance lists the GCPEMU_LB_* variables `gcpemu env` withholds
// because more than one forwarding rule would print them (ADR 0013), with
// the qualified variables to use instead, or a rename when those are
// withheld too.
func TestReportWithheldLBVars(t *testing.T) {
	body := `{"listeners":[],"withheld":[
		{"variable":"GCPEMU_LB_P_A_GLOBAL_X","forwardingRules":["projects/p-a/global/forwardingRules/global-x","projects/p-a-global/global/forwardingRules/x"],
		 "qualified":["GCPEMU_LB_P_A_GLOBAL_X","GCPEMU_LB_P_A_GLOBAL_X"]},
		{"variable":"GCPEMU_LB_WEB","forwardingRules":["projects/p/global/forwardingRules/web","projects/p/regions/us-central1/forwardingRules/web"],
		 "qualified":["GCPEMU_LB_P_GLOBAL_WEB","GCPEMU_LB_P_US_CENTRAL1_WEB"]}]}`
	var out strings.Builder
	reportUnpublished(&out, []byte(body))
	got := out.String()
	for _, want := range []string{
		"[WARN] `gcpemu env` does not print GCPEMU_LB_WEB: forwarding rules projects/p/global/forwardingRules/web and projects/p/regions/us-central1/forwardingRules/web share it",
		"fix: use GCPEMU_LB_P_GLOBAL_WEB or GCPEMU_LB_P_US_CENTRAL1_WEB",
		"[WARN] `gcpemu env` does not print GCPEMU_LB_P_A_GLOBAL_X: forwarding rules projects/p-a/global/forwardingRules/global-x and projects/p-a-global/global/forwardingRules/x share it",
		"fix: rename one of the forwarding rules",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q:\n%s", want, got)
		}
	}
}

// A running Instance lists each public Cloud SQL instance's host address,
// and flags one that needs a loopback address and has none (ADR 0005,
// ADR 0012).
func TestReportSQLHosts(t *testing.T) {
	body := `{"instances":[
		{"instance":"p:us-central1:a","address":"127.0.0.7","loopback":true},
		{"instance":"p:us-central1:b","loopback":true,"missing":"no free loopback address"},
		{"instance":"p:us-central1:c","address":"172.18.0.4"}]}`
	var out strings.Builder
	reportSQLHosts(&out, []byte(body))
	got := out.String()
	for _, want := range []string{
		"[ok  ] Cloud SQL instance p:us-central1:a: host clients reach it on 127.0.0.7",
		"[WARN] Cloud SQL instance p:us-central1:b has no host address, so host clients cannot reach it: no free loopback address",
		"[ok  ] Cloud SQL instance p:us-central1:c: host clients reach it on 172.18.0.4",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "refused") {
		t.Errorf("output reports a refused loopback alias:\n%s", got)
	}

	// The runtime refused a loopback alias, so Cloud SQL now uses only
	// 127.0.0.1, whatever the decision said.
	out.Reset()
	reportSQLHosts(&out, []byte(`{"instances":[],"aliasesRefused":true}`))
	if got := out.String(); !strings.Contains(got, "[WARN] the runtime refused a loopback alias, so Cloud SQL instances use only 127.0.0.1") ||
		!strings.Contains(got, "sudo gcpemu loopback install") {
		t.Errorf("output lacks the refusal and its fix:\n%s", got)
	}
}

const hostServicesBody = `{"services":[
	{"cluster":"projects/p/locations/l/clusters/c1","namespace":"default","service":"web","name":"web.default.svc.c1.l.p.gke.internal","shortName":"web.default.svc.cluster.local","address":"127.0.0.7","ports":[8080,9090]},
	{"cluster":"projects/p/locations/l/clusters/c1","namespace":"default","service":"api","name":"api.default.svc.c1.l.p.gke.internal","address":"127.0.0.8","ports":[]}],
	"collisions":[{"name":"api.default.svc.cluster.local","clusters":["projects/p/locations/l/clusters/c1","projects/p/locations/l/clusters/c2"]}],
	"unserved":[{"cluster":"projects/p/locations/l/clusters/c2","namespace":"default","service":"db","reason":"no free loopback alias in 127.0.0.2-127.0.0.254"}],
	"poolExhausted":true}`

// Doctor lists the short Service names more than one cluster has, a pool
// with no free Loopback alias, and the Services left without one (ADR
// 0014).
func TestReportHostServices(t *testing.T) {
	var out strings.Builder
	reportHostServices(&out, []byte(hostServicesBody))
	got := out.String()
	for _, want := range []string{
		"[WARN] api.default.svc.cluster.local names a Kubernetes Service in projects/p/locations/l/clusters/c1 and projects/p/locations/l/clusters/c2, so it does not resolve for host clients",
		"fix: use the gke.internal names, e.g. api.default.svc.<cluster>.<location>.<project>.gke.internal",
		"[WARN] Loopback aliases 127.0.0.2-127.0.0.254 are all held, so new Kubernetes Services get SERVFAIL for host clients",
		"[WARN] Kubernetes Service default/db of projects/p/locations/l/clusters/c2 has no Loopback alias: no free loopback alias",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q:\n%s", want, got)
		}
	}
	out.Reset()
	reportHostServices(&out, []byte(`{"services":[],"collisions":[],"unserved":[],"poolExhausted":false}`))
	if out.String() != "" {
		t.Errorf("nothing to report: %q", out.String())
	}
}

// doctor names the runtime endpoint it picked and why, and any it skipped
// because it serves Windows containers (#375).
func TestCheckRuntimeDetection(t *testing.T) {
	pipe := func(n string) string { return "npipe:////./pipe/" + n }
	for _, tc := range []struct {
		name      string
		det       emuruntime.Detection
		err       error
		want, not []string
	}{
		{name: "picked with others",
			det: emuruntime.Detection{Endpoint: pipe("podman-machine-default"), Source: "socket list",
				Found:   []string{pipe("dockerDesktopLinuxEngine"), pipe("docker_engine"), pipe("podman-machine-default")},
				Skipped: []string{pipe("docker_engine")}},
			want: []string{"[ok  ] container runtime " + pipe("podman-machine-default") + " (chosen by socket list)",
				"also found: " + pipe("dockerDesktopLinuxEngine") + "\n",
				"skipped " + pipe("docker_engine") + ": serves Windows containers"}},
		{name: "unix", det: emuruntime.Detection{Endpoint: "unix:///var/run/docker.sock", Source: "socket list",
			Found: []string{"unix:///var/run/docker.sock", "unix:///home/u/.docker/run/docker.sock"}},
			want: []string{"also found: unix:///home/u/.docker/run/docker.sock\n"}, not: []string{"skipped"}},
		{name: "explicit windows containers",
			det:  emuruntime.Detection{Endpoint: pipe("docker_engine"), Source: "$DOCKER_HOST"},
			err:  fmt.Errorf("%s chosen by $DOCKER_HOST: %w", pipe("docker_engine"), emuruntime.ErrWindowsContainers),
			want: []string{"[FAIL] container runtime: ", "serves Windows containers; switch Docker Desktop to Linux containers", "fix: or choose a runtime serving Linux containers (unset DOCKER_HOST"},
			not:  []string{"start that runtime"}},
		{name: "probed windows containers only",
			det:  emuruntime.Detection{Found: []string{pipe("docker_engine")}, Skipped: []string{pipe("docker_engine")}},
			err:  fmt.Errorf("no container runtime found but %s: %w", pipe("docker_engine"), emuruntime.ErrWindowsContainers),
			want: []string{"[FAIL]", "switch Docker Desktop to Linux containers", "skipped " + pipe("docker_engine")},
			not:  []string{"fix:", "DOCKER_HOST", "also found"}},
		{name: "explicit down", det: emuruntime.Detection{Endpoint: "unix:///gone", Source: "$DOCKER_HOST"}, err: errors.New("gone"),
			want: []string{"[FAIL]", "fix: start that runtime"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out strings.Builder
			report := func(ok bool, what, fix string) {
				mark := "ok  "
				if !ok {
					mark = "FAIL"
				}
				fmt.Fprintf(&out, "[%s] %s\n", mark, what)
				if fix != "" {
					fmt.Fprintf(&out, "       fix: %s\n", fix)
				}
			}
			checkRuntimeDetection(&out, report, tc.det, tc.err)
			got := out.String()
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("%q lacks %q", got, w)
				}
			}
			for _, n := range tc.not {
				if strings.Contains(got, n) {
					t.Errorf("%q has %q", got, n)
				}
			}
		})
	}
}

// On Windows, doctor warns about each forwarding rule, served or not,
// and each configured port in an excluded port range (#376).
func TestReportExcludedPorts(t *testing.T) {
	body := `{"listeners":[
		{"forwardingRule":"projects/p/global/forwardingRules/a","port":8080,"listener":"127.0.0.5:8080"},
		{"forwardingRule":"projects/p/global/forwardingRules/b","port":8033,"listener":"127.0.0.6:18080","mode":"fallback"}],
		"unavailable":[{"forwardingRule":"projects/p/global/forwardingRules/c","port":50010,"error":"bind: access denied"}]}`
	ports := forwardingRulePorts([]byte(body))
	ports["port lb (--port lb=8033)"] = 8033
	ranges := loopback.ParseExcludedRanges([]byte("\n      8033        8033\n     50000       50059     *\n"))
	var out strings.Builder
	reportExcludedPorts(&out, "tcp", ranges, ports)
	got := out.String()
	for _, want := range []string{
		"[WARN] forwarding rule projects/p/global/forwardingRules/b (port 8033) is in Windows' excluded port range 8033",
		"[WARN] forwarding rule projects/p/global/forwardingRules/c (port 50010) is in Windows' excluded port range 50000-50059",
		"[WARN] port lb (--port lb=8033) (port 8033) is in Windows' excluded port range 8033",
		"netsh int ipv4 show excludedportrange protocol=tcp",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "forwardingRules/a") {
		t.Errorf("output names rule a, whose port is not excluded:\n%s", got)
	}
	if strings.Index(got, "forwardingRules/b") > strings.Index(got, "forwardingRules/c") {
		t.Errorf("output is not sorted:\n%s", got)
	}
	out.Reset()
	reportExcludedPorts(&out, "tcp", nil, ports)
	if out.Len() != 0 {
		t.Errorf("no ranges: output %q, want none", out.String())
	}
}
