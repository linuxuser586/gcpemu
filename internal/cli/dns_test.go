package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// linuxResolved is a Linux host with systemd-resolved, where eth0 holds
// the default route.
func linuxResolved() resolverHost {
	return resolverHost{goos: "linux", resolved: true, links: map[string]resolvedLink{
		"eth0": {domains: []string{"~."}, dns: []string{"192.168.1.1"}},
	}}
}

func planText(t *testing.T, c resolverChange) string {
	t.Helper()
	var b strings.Builder
	for _, s := range c.plan {
		b.WriteString(s.String() + "\n")
		b.Write(s.stdin)
		b.Write(s.text)
	}
	return b.String()
}

func TestResolverPlanLinux(t *testing.T) {
	c, err := resolverPlan(linuxResolved(), nil, "default", defaultResolverSuffixes, 5353, true)
	if err != nil {
		t.Fatal(err)
	}
	want := `sudo sh -s
set -e
ip link show gcpemu-dns0 >/dev/null 2>&1 || ip link add gcpemu-dns0 type dummy
ip addr replace 169.254.53.1/32 dev gcpemu-dns0
ip link set gcpemu-dns0 up
i=0; until resolvectl status gcpemu-dns0 >/dev/null 2>&1 || [ $i -ge 50 ]; do i=$((i+1)); sleep 0.1; done
resolvectl dns gcpemu-dns0 127.0.0.1:5353
resolvectl domain gcpemu-dns0 '~svc.cluster.local' '~gke.internal'
resolvectl default-route gcpemu-dns0 false
`
	if got := planText(t, c); got != want {
		t.Errorf("plan:\n%s\nwant:\n%s", got, want)
	}
	if w := (resolverRecord{Suffixes: []string{"svc.cluster.local", "gke.internal"}, Port: 5353, Link: "gcpemu-dns0"}); !reflect.DeepEqual(c.record, w) {
		t.Errorf("record = %+v, want %+v", c.record, w)
	}
}

// TestResolverPlanLinuxAgain: a second install adds its suffix to the
// Instance's routes on the link it already holds, at the new port.
func TestResolverPlanLinuxAgain(t *testing.T) {
	h := linuxResolved()
	h.elevated = true
	h.links["gcpemu-dns0"] = resolvedLink{domains: []string{"~svc.cluster.local", "~gke.internal"}, dns: []string{"127.0.0.1:5353"}}
	recs := map[string]resolverRecord{"default": {Suffixes: []string{"svc.cluster.local", "gke.internal"}, Port: 5353, Link: "gcpemu-dns0"}}
	c, err := resolverPlan(h, recs, "default", append(slices.Clone(defaultResolverSuffixes), "example.dev.test"), 5354, true)
	if err != nil {
		t.Fatal(err)
	}
	want := `sh -s
set -e
ip link show gcpemu-dns0 >/dev/null 2>&1 || ip link add gcpemu-dns0 type dummy
ip addr replace 169.254.53.1/32 dev gcpemu-dns0
ip link set gcpemu-dns0 up
i=0; until resolvectl status gcpemu-dns0 >/dev/null 2>&1 || [ $i -ge 50 ]; do i=$((i+1)); sleep 0.1; done
resolvectl dns gcpemu-dns0 127.0.0.1:5354
resolvectl domain gcpemu-dns0 '~svc.cluster.local' '~gke.internal' '~example.dev.test'
resolvectl default-route gcpemu-dns0 false
`
	if got := planText(t, c); got != want {
		t.Errorf("plan:\n%s\nwant:\n%s", got, want)
	}
	if w := (resolverRecord{Suffixes: []string{"svc.cluster.local", "gke.internal", "example.dev.test"}, Port: 5354, Link: "gcpemu-dns0"}); !reflect.DeepEqual(c.record, w) {
		t.Errorf("record = %+v, want %+v", c.record, w)
	}
}

