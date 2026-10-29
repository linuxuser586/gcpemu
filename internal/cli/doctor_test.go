package cli

import (
	"bytes"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/lanbind"
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
	ranged := map[int]string{4562: "127.0.0.1:4562", 20000: "127.0.0.1:20000", 20001: "127.0.0.1:20001"}
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
	got = out.String()
	if !strings.Contains(got, "[WARN] the runtime refused a loopback alias, so Cloud SQL instances use only 127.0.0.1") ||
		!strings.Contains(got, "sudo gcpemu loopback install") {
		t.Errorf("output lacks the refusal and its fix:\n%s", got)
	}
	// Doctor runs in the container, which can't tell the host OS.
	for _, want := range []string{"only a macOS host", "Linux and Windows hosts already have them", "GCPEMU_LOOPBACK_ALIASES=true unless the runtime is Docker Desktop"} {
		if !strings.Contains(got, want) {
			t.Errorf("fix lacks %q:\n%s", want, got)
		}
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

// In a Containerized Instance, CA trust is the host's: doctor notes how to
// trust the CA there, and checks neither the container's bundles, its NSS
// databases nor its container runtimes (#493, #28). On the host, a CA the
// OS store lacks gets a note pointing at `gcpemu ca install`.
func TestCheckCATrust(t *testing.T) {
	saved := systemBundles
	defer func() { systemBundles = saved }()
	bundle := filepath.Join(t.TempDir(), "bundle.pem")
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("der")})
	if err := os.WriteFile(bundle, pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	other := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("other der")})
	systemBundles = []string{bundle}
	const colima = "unix:///Users/u/.colima/default/docker.sock"
	for _, tc := range []struct {
		name        string
		pem         []byte
		inContainer bool
		linuxOnly   bool // off Linux the OS store is the real one
		want        string
	}{
		{"in a container", pemBytes, true, false, "[--  ] Instance CA trust is the host's in a container: on the host, not in the container, run " +
			"`docker exec CONTAINER /gcpemu ca print > gcpemu-ca.pem` and `gcpemu --instance container ca install --cert gcpemu-ca.pem` " +
			"(README: \"Trusting the CA on the host\")\n"},
		{"trusted", pemBytes, false, true, "[ok  ] Instance CA trusted by the OS trust store\n"},
		{"not trusted", other, false, false, "[--  ] Instance CA not trusted by the OS trust store (run `gcpemu ca install`)\n"},
	} {
		var out strings.Builder
		checkCATrust(&out, "default", tc.pem, tc.inContainer, colima)
		got := out.String()
		switch {
		case tc.inContainer:
			if got != tc.want {
				t.Errorf("%s: output %q, want %q", tc.name, got, tc.want)
			}
		case tc.linuxOnly && runtime.GOOS != "linux":
		case !strings.HasPrefix(got, tc.want):
			t.Errorf("%s: output %q, want it to start %q", tc.name, got, tc.want)
		}
		if !tc.inContainer && !strings.Contains(got, "[--  ] cannot check whether Colima (profile default) trusts the Instance CA") {
			t.Errorf("%s: output %q, want the runtime trust note", tc.name, got)
		}
		if strings.Contains(got, "emulator CA") {
			t.Errorf("%s: output %q says \"emulator CA\"", tc.name, got)
		}
	}
}