// TestResolverPlanLinuxSecondInstance: another Instance gets the next
// link and address, and can't take a suffix the first one routes.
func TestResolverPlanLinuxSecondInstance(t *testing.T) {
	h := linuxResolved()
	h.links["gcpemu-dns0"] = resolvedLink{domains: []string{"~svc.cluster.local", "~gke.internal"}}
	recs := map[string]resolverRecord{"default": {Suffixes: []string{"svc.cluster.local", "gke.internal"}, Port: 5353, Link: "gcpemu-dns0"}}
	if _, err := resolverPlan(h, recs, "other", []string{"gke.internal", "other.test"}, 6353, true); err == nil ||
		!strings.Contains(err.Error(), `gke.internal`) || !strings.Contains(err.Error(), `instance "default"`) {
		t.Errorf("err = %v, want one naming gke.internal and instance default", err)
	}
	c, err := resolverPlan(h, recs, "other", []string{"other.test"}, 6353, true)
	if err != nil {
		t.Fatal(err)
	}
	if got := planText(t, c); !strings.Contains(got, "ip addr replace 169.254.53.2/32 dev gcpemu-dns1\n") || !strings.Contains(got, "resolvectl domain gcpemu-dns1 '~other.test'\n") {
		t.Errorf("plan:\n%s", got)
	}
	if c.record.Link != "gcpemu-dns1" {
		t.Errorf("link = %q", c.record.Link)
	}
}

// TestResolverPlanLinuxRefuses: a suffix another link already routes, as
// a routing or a search domain, stops the whole install; so does a host
// without systemd-resolved, which gets the address and suffixes instead.
func TestResolverPlanLinuxRefuses(t *testing.T) {
	for _, d := range []string{"~gke.internal", "gke.internal"} {
		h := linuxResolved()
		h.links["docker0"] = resolvedLink{domains: []string{d}, dns: []string{"127.0.0.1:5353"}}
		_, err := resolverPlan(h, nil, "default", defaultResolverSuffixes, 5353, true)
		if err == nil || !strings.Contains(err.Error(), "docker0") || !strings.Contains(err.Error(), "gke.internal") {
			t.Errorf("%s on docker0: err = %v", d, err)
		}
	}
	h := linuxResolved()
	h.resolved = false
	_, err := resolverPlan(h, nil, "default", defaultResolverSuffixes, 5353, true)
	if err == nil || !strings.Contains(err.Error(), "systemd-resolved") || !strings.Contains(err.Error(), "127.0.0.1:5353") ||
		!strings.Contains(err.Error(), "svc.cluster.local gke.internal") {
		t.Errorf("without systemd-resolved: err = %v", err)
	}
}

// TestResolverPlanLinuxUninstall: uninstall --suffix keeps the rest of
// the Instance's routes; the last one goes with the link. A suffix the
// record doesn't name is refused.
func TestResolverPlanLinuxUninstall(t *testing.T) {
	h := linuxResolved()
	h.links["gcpemu-dns0"] = resolvedLink{domains: []string{"~svc.cluster.local", "~gke.internal"}}
	recs := map[string]resolverRecord{"default": {Suffixes: []string{"svc.cluster.local", "gke.internal"}, Port: 5353, Link: "gcpemu-dns0"}}
	c, err := resolverPlan(h, recs, "default", []string{"gke.internal"}, 5353, false)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := planText(t, c), "sudo sh -s\nset -e\nif ip link show gcpemu-dns0 >/dev/null 2>&1; then resolvectl domain gcpemu-dns0 '~svc.cluster.local'; fi\n"; got != want {
		t.Errorf("uninstall --suffix plan:\n%s\nwant:\n%s", got, want)
	}
	if w := (resolverRecord{Suffixes: []string{"svc.cluster.local"}, Port: 5353, Link: "gcpemu-dns0"}); !reflect.DeepEqual(c.record, w) {
		t.Errorf("record = %+v, want %+v", c.record, w)
	}
	for _, s := range [][]string{nil, {"gke.internal", "svc.cluster.local"}} {
		c, err := resolverPlan(h, recs, "default", s, 5353, false)
		if err != nil {
			t.Fatal(err)
		}
		if got, want := planText(t, c), "sudo sh -s\nset -e\nip link del gcpemu-dns0 2>/dev/null || true\n"; got != want {
			t.Errorf("uninstall %v plan:\n%s\nwant:\n%s", s, got, want)
		}
		if len(c.record.Suffixes) != 0 {
			t.Errorf("record = %+v, want none", c.record)
		}
	}
	if _, err := resolverPlan(h, recs, "default", []string{"other.test"}, 5353, false); err == nil || !strings.Contains(err.Error(), "other.test") {
		t.Errorf("uninstall of a suffix not installed: err = %v", err)
	}
	if c, err := resolverPlan(h, nil, "default", nil, 5353, false); err != nil || len(c.plan) != 0 {
		t.Errorf("uninstall without a record = %v, %v; want nothing to do", c.plan, err)
	}
}