// testEtc returns a temporary /etc with the given subdirectories, for
// checkRuntimeTrust, and a containerd with CRI disabled, as Docker's
// containerd.io package ships it.
func testEtc(t *testing.T, dirs ...string) trustHost {
	t.Helper()
	etc := t.TempDir()
	for _, d := range dirs {
		if err := os.MkdirAll(filepath.Join(etc, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return trustHost{goos: "linux", etc: etc, isDir: func(p string) bool { fi, err := os.Stat(p); return err == nil && fi.IsDir() },
		containerd: fakeContainerd("containerd containerd.io 1.7.27 05044ec\n", "disabled_plugins = [\"cri\"]\nversion = 2\n", nil)}
}

// writeTestFile writes b to name, creating its directory.
func writeTestFile(t *testing.T, name string, b []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// Docker trusts the Instance CA for image pulls only when every registry
// host's certs.d holds gcpemu-INSTANCE.crt matching it (#28).
func TestCheckRuntimeTrustDocker(t *testing.T) {
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("der")})
	hosts := registryHosts()
	if len(hosts) < 2 {
		t.Fatalf("registry hosts %v, want several", hosts)
	}
	h := testEtc(t, "docker")
	crt := func(host string) string { return filepath.Join(h.etc, "docker", "certs.d", host, "gcpemu-dev.crt") }
	for _, host := range hosts {
		writeTestFile(t, crt(host), pemBytes)
	}
	var out strings.Builder
	checkRuntimeTrust(&out, h, "dev", pemBytes, "")
	if want := "[ok  ] Instance CA trusted by Docker for "; !strings.HasPrefix(out.String(), want) || strings.Count(out.String(), "\n") != 1 {
		t.Errorf("all in place: output %q, want one line starting %q", out.String(), want)
	}

	if err := os.Remove(crt(hosts[0])); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, crt(hosts[1]), []byte("another CA\n"))
	out.Reset()
	checkRuntimeTrust(&out, h, "dev", pemBytes, "")
	want := fmt.Sprintf("[--  ] Instance CA not trusted by Docker for 2 of %d registry hosts (run `gcpemu ca install`)\n", len(hosts))
	if out.String() != want {
		t.Errorf("two missing: output %q, want %q", out.String(), want)
	}
}

// containerd trusts the Instance CA for a registry host when its hosts.toml
// names the shared CA file `ca install` writes and that file matches. A
// hosts.toml gcpemu did not write, which `ca install` leaves alone, gets
// the manual fix by name (#28).
func TestCheckRuntimeTrustContainerd(t *testing.T) {
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("der")})
	hosts := registryHosts()
	h := testEtc(t, "containerd")
	shared := h.etc + "/gcpemu/gcpemu-dev.crt"
	toml := func(host string) string { return filepath.Join(h.etc, "containerd", "certs.d", host, "hosts.toml") }
	for _, host := range hosts {
		writeTestFile(t, toml(host), []byte("# written by gcpemu (dev)\nserver = \"https://"+host+"\"\n\n[host.\"https://"+host+"\"]\n  ca = \""+shared+"\"\n"))
	}
	run := func() string {
		var out strings.Builder
		checkRuntimeTrust(&out, h, "dev", pemBytes, "")
		return out.String()
	}
	notTrusted := func(n int) string {
		return fmt.Sprintf("[--  ] Instance CA not trusted by containerd for %d of %d registry hosts", n, len(hosts))
	}

	if got, want := run(), notTrusted(len(hosts))+" (run `gcpemu ca install`)\n"; got != want {
		t.Errorf("no shared CA: output %q, want %q", got, want)
	}
	writeTestFile(t, shared, []byte("another CA\n"))
	if got, want := run(), notTrusted(len(hosts))+" (run `gcpemu ca install`)\n"; got != want {
		t.Errorf("mismatched shared CA: output %q, want %q", got, want)
	}
	writeTestFile(t, shared, pemBytes)
	if got, want := run(), fmt.Sprintf("[ok  ] Instance CA trusted by containerd for %d registry hosts\n", len(hosts)); got != want {
		t.Errorf("all in place: output %q, want %q", got, want)
	}

	writeTestFile(t, toml(hosts[0]), []byte("server = \"https://"+hosts[0]+"\"\n"))
	want := notTrusted(1) + " (add `ca = \"" + shared + "\"` to " + toml(hosts[0]) + ", which `gcpemu ca install` does not change)\n"
	if got := run(); got != want {
		t.Errorf("user-written hosts.toml: output %q, want %q", got, want)
	}
	if err := os.Remove(toml(hosts[1])); err != nil {
		t.Fatal(err)
	}
	want = notTrusted(2) + " (run `gcpemu ca install`; add `ca = \"" + shared + "\"` to " + toml(hosts[0]) + ", which `gcpemu ca install` does not change)\n"
	if got := run(); got != want {
		t.Errorf("user-written and missing hosts.toml: output %q, want %q", got, want)
	}
}

// Next to containerd's hosts.toml line, doctor says whether a kubelet on
// the host, pulling through containerd's CRI, reads those files, from checkCRIConfigPath: a note with the
// fix when they do not or containerd cannot say, nothing when CRI is
// disabled or there is no etc/containerd (#583).
func TestCheckRuntimeTrustCRI(t *testing.T) {
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("der")})
	h := testEtc(t, "containerd")
	certsD := h.etc + "/containerd/certs.d"
	run := func() string {
		var out strings.Builder
		checkRuntimeTrust(&out, h, "dev", pemBytes, "")
		_, cri, _ := strings.Cut(out.String(), "registry hosts (run `gcpemu ca install`)\n")
		return cri
	}
	if got := run(); got != "" {
		t.Errorf("CRI disabled: output %q, want no CRI line", got)
	}

	h.containerd = fakeContainerd(containerd17, dump17, nil)
	want := "[--  ] image pulls by a kubelet on this host (containerd 1.7.24's CRI) do not read the hosts.toml files in " + certsD +
		", as CRI's config_path does not list that folder; " + criMatters + "\n" +
		"       fix: add this to " + h.etc + "/containerd/config.toml (skip `version = 2` if the file already has it, and merge with that table if it is already there), then run `sudo systemctl restart containerd`:\n" +
		"         version = 2\n" +
		"         [plugins.\"io.containerd.grpc.v1.cri\".registry]\n" +
		"           config_path = \"" + certsD + "\"\n"
	if got := run(); got != want {
		t.Errorf("1.7: output %q, want %q", got, want)
	}

	h.containerd = fakeContainerd("containerd github.com/containerd/containerd/v2 v2.2.0 1c4457e\n", strings.ReplaceAll(dump2, "CONFIG_PATH", ""), nil)
	want = "[ok  ] image pulls by a kubelet on this host (containerd 2.2.0's CRI) read the hosts.toml files in " + certsD + "\n"
	if got := run(); got != want {
		t.Errorf("2.2: output %q, want %q", got, want)
	}

	h.containerd = func(...string) ([]byte, error) { return nil, &exec.Error{Name: "containerd", Err: exec.ErrNotFound} }
	want = "[--  ] cannot check whether image pulls by a kubelet on this host (containerd's CRI) read " + certsD + ": containerd is not on PATH; " + criMatters + "\n"
	if got := run(); got != want {
		t.Errorf("no binary: output %q, want %q", got, want)
	}

	for name, h := range map[string]trustHost{"no etc/containerd": testEtc(t, "docker"), "darwin": {goos: "darwin"}} {
		h.containerd = fakeContainerd(containerd17, dump17, nil)
		var out strings.Builder
		checkRuntimeTrust(&out, h, "dev", pemBytes, "")
		if strings.Contains(out.String(), "CRI") {
			t.Errorf("%s: output %q, want no CRI line", name, out.String())
		}
	}
}

// A VM runtime's trust store is out of doctor's reach: it notes so and
// prints the advice `ca install` prints for that runtime (#28).
func TestCheckRuntimeTrustVM(t *testing.T) {
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("der")})
	for _, tc := range []struct{ endpoint, what string }{
		{"unix:///Users/u/.colima/default/docker.sock", "Colima (profile default)"},
		{"/var/folders/xy/T/podman/podman-machine-default-api.sock", "Podman machine podman-machine-default"},
		{"unix:///Users/u/.docker/run/docker.sock", "Docker Desktop"},
	} {
		var out strings.Builder
		checkRuntimeTrust(&out, trustHost{goos: "darwin"}, "dev", pemBytes, tc.endpoint)
		var advice strings.Builder
		writeVMTrust(&advice, vmTrustCommand(tc.endpoint, "dev", &certFile{pem: pemBytes}, true), true, false)
		want := "[--  ] cannot check whether " + tc.what + " trusts the Instance CA for image pulls: its VM is out of reach\n" + advice.String()
		if advice.Len() == 0 || out.String() != want {
			t.Errorf("%s: output %q, want %q", tc.endpoint, out.String(), want)
		}
	}
}