// TestResolverPlanDarwin: one /etc/resolver file per suffix, marked with
// the Instance's name, so only its own files are rewritten or removed.
func TestResolverPlanDarwin(t *testing.T) {
	file := func(port string) string {
		return "# written by gcpemu (default)\nnameserver 127.0.0.1\nport " + port + "\n"
	}
	h := resolverHost{goos: "darwin", files: map[string]string{}}
	c, err := resolverPlan(h, nil, "default", defaultResolverSuffixes, 5353, true)
	if err != nil {
		t.Fatal(err)
	}
	want := `sudo sh -s
set -e
mkdir -p /etc/resolver
printf '%s\n' '# written by gcpemu (default)' 'nameserver 127.0.0.1' 'port 5353' > /etc/resolver/svc.cluster.local
printf '%s\n' '# written by gcpemu (default)' 'nameserver 127.0.0.1' 'port 5353' > /etc/resolver/gke.internal
`
	if got := planText(t, c); got != want {
		t.Errorf("install plan:\n%s\nwant:\n%s", got, want)
	}
	if w := (resolverRecord{Suffixes: []string{"svc.cluster.local", "gke.internal"}, Port: 5353}); !reflect.DeepEqual(c.record, w) {
		t.Errorf("record = %+v, want %+v", c.record, w)
	}

	// Again, with a suffix more and a new port: every file is rewritten.
	h.files = map[string]string{"svc.cluster.local": file("5353"), "gke.internal": file("5353")}
	recs := map[string]resolverRecord{"default": c.record}
	c, err = resolverPlan(h, recs, "default", []string{"example.dev.test"}, 5354, true)
	if err != nil {
		t.Fatal(err)
	}
	if got := planText(t, c); strings.Count(got, "'port 5354' > /etc/resolver/") != 3 || !strings.Contains(got, "/etc/resolver/example.dev.test\n") {
		t.Errorf("second install plan:\n%s", got)
	}
	if w := (resolverRecord{Suffixes: []string{"svc.cluster.local", "gke.internal", "example.dev.test"}, Port: 5354}); !reflect.DeepEqual(c.record, w) {
		t.Errorf("record = %+v, want %+v", c.record, w)
	}

	// A file gcpemu didn't write, or another Instance wrote, stops it all.
	for _, content := range []string{"nameserver 10.0.0.53\n", strings.Replace(file("5353"), "(default)", "(other)", 1)} {
		h := resolverHost{goos: "darwin", files: map[string]string{"gke.internal": content}}
		if _, err := resolverPlan(h, nil, "default", defaultResolverSuffixes, 5353, true); err == nil || !strings.Contains(err.Error(), "/etc/resolver/gke.internal") {
			t.Errorf("over %q: err = %v", content, err)
		}
	}
	if _, err := resolverPlan(h, map[string]resolverRecord{"other": {Suffixes: []string{"gke.internal"}, Port: 6353}}, "default", defaultResolverSuffixes, 5353, true); err == nil ||
		!strings.Contains(err.Error(), `instance "other"`) {
		t.Errorf("over another Instance's route: err = %v", err)
	}

	// Uninstall removes each file only while it carries the Instance's
	// marker (TestDNSCommand runs it), without reading them first.
	recs = map[string]resolverRecord{"default": {Suffixes: []string{"svc.cluster.local", "gke.internal", "example.dev.test"}, Port: 5353}}
	c, err = resolverPlan(resolverHost{goos: "darwin"}, recs, "default", []string{"svc.cluster.local"}, 5353, false)
	if err != nil {
		t.Fatal(err)
	}
	want = "sudo sh -s\nset -e\nif grep -qxF '# written by gcpemu (default)' /etc/resolver/svc.cluster.local 2>/dev/null; then rm -f /etc/resolver/svc.cluster.local; fi\n"
	if got := planText(t, c); got != want {
		t.Errorf("uninstall --suffix plan:\n%s\nwant:\n%s", got, want)
	}
	if w := (resolverRecord{Suffixes: []string{"gke.internal", "example.dev.test"}, Port: 5353}); !reflect.DeepEqual(c.record, w) {
		t.Errorf("record = %+v, want %+v", c.record, w)
	}
	c, err = resolverPlan(resolverHost{goos: "darwin"}, recs, "default", nil, 5353, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := planText(t, c); strings.Count(got, "then rm -f /etc/resolver/") != 3 {
		t.Errorf("uninstall plan:\n%s", got)
	}
	if len(c.record.Suffixes) != 0 {
		t.Errorf("record = %+v, want none", c.record)
	}
}

// TestResolverPlanWindows: one NRPT rule per suffix, commented with the
// Instance's name; NRPT can't name a port, so dns has to be on 53.
func TestResolverPlanWindows(t *testing.T) {
	const ours = "written by gcpemu (default)"
	h := resolverHost{goos: "windows"}
	if _, err := resolverPlan(h, nil, "default", defaultResolverSuffixes, 5353, true); err == nil || !strings.Contains(err.Error(), "--port dns=53") {
		t.Errorf("install on 5353: err = %v", err)
	}
	c, err := resolverPlan(h, nil, "default", defaultResolverSuffixes, 53, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.plan) != 1 || !strings.Contains(strings.Join(c.plan[0].args, " "), "-Verb RunAs") {
		t.Fatalf("plan = %v, want one step elevated through UAC", c.plan)
	}
	want := `$ErrorActionPreference = 'Stop'
Add-DnsClientNrptRule -Namespace '.svc.cluster.local' -NameServers '127.0.0.1' -Comment '` + ours + `'
Add-DnsClientNrptRule -Namespace '.gke.internal' -NameServers '127.0.0.1' -Comment '` + ours + `'
`
	if got := string(c.plan[0].text); got != want {
		t.Errorf("install script:\n%s\nwant:\n%s", got, want)
	}
	if w := (resolverRecord{Suffixes: []string{"svc.cluster.local", "gke.internal"}, Port: 53}); !reflect.DeepEqual(c.record, w) {
		t.Errorf("record = %+v, want %+v", c.record, w)
	}

	// Again, elevated, with a suffix more: only that rule is added.
	h = resolverHost{goos: "windows", elevated: true, nrpt: []nrptRule{
		{Name: "{1}", Namespace: []string{".svc.cluster.local"}, Comment: ours},
		{Name: "{2}", Namespace: []string{".gke.internal"}, Comment: ours},
		{Name: "{3}", Namespace: []string{".corp.example"}, Comment: "VPN"},
	}}
	recs := map[string]resolverRecord{"default": c.record}
	c, err = resolverPlan(h, recs, "default", []string{"example.dev.test"}, 53, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.plan) != 1 || strings.Contains(strings.Join(c.plan[0].args, " "), "-Verb RunAs") || !strings.Contains(c.plan[0].String(), "<the script below>") {
		t.Fatalf("plan = %v, want one step run directly", c.plan)
	}
	want = "$ErrorActionPreference = 'Stop'\nAdd-DnsClientNrptRule -Namespace '.example.dev.test' -NameServers '127.0.0.1' -Comment '" + ours + "'\n"
	if got := string(c.plan[0].text); got != want {
		t.Errorf("second install script:\n%s\nwant:\n%s", got, want)
	}

	// A rule gcpemu didn't write stops it all, as does another Instance's record.
	for _, n := range []string{".corp.example", "corp.example"} {
		h := resolverHost{goos: "windows", nrpt: []nrptRule{{Name: "{3}", Namespace: []string{".x.test", n}, Comment: "VPN"}}}
		if _, err := resolverPlan(h, nil, "default", []string{"corp.example", "a.test"}, 53, true); err == nil || !strings.Contains(err.Error(), "NRPT rule {3}") {
			t.Errorf("over %s: err = %v", n, err)
		}
	}
	// So does a namespace in the effective NRPT policy that no local rule
	// explains, such as one Group Policy or a VPN client pushes; the
	// policy's copy of the Instance's own rule doesn't.
	gpo := resolverHost{goos: "windows", nrpt: []nrptRule{
		{Name: "{1}", Namespace: []string{".svc.cluster.local"}, Comment: ours},
		{Name: "{2}", Namespace: []string{".gke.internal"}, Comment: ours},
	}, policy: []string{".svc.cluster.local", ".gke.internal", ".corp.example"}}
	if _, err := resolverPlan(gpo, nil, "default", []string{"corp.example"}, 53, true); err == nil || !strings.Contains(err.Error(), "corp.example (NRPT policy for .corp.example") {
		t.Errorf("over a policy namespace: err = %v", err)
	}
	if _, err := resolverPlan(gpo, nil, "default", defaultResolverSuffixes, 53, true); err != nil {
		t.Errorf("over the policy's copy of its own rules: %v", err)
	}
	if _, err := resolverPlan(h, map[string]resolverRecord{"other": {Suffixes: []string{"a.test"}, Port: 53}}, "default", []string{"a.test"}, 53, true); err == nil ||
		!strings.Contains(err.Error(), `instance "other"`) {
		t.Errorf("over another Instance's route: err = %v", err)
	}

	// Uninstall removes only the Instance's own rules, found as it runs.
	recs = map[string]resolverRecord{"default": {Suffixes: []string{"svc.cluster.local", "gke.internal"}, Port: 53}}
	c, err = resolverPlan(h, recs, "default", []string{"gke.internal"}, 53, false)
	if err != nil {
		t.Fatal(err)
	}
	remove := func(s string) string {
		return "Get-DnsClientNrptRule | Where-Object { $_.Comment -eq '" + ours + "' -and $_.Namespace -contains '." + s + "' } | Remove-DnsClientNrptRule -Force\n"
	}
	if got, want := string(c.plan[0].text), "$ErrorActionPreference = 'Stop'\n"+remove("gke.internal"); got != want {
		t.Errorf("uninstall --suffix script:\n%s\nwant:\n%s", got, want)
	}
	if w := (resolverRecord{Suffixes: []string{"svc.cluster.local"}, Port: 53}); !reflect.DeepEqual(c.record, w) {
		t.Errorf("record = %+v, want %+v", c.record, w)
	}
	c, err = resolverPlan(h, recs, "default", nil, 5353, false)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(c.plan[0].text), "$ErrorActionPreference = 'Stop'\n"+remove("svc.cluster.local")+remove("gke.internal"); got != want {
		t.Errorf("uninstall script:\n%s\nwant:\n%s", got, want)
	}
	if len(c.record.Suffixes) != 0 {
		t.Errorf("record = %+v, want none", c.record)
	}
}

// dnsHost points the dns commands at a macOS host whose resolver
// directory is a temp dir, as root, with records in a temp config dir.
func dnsHost(t *testing.T) (etc string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the macOS scripts need sh")
	}
	etc, cfg := t.TempDir(), t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfg)
	t.Setenv("HOME", cfg)
	t.Setenv("SUDO_USER", "")
	old := resolverHostNow
	resolverHostNow = func(read bool) (resolverHost, error) {
		h, err := readResolverHost("darwin", etc, read)
		h.elevated = true
		return h, err
	}
	t.Cleanup(func() { resolverHostNow = old })
	return etc
}

// gcpemu runs the gcpemu command line with args and returns its output.
func gcpemu(t *testing.T, args ...string) (string, error) {
	t.Helper()
	root := New(nil)
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(append([]string{"--config", filepath.Join(t.TempDir(), "none.yaml"), "--data-dir", t.TempDir()}, args...))
	err := root.Execute()
	return out.String(), err
}

func TestDNSCommand(t *testing.T) {
	etc := dnsHost(t)
	resolver := filepath.Join(etc, "resolver")
	files := func() []string {
		m, _ := filepath.Glob(filepath.Join(resolver, "*"))
		for i := range m {
			m[i] = filepath.Base(m[i])
		}
		return m
	}

	out, err := gcpemu(t, "dns", "install", "--dry-run", "--suffix", "Example.Dev.Test.")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"svc.cluster.local", "gke.internal", "example.dev.test"} {
		if !strings.Contains(out, resolver+"/"+s+"\n") {
			t.Errorf("dry run doesn't name %s:\n%s", s, out)
		}
	}
	if f := files(); len(f) != 0 {
		t.Errorf("dry run wrote %v", f)
	}

	// Without --yes, a stdin that isn't a terminal refuses.
	pipe, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer pipe.Close()
	w.Close()
	stdin := os.Stdin
	os.Stdin = pipe
	_, err = gcpemu(t, "dns", "install")
	os.Stdin = stdin
	if err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Errorf("install without --yes or a terminal: err = %v", err)
	}
	if f := files(); len(f) != 0 {
		t.Errorf("refused install wrote %v", f)
	}

	for _, bad := range []string{"a b", "x;rm", "-x.test", ""} {
		if _, err := gcpemu(t, "dns", "install", "--dry-run", "--suffix", bad); err == nil {
			t.Errorf("--suffix %q: no error", bad)
		}
	}
	if _, err := gcpemu(t, "--instance", "it's", "dns", "install", "--dry-run"); err == nil {
		t.Error("an Instance name with a quote: no error")
	}

	if _, err := gcpemu(t, "dns", "install", "--yes", "--suffix", "example.dev.test"); err != nil {
		t.Fatal(err)
	}
	if got, want := files(), []string{"example.dev.test", "gke.internal", "svc.cluster.local"}; !slices.Equal(got, want) {
		t.Fatalf("files = %v, want %v", got, want)
	}
	b, _ := os.ReadFile(filepath.Join(resolver, "gke.internal"))
	if want := "# written by gcpemu (default)\nnameserver 127.0.0.1\nport 5353\n"; string(b) != want {
		t.Errorf("gke.internal:\n%s\nwant:\n%s", b, want)
	}
	// Another Instance can't take a suffix this one routes.
	if _, err := gcpemu(t, "--instance", "other", "dns", "install", "--yes", "--suffix", "a.test"); err == nil || !strings.Contains(err.Error(), `instance "default"`) {
		t.Errorf("other Instance over the defaults: err = %v", err)
	}

	if _, err := gcpemu(t, "dns", "uninstall", "--yes", "--suffix", "gke.internal"); err != nil {
		t.Fatal(err)
	}
	if got, want := files(), []string{"example.dev.test", "svc.cluster.local"}; !slices.Equal(got, want) {
		t.Fatalf("files after uninstall --suffix = %v, want %v", got, want)
	}
	// A file someone else has rewritten since stays.
	if err := os.WriteFile(filepath.Join(resolver, "example.dev.test"), []byte("nameserver 10.0.0.53\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := gcpemu(t, "dns", "uninstall", "--yes"); err != nil {
		t.Fatal(err)
	}
	if got, want := files(), []string{"example.dev.test"}; !slices.Equal(got, want) {
		t.Errorf("files after uninstall = %v, want %v", got, want)
	}
	if out, err := gcpemu(t, "dns", "uninstall", "--yes"); err != nil || !strings.Contains(out, "no Resolver routes") {
		t.Errorf("uninstall again = %q, %v", out, err)
	}
}