// Without Docker or containerd, or off Linux, there is no runtime trust
// to report.
func TestCheckRuntimeTrustNoRuntime(t *testing.T) {
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("der")})
	for name, h := range map[string]trustHost{"empty etc": testEtc(t), "darwin": {goos: "darwin"}} {
		var out strings.Builder
		checkRuntimeTrust(&out, h, "dev", pemBytes, "unix:///var/run/docker.sock")
		if out.Len() != 0 {
			t.Errorf("%s: output %q, want none", name, out.String())
		}
	}
}

// An --lb-bind address that is not local is checked against the
// container's addresses in a Containerized Instance, and against the
// host's otherwise, a container on the host's network included (#564).
// In any container the fix lists the addresses, since the image has no
// `ip` (#493). On a bridge network the fix also says how to reach the LAN
// (#497).
func TestCheckLANBindAddress(t *testing.T) {
	for _, tc := range []struct {
		name                           string
		markers, containerized, bridge bool
		what, fixPrefix                string
	}{
		{"bridge network", true, true, true, "--lb-bind 203.0.113.7 is an address of this container", "pick one of the container's addresses: "},
		{"unconfirmed", true, true, false, "--lb-bind 203.0.113.7 is an address of this container", "pick one of the container's addresses: "},
		{"--containerized=true, no marker files", false, true, false, "--lb-bind 203.0.113.7 is an address of this container", "pick one from " + listAddrsCmd(runtime.GOOS)},
		{"host network", true, false, false, "--lb-bind 203.0.113.7 is an address of this host", "pick one of this host's addresses: "},
		{"host", false, false, false, "--lb-bind 203.0.113.7 is an address of this host", "pick one from " + listAddrsCmd(runtime.GOOS)},
	} {
		var whats, fixes []string
		failed := false
		report := func(ok bool, what, fix string) {
			if strings.HasPrefix(what, "--lb-bind 203.0.113.7") {
				failed = !ok
				whats, fixes = append(whats, what), append(fixes, fix)
			}
		}
		checkLANBind(io.Discard, report, &config.Config{LBBind: []string{"203.0.113.7"}}, false, false, false,
			containerFacts{markers: tc.markers, containerized: tc.containerized, bridge: tc.bridge})
		if !failed || len(whats) != 1 || whats[0] != tc.what || !strings.HasPrefix(fixes[0], tc.fixPrefix) {
			t.Errorf("%s: failed=%v %q %q, want %q %q...", tc.name, failed, whats, fixes, tc.what, tc.fixPrefix)
			continue
		}
		if got := strings.Contains(fixes[0], lanbind.Fix); got != tc.bridge {
			t.Errorf("%s: fix %q names the LAN fix: %v", tc.name, fixes[0], got)
		}
		if tc.markers && (strings.Contains(fixes[0], "ip -br addr") || strings.Contains(fixes[0], "127.0.0.1")) {
			t.Errorf("%s: container fix %q names a host command or a loopback address", tc.name, fixes[0])
		}
	}
}

// The fixes for privileged --gfe-bind and --lb-bind ports lower the
// container's own sysctl with `docker run --sysctl` in a Containerized
// Instance, and the host's with `sudo sysctl` otherwise, a container on
// the host's network included (#493, #564). In any container the lb-lan
// relay needs the runtime's socket mounted.
func TestPrivilegedPortFixes(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		markers, containerized bool
		gfe, lb                string
	}{
		{"Containerized Instance", true, true, "docker run --sysctl net.ipv4.ip_unprivileged_port_start=443", "docker run --sysctl net.ipv4.ip_unprivileged_port_start=80"},
		{"host network", true, false, "sudo sysctl net.ipv4.ip_unprivileged_port_start=443", "sudo sysctl net.ipv4.ip_unprivileged_port_start=80"},
		{"host", false, false, "sudo sysctl net.ipv4.ip_unprivileged_port_start=443", "sudo sysctl net.ipv4.ip_unprivileged_port_start=80"},
	} {
		var fixes []string
		report := func(ok bool, what, fix string) {
			if !ok {
				fixes = append(fixes, fix)
			}
		}
		c := containerFacts{markers: tc.markers, containerized: tc.containerized}
		checkGFEPort(report, 1024, false, false, c)
		checkLBPort(report, 1024, false, c)
		if len(fixes) != 2 || !strings.Contains(fixes[0], tc.gfe) || !strings.Contains(fixes[1], tc.lb) {
			t.Errorf("%s: fixes %q, want ones naming %q and %q", tc.name, fixes, tc.gfe, tc.lb)
			continue
		}
		if tc.containerized && strings.Contains(strings.Join(fixes, "\n"), "sudo sysctl") {
			t.Errorf("%s: container fixes %q name sudo sysctl", tc.name, fixes)
		}
		if !tc.containerized && strings.Contains(strings.Join(fixes, "\n"), "docker run --sysctl") {
			t.Errorf("%s: fixes %q name docker run --sysctl", tc.name, fixes)
		}
		if got := strings.Contains(fixes[1], "mount the container runtime's socket"); got != tc.markers {
			t.Errorf("%s: lb fix %q says to mount the socket: %v", tc.name, fixes[1], got)
		}
	}
}

// In a Containerized Instance, doctor notes the host checks it cannot run,
// for every host, since it cannot tell the host's OS; on a Linux host
// there are none (#493).
func TestCheckHostOnly(t *testing.T) {
	for _, tc := range []struct {
		inContainer bool
		want        []string
	}{
		{true, []string{"[--  ] host checks not run in a container", "on a macOS host", "`" + loopback.InstallHint + "`", "native binary",
			"on a Windows host", "--port-range", "`netsh int ipv4 show excludedportrange protocol=tcp`"}},
		{false, nil},
	} {
		var out strings.Builder
		checkHostOnly(&out, &config.Config{}, "linux", tc.inContainer, false)
		got := out.String()
		if len(tc.want) == 0 && got != "" {
			t.Errorf("inContainer=false on linux: output %q, want none", got)
		}
		for _, w := range tc.want {
			if !strings.Contains(got, w) {
				t.Errorf("inContainer=%v: output lacks %q:\n%s", tc.inContainer, w, got)
			}
		}
		if tc.inContainer && strings.Count(got, "\n") != 1 {
			t.Errorf("container note is not one line:\n%s", got)
		}
	}
}

// A running Instance whose runtime refused the Build daemon's privileged
// container is flagged with a fix, and one whose daemon runs reports its
// cache; one without a Build daemon yet reports nothing (ADR 0016).
func TestReportBuildDaemon(t *testing.T) {
	for _, tc := range []struct {
		body     string
		ok       []bool
		what     string
		fixNames string
	}{
		{`{"running":false,"cacheBytes":0,"error":"the Build daemon needs a privileged container, which the container runtime refused: mount: permission denied","privilegedRefused":true}`,
			[]bool{false}, "the container runtime refused the Build daemon's privileged container, so Builds fail", "privileged"},
		{`{"running":true,"cacheBytes":3145728,"privilegedRefused":false}`, []bool{true}, "Build daemon running, image cache 3.0 MiB", ""},
		{`{"running":false,"cacheBytes":0,"privilegedRefused":false}`, nil, "", ""},
	} {
		var oks []bool
		var whats, fixes []string
		reportBuildDaemon(func(ok bool, what, fix string) {
			oks, whats, fixes = append(oks, ok), append(whats, what), append(fixes, fix)
		}, []byte(tc.body))
		if len(oks) != len(tc.ok) || (len(oks) == 1 && (oks[0] != tc.ok[0] || !strings.Contains(whats[0], tc.what) || !strings.Contains(fixes[0], tc.fixNames))) {
			t.Errorf("%s: reported %v %q fix %q", tc.body, oks, whats, fixes)
		}
	}
}