func TestParseResolvectl(t *testing.T) {
	got := parseResolvectl("Global:\nLink 2 (enp0s31f6): lan\nLink 4 (virbr0):\nLink 11 (gcpemu-dns0): ~svc.cluster.local ~gke.internal\n")
	want := map[string][]string{"Global": {}, "enp0s31f6": {"lan"}, "virbr0": {}, "gcpemu-dns0": {"~svc.cluster.local", "~gke.internal"}}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for k, v := range want {
		if !slices.Equal(got[k], v) {
			t.Errorf("%s = %v, want %v", k, got[k], v)
		}
	}
	if got := parseResolvectl("Link 11 (gcpemu-dns0): 127.0.0.1:5353\n"); !slices.Equal(got["gcpemu-dns0"], []string{"127.0.0.1:5353"}) {
		t.Errorf("dns = %v", got)
	}
}

func TestParseNRPT(t *testing.T) {
	got, err := parseNRPT([]byte(`[{"Name":"{A}","Namespace":".svc.cluster.local","Comment":"written by gcpemu (default)"},{"Name":"{B}","Namespace":".a.test b.test","Comment":""}]` + "\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Name != "{A}" || !slices.Equal(got[1].Namespace, []string{".a.test", "b.test"}) || got[0].Comment != "written by gcpemu (default)" {
		t.Errorf("got %+v", got)
	}
	if got, err := parseNRPT([]byte("[]\r\n")); err != nil || len(got) != 0 {
		t.Errorf("no rules = %v, %v", got, err)
	}
}

// TestCheckResolverRoutes: doctor reports each recorded route as present,
// missing (as after a reboot on Linux) or on a stale port, on each OS.
func TestCheckResolverRoutes(t *testing.T) {
	marked := func(port string) string {
		return "# written by gcpemu (default)\nnameserver 127.0.0.1\nport " + port + "\n"
	}
	const ours = "written by gcpemu (default)"
	linux := func(dns string) resolverHost {
		h := linuxResolved()
		h.links["gcpemu-dns0"] = resolvedLink{domains: []string{"~svc.cluster.local"}, dns: []string{dns}}
		return h
	}
	rec := resolverRecord{Suffixes: []string{"svc.cluster.local", "gke.internal"}, Port: 5353, Link: "gcpemu-dns0"}
	for _, tc := range []struct {
		name string
		h    resolverHost
		port int
		want []string
	}{
		{"linux", linux("127.0.0.1:5353"), 5353, []string{"ok  ] Resolver route for svc.cluster.local", "WARN] Resolver route for gke.internal is gone"}},
		{"linux stale", linux("127.0.0.1:5353"), 5354, []string{"WARN] Resolver route for svc.cluster.local goes to 127.0.0.1:5353", "WARN] Resolver route for gke.internal is gone"}},
		{"linux after reboot", linuxResolved(), 5353, []string{"WARN] Resolver route for svc.cluster.local is gone", "WARN] Resolver route for gke.internal is gone"}},
		{"darwin", resolverHost{goos: "darwin", files: map[string]string{"svc.cluster.local": marked("5353"), "gke.internal": "nameserver 10.0.0.1\n"}}, 5353,
			[]string{"ok  ] Resolver route for svc.cluster.local", "WARN] Resolver route for gke.internal is gone"}},
		{"darwin stale", resolverHost{goos: "darwin", files: map[string]string{"svc.cluster.local": marked("5353"), "gke.internal": marked("5353")}}, 5354,
			[]string{"WARN] Resolver route for svc.cluster.local goes to 127.0.0.1:5353", "WARN] Resolver route for gke.internal goes to 127.0.0.1:5353"}},
		{"windows", resolverHost{goos: "windows", nrpt: []nrptRule{{Name: "{1}", Namespace: []string{".svc.cluster.local"}, Comment: ours}, {Name: "{2}", Namespace: []string{".gke.internal"}, Comment: "VPN"}}}, 53,
			[]string{"ok  ] Resolver route for svc.cluster.local", "WARN] Resolver route for gke.internal is gone"}},
		{"windows stale", resolverHost{goos: "windows", nrpt: []nrptRule{{Name: "{1}", Namespace: []string{".svc.cluster.local"}, Comment: ours}}}, 5353,
			[]string{"WARN] Resolver route for svc.cluster.local goes to 127.0.0.1:53", "WARN] Resolver route for gke.internal is gone"}},
	} {
		var out bytes.Buffer
		checkResolverRoutes(&out, tc.h, "default", rec, tc.port)
		got := out.String()
		for _, w := range tc.want {
			if !strings.Contains(got, "["+w) {
				t.Errorf("%s: missing %q in:\n%s", tc.name, w, got)
			}
		}
		if strings.Contains(got, "WARN") && !strings.Contains(got, "run `gcpemu dns install` again") {
			t.Errorf("%s: no fix in:\n%s", tc.name, got)
		}
	}
}

// TestStatusResolverRoutes: status lists the Instance's Resolver routes.
func TestStatusResolverRoutes(t *testing.T) {
	dnsHost(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/_emu/v1/info":
			_, _ = io.WriteString(w, `{"instance":"default","endpoints":{"dns":"127.0.0.1:5353"}}`)
		case "/_emu/v1/ready":
			_, _ = io.WriteString(w, `{"ready":true,"services":{}}`)
		default:
			_, _ = io.WriteString(w, `{}`)
		}
	}))
	defer srv.Close()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "endpoints.json"), []byte(`{"gateway":"`+strings.TrimPrefix(srv.URL, "http://")+`","dns":"127.0.0.1:5353"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := gcpemu(t, "--data-dir", dir, "dns", "install", "--yes", "--suffix", "example.dev.test"); err != nil {
		t.Fatal(err)
	}
	out, err := gcpemu(t, "--data-dir", dir, "status")
	if err != nil {
		t.Fatal(err)
	}
	if want := "resolver routes to 127.0.0.1:5353:\n  svc.cluster.local\n  gke.internal\n  example.dev.test\n"; !strings.Contains(out, want) {
		t.Errorf("status:\n%s\nwant it to contain:\n%s", out, want)
	}
	out, err = gcpemu(t, "--data-dir", dir, "status", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		ResolverRoutes struct {
			Port     int
			Suffixes []string
		}
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("status --json: %v\n%s", err, out)
	}
	if want := []string{"svc.cluster.local", "gke.internal", "example.dev.test"}; v.ResolverRoutes.Port != 5353 || !slices.Equal(v.ResolverRoutes.Suffixes, want) {
		t.Errorf("status --json resolverRoutes = %+v, want port 5353 and %v", v.ResolverRoutes, want)
	}
}