// doctor reports a running Instance's Registry relay (ADR 0043): passing
// while it runs, with Podman machine's step, and failing with the fix when
// it was skipped or failed. A Containerized Instance's is off, with
// Podman machine's step too (#835).
func TestReportRegistryRelay(t *testing.T) {
	for _, tc := range []struct {
		body  string
		ok    []bool
		what  string
		fix   string
		lines string // printed besides report
	}{
		{`{"state":"running","listen":"127.0.0.1:4561"}`, []bool{true}, "reach the registry at 127.0.0.1:4561 inside the runtime's VM", "", ""},
		{`{"state":"running","listen":"127.0.0.1:4561","podman":"run podman push --tls-verify=false"}`, []bool{true}, "127.0.0.1:4561", "", "[WARN] Podman machine pushes to the registry over HTTPS by default"},
		{`{"state":"skipped","reason":"127.0.0.1:4561 is taken inside the runtime's VM","fix":"stop whatever listens"}`, []bool{false}, "can't reach the registry", "stop whatever listens", ""},
		{`{"state":"failed","reason":"boom"}`, []bool{false}, "failed to start, so the runtime's own docker push can't reach the registry: boom", "log", ""},
		{`{"state":"off","reason":"the runtime's daemon shares the host's loopback"}`, nil, "", "", "[--  ] Registry relay: off (the runtime's daemon shares the host's loopback)"},
		{`{"state":"starting"}`, nil, "", "", "[--  ] Registry relay: starting"},
		{`{"state":"off","reason":"a Containerized Instance's Published registry port 4561 reaches VM-side clients when published at the same host port (-p 127.0.0.1:4561:4561)"}`, nil, "", "", "[--  ] Registry relay: off (a Containerized Instance's Published registry port 4561 reaches VM-side clients when published at the same host port (-p 127.0.0.1:4561:4561))"},
		{`{"state":"off","reason":"a Containerized Instance's Published registry port 4561 reaches VM-side clients when published at the same host port (-p 127.0.0.1:4561:4561)","podman":"run podman push --tls-verify=false"}`, nil, "", "", "(-p 127.0.0.1:4561:4561))\n[WARN] Podman machine pushes to the registry over HTTPS by default\n       fix: run podman push --tls-verify=false"},
	} {
		var oks []bool
		var whats, fixes []string
		var out bytes.Buffer
		reportRegistryRelay(&out, func(ok bool, what, fix string) {
			oks, whats, fixes = append(oks, ok), append(whats, what), append(fixes, fix)
		}, []byte(tc.body))
		if len(oks) != len(tc.ok) || (len(oks) == 1 && (oks[0] != tc.ok[0] || !strings.Contains(whats[0], tc.what) || !strings.Contains(fixes[0], tc.fix))) {
			t.Errorf("%s: reported %v %q fix %q", tc.body, oks, whats, fixes)
		}
		if !strings.Contains(out.String(), tc.lines) {
			t.Errorf("%s: printed %q; want %q", tc.body, out.String(), tc.lines)
		}
	}
}

// When the registry's port is taken before start, doctor names another
// Instance's Registry relay that holds it, and on OrbStack and Colima,
// which forward their VM's host network, says a container with host
// networking may.
func TestReportRegistryPortHolder(t *testing.T) {
	for _, tc := range []struct {
		name     string
		h        relayHolder
		endpoint string
		want     string
	}{
		{"another Instance's relay", relayHolder{name: "gcpemu-abc-registryrelay", dir: "/data/other"}, "unix:///var/run/docker.sock", "held by gcpemu-abc-registryrelay, the Registry relay of the Instance in /data/other"},
		{"OrbStack", relayHolder{}, "unix:///Users/me/.orbstack/run/docker.sock", "a container with host networking that listens on port 4561 holds it here too"},
		{"Colima", relayHolder{}, "unix:///Users/me/.colima/default/docker.sock", "a container with host networking"},
		{"Docker Desktop", relayHolder{}, "unix:///Users/me/.docker/run/docker.sock", ""},
	} {
		var out bytes.Buffer
		reportRegistryPortHolder(&out, tc.h, 4561, forwardsHostNetwork(tc.endpoint))
		if tc.want == "" && out.Len() > 0 || !strings.Contains(out.String(), tc.want) {
			t.Errorf("%s: printed %q; want %q", tc.name, out.String(), tc.want)
		}
	}
}

// doctor decides once whether the Instance is a Containerized Instance,
// and on a bridge network, for its network checks (#564), and warns that
// it guesses only with the marker files, no answer from the runtime and
// --containerized=auto, naming why (#502).
func TestCheckContainerized(t *testing.T) {
	for _, tc := range []struct {
		setting                    string
		markers, hasRuntime, found bool
		self                       string
		warn                       string
		want                       containerFacts
	}{
		{"auto", true, false, false, "", "no container runtime socket", containerFacts{markers: true, containerized: true}},
		{"", true, false, false, "", "no container runtime socket", containerFacts{markers: true, containerized: true}},
		{"auto", true, true, false, "", emuruntime.SelfEnv, containerFacts{markers: true, containerized: true}},
		{"auto", true, true, true, "", "", containerFacts{markers: true}},
		{"auto", true, true, true, "abc", "", containerFacts{markers: true, containerized: true, bridge: true}},
		{"false", true, true, true, "abc", "", containerFacts{markers: true, containerized: true, bridge: true}},
		{"auto", false, false, false, "", "", containerFacts{}},
		{"false", true, false, false, "", "", containerFacts{markers: true}},
		{"true", true, true, false, "", "", containerFacts{markers: true, containerized: true}},
		{"true", false, false, false, "", "", containerFacts{containerized: true}},
	} {
		var out strings.Builder
		got := checkContainerized(&out, emuruntime.ContainerizedFacts{Setting: tc.setting, Markers: tc.markers, Runtime: tc.hasRuntime, Found: tc.found, Self: tc.self})
		if got != tc.want {
			t.Errorf("%+v: facts %+v, want %+v", tc, got, tc.want)
		}
		logged := out.String()
		if tc.warn == "" && logged != "" {
			t.Errorf("%+v: output %q, want none", tc, logged)
		}
		if tc.warn != "" && (!strings.HasPrefix(logged, "[WARN] ") || !strings.Contains(logged, tc.warn) ||
			!strings.Contains(logged, "--containerized=false") || strings.Count(logged, "\n") != 1) {
			t.Errorf("%+v: output %q, want one [WARN] line naming %q and --containerized=false", tc, logged, tc.warn)
		}
	}
}

// doctor reports each Delegated API's External emulator, failing for one
// that does not accept connections and naming its address (#623).
func TestReportExternal(t *testing.T) {
	var oks []bool
	var whats, fixes []string
	reportExternal(func(ok bool, what, fix string) {
		oks, whats, fixes = append(oks, ok), append(whats, what), append(fixes, fix)
	}, []byte(`{"external":[
		{"api":"firestore","address":"localhost:8080","hosts":["firestore.googleapis.com"],"reachable":true},
		{"api":"spanner","address":"localhost:9010","hosts":["spanner.googleapis.com"],"reachable":false,"error":"dial tcp [::1]:9010: connect: connection refused"}]}`))
	if len(oks) != 2 {
		t.Fatalf("reported %v %q, want firestore and spanner", oks, whats)
	}
	if !oks[0] || whats[0] != "External emulator for firestore at localhost:8080 accepts connections" {
		t.Errorf("firestore: %v %q", oks[0], whats[0])
	}
	if oks[1] || !strings.Contains(whats[1], "spanner") || !strings.Contains(whats[1], "localhost:9010") ||
		!strings.Contains(whats[1], "connection refused") || !strings.Contains(fixes[1], "--external spanner=") {
		t.Errorf("spanner: %v %q fix %q", oks[1], whats[1], fixes[1])
	}
}

// TestReportManagedExternal: doctor reports a Managed emulator, and one
// unavailable for want of a container runtime says so (#624).
func TestReportManagedExternal(t *testing.T) {
	var oks []bool
	var whats, fixes []string
	reportExternal(func(ok bool, what, fix string) {
		oks, whats, fixes = append(oks, ok), append(whats, what), append(fixes, fix)
	}, []byte(`{"external":[
		{"api":"bigtable","address":"","managed":true,"reachable":true},
		{"api":"firestore","address":"","managed":true,"reachable":false,"error":"it needs a container runtime, and none is usable: no container runtime found"}]}`))
	if len(oks) != 2 {
		t.Fatalf("reported %v %q, want bigtable and firestore", oks, whats)
	}
	if !oks[0] || whats[0] != "Managed emulator for bigtable accepts connections" {
		t.Errorf("bigtable: %v %q", oks[0], whats[0])
	}
	if oks[1] || !strings.Contains(whats[1], "Managed emulator for firestore") || !strings.Contains(whats[1], "container runtime") ||
		!strings.Contains(fixes[1], "container runtime") {
		t.Errorf("firestore: %v %q fix %q", oks[1], whats[1], fixes[1])
	}
}

// A running Instance lists the --lb-bind addresses a forwarding rule or a
// GKE LoadBalancer Service cannot serve because another frontend holds
// them, or the bind failed (#713).
func TestReportLBBindUnavailable(t *testing.T) {
	body := `{"listeners":[],"lanUnavailable":[
		{"frontend":"LoadBalancer Service p/us-central1-a/c0/default/web","address":"192.0.2.7:8080","error":"already serves forwarding rule fr"}]}`
	var out strings.Builder
	reportUnpublished(&out, []byte(body))
	want := "[WARN] LoadBalancer Service p/us-central1-a/c0/default/web is not served on --lb-bind address 192.0.2.7:8080: already serves forwarding rule fr"
	if got := out.String(); !strings.Contains(got, want) {
		t.Errorf("output lacks %q:\n%s", want, got)
	}
}

// A running Instance names what each --lb-bind endpoint serves: a
// forwarding rule, or a GKE LoadBalancer Service (#713).
func TestReportLANEndpoints(t *testing.T) {
	var out strings.Builder
	reportLANEndpoints(&out, map[string]string{
		"lb-lan:fr": "192.0.2.7:8080",
		"lb-lan:gke:p/us-central1-a/c0/default/web": "192.0.2.7:80,192.0.2.7:443",
	})
	got := out.String()
	for _, want := range []string{
		"[ok  ] forwarding rule fr exposed beyond loopback on 192.0.2.7:8080",
		"[ok  ] LoadBalancer Service p/us-central1-a/c0/default/web exposed beyond loopback on 192.0.2.7:80,192.0.2.7:443",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "forwarding rule gke:") {
		t.Errorf("a Service is named as a forwarding rule:\n%s", got)
	}
}

// A runtime whose info says containers lack the cpuset cgroup controller is
// a problem, since GKE nodes (k3s) cannot start, with the fix for a Podman
// machine or for rootless Linux (#632); one that has it is not reported.
func TestCheckCPUSet(t *testing.T) {
	podman := emuruntime.Info{Name: "podman", Version: "6.1.4", NoCPUSet: true}
	for _, tc := range []struct {
		goos      string
		in        emuruntime.Info
		want, not []string
	}{
		{goos: "darwin", in: podman, want: []string{"cpuset cgroup", "GKE nodes (k3s)", "podman machine stop && podman machine set --rootful && podman machine start"}, not: []string{"Delegate="}},
		{goos: "windows", in: podman, want: []string{"podman machine set --rootful"}, not: []string{"Delegate="}},
		{goos: "linux", in: podman, want: []string{"podman machine set --rootful", "Delegate=cpu cpuset io memory pids", "user@.service.d"}},
		{goos: "linux", in: emuruntime.Info{Name: "docker", NoCPUSet: true}, want: []string{"Delegate=cpu cpuset io memory pids"}, not: []string{"podman machine"}},
		{goos: "darwin", in: emuruntime.Info{Name: "podman", Version: "6.1.4"}},
	} {
		failed := false
		var got string
		report := func(ok bool, what, fix string) { failed = failed || !ok; got += what + "\n" + fix }
		checkCPUSet(report, tc.goos, tc.in)
		if failed != (len(tc.want) > 0) || (len(tc.want) == 0 && got != "") {
			t.Errorf("%s %+v: failed=%v %q", tc.goos, tc.in, failed, got)
		}
		for _, w := range tc.want {
			if !strings.Contains(got, w) {
				t.Errorf("%s %s: %q lacks %q", tc.goos, tc.in.Name, got, w)
			}
		}
		for _, n := range tc.not {
			if strings.Contains(got, n) {
				t.Errorf("%s %s: %q has %q", tc.goos, tc.in.Name, got, n)
			}
		}
	}
}
