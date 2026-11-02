package emu

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestAliasScript: a Translated network's Managed containers carry their
// Declared address on a VLAN of the VPC interface, and routes via the
// NAT egress proxy go via its Declared address (ADR 0004).
func TestAliasScript(t *testing.T) {
	n := SubnetNet{Subnet: "10.0.0.0/20", ContainerSubnet: "10.241.0.0/20"}
	if s := n.AliasScript("10.0.0.2", "10.241.0.2"); s != "" {
		t.Fatalf("not Translated: %q", s)
	}
	n.Translated = true
	s := n.AliasScript("10.0.0.2", "10.241.0.2", "default", "172.16.0.0/12")
	for _, want := range []string{
		"awk -v a=10.241.0.2 ",
		`V="$IF.4000"`,
		`ip link add link "$IF" name "$V" type vlan id 4000`,
		`ip addr add 10.0.0.2/20 dev "$V"`,
		`ip route replace 10.0.0.0/20 dev "$V" src 10.0.0.2`,
		`ip route replace default via 10.0.15.254 dev "$V" src 10.0.0.2`,
		`ip route replace 172.16.0.0/12 via 10.0.15.254 dev "$V" src 10.0.0.2`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("script lacks %q:\n%s", want, s)
		}
	}
}

// TestAliasError (#173): when the kernel cannot create AliasScript's VLAN
// link, the failure names the runtime and says the Translated network
// can't be used on it; any other failure passes through unchanged.
func TestAliasError(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	n := SubnetNet{Name: "gcpemu-vpc-a", Subnet: "10.0.0.0/20", ContainerSubnet: "10.241.0.0/20", Translated: true}
	const rt = "docker 27.3.1 (Colima, kernel 6.8.0)"
	// run runs the script with an ip that holds 10.241.0.2 on eth0 and
	// fails cmd (an ip subcommand) with msg.
	run := func(t *testing.T, cmd, msg string) (string, error) {
		t.Helper()
		dir := t.TempDir()
		stub := "#!/bin/sh\ncase \"$*\" in\n" +
			"\"-o -4 addr show\") echo '2: eth0    inet 10.241.0.2/20 brd 10.241.15.255 scope global eth0' ;;\n" +
			"\"link show\"*) exit 1 ;;\n" +
			"\"" + cmd + "\"*) echo '" + msg + "' >&2; exit 2 ;;\n" +
			"esac\n"
		if err := os.WriteFile(filepath.Join(dir, "ip"), []byte(stub), 0o755); err != nil {
			t.Fatal(err)
		}
		c := exec.Command("sh", "-c", n.AliasScript("10.0.0.2", "10.241.0.2"))
		c.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"))
		out, err := c.CombinedOutput()
		return string(out), err
	}
	for _, msg := range []string{
		`Error: Unknown device type.`,
		`RTNETLINK answers: Operation not supported`,
		`RTNETLINK answers: Not supported`,
	} {
		out, err := run(t, "link add", msg)
		if err == nil {
			t.Fatalf("%s: script succeeded", msg)
		}
		got := n.AliasError(rt, out, err)
		var nv *NoVLANError
		if !errors.As(got, &nv) || nv.Runtime != rt || nv.Network != n.Name {
			t.Fatalf("%s: AliasError = %v; want NoVLANError", msg, got)
		}
		for _, want := range []string{rt, "VLAN (8021q)", "Translated network gcpemu-vpc-a"} {
			if !strings.Contains(got.Error(), want) {
				t.Errorf("%s: %q lacks %q", msg, got, want)
			}
		}
	}
	// Other failures, including another step failing with the same text,
	// pass through.
	for _, tc := range []struct{ cmd, msg string }{
		{"link add", "RTNETLINK answers: Operation not permitted"},
		{"route replace", "RTNETLINK answers: Operation not supported"},
	} {
		out, err := run(t, tc.cmd, tc.msg)
		if err == nil {
			t.Fatalf("%s: script succeeded", tc.cmd)
		}
		if got := n.AliasError(rt, out, err); got != err {
			t.Errorf("%s %q: AliasError = %v; want %v unchanged", tc.cmd, tc.msg, got, err)
		}
	}
	if got := n.AliasError(rt, "", nil); got != nil {
		t.Errorf("AliasError(nil) = %v", got)
	}
}

// TestPublicRoutesScript: the script keeps exactly the wanted public
// routes among those it tagged, leaving other routes alone (#187).
func TestPublicRoutesScript(t *testing.T) {
	// The script programs host routes, which only a Linux host has: native
	// Windows runs the Runtime Services on a VM runtime (ADR 0015).
	if runtime.GOOS == "windows" {
		t.Skip("host routes are not programmed on Windows")
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	run := func(t *testing.T, via string, ips []string, existing string) []string {
		t.Helper()
		dir := t.TempDir()
		log := filepath.Join(dir, "log")
		stub := "#!/bin/sh\necho \"$*\" >> " + log + "\n" +
			"case \"$*\" in \"-4 route show proto 245\") printf '" + existing + "' ;; esac\n"
		if err := os.WriteFile(filepath.Join(dir, "ip"), []byte(stub), 0o755); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("sh", "-c", PublicRoutesScript(via, ips))
		cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"))
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("script: %v: %s", err, out)
		}
		b, _ := os.ReadFile(log)
		var calls []string
		for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			if l != "-4 route show proto 245" {
				calls = append(calls, l)
			}
		}
		return calls
	}
	wantCalls := func(t *testing.T, label string, got, want []string) {
		t.Helper()
		if strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Errorf("%s, calls:\n%s\nwant:\n%s", label, strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
	}
	got := run(t, "172.18.0.9", []string{"34.120.0.1", "34.120.0.2"},
		`34.120.0.1 via 172.18.0.8 dev eth1 proto 245\n34.120.0.7 via 172.18.0.8 dev eth1 proto 245\n`)
	want := []string{
		"route del 34.120.0.7 proto 245",
		"route replace 34.120.0.1/32 via 172.18.0.9 proto 245",
		"route replace 34.120.0.2/32 via 172.18.0.9 proto 245",
	}
	wantCalls(t, "via", got, want)
	// An empty via rejects the addresses (ADR 0055): replacing the route
	// via the edge leaves no gap for traffic to leave by the default route.
	got = run(t, "", []string{"34.120.0.1"},
		`34.120.0.1 via 172.18.0.8 dev eth1 proto 245\nunreachable 34.120.0.7 proto 245\n`)
	want = []string{
		"route del 34.120.0.7 proto 245",
		"route replace unreachable 34.120.0.1/32 proto 245",
	}
	wantCalls(t, "empty via", got, want)
	// No addresses removes every tagged route, unreachable or via.
	got = run(t, "", nil,
		`unreachable 34.120.0.1 proto 245\n34.120.0.2 via 172.18.0.8 dev eth1 proto 245\n`)
	want = []string{
		"route del 34.120.0.1 proto 245",
		"route del 34.120.0.2 proto 245",
	}
	wantCalls(t, "no addresses", got, want)
}

// TestPublicRoutesScriptKernel runs the script in a network namespace of
// its own, where the kernel shows that switching an address between a
// route via the edge and an unreachable route leaves exactly one route
// for it, and that no addresses removes both kinds (ADR 0055).
func TestPublicRoutesScriptKernel(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("network namespaces are Linux-only")
	}
	ns := func(script string) (string, error) {
		out, err := exec.Command("unshare", "-rn", "sh", "-ec",
			"ip link set lo up\nip route add 172.18.0.0/16 dev lo\n"+script+"ip -4 route show proto 245\n").CombinedOutput()
		return string(out), err
	}
	if out, err := ns(""); err != nil {
		t.Skipf("no unprivileged network namespace: %v: %s", err, out)
	}
	ips := []string{"34.120.0.1", "34.120.0.2"}
	for _, tc := range []struct {
		name, script, want string
	}{
		{"via", PublicRoutesScript("172.18.0.9", ips),
			"34.120.0.1 via 172.18.0.9 dev lo \n34.120.0.2 via 172.18.0.9 dev lo \n"},
		{"via then unreachable", PublicRoutesScript("172.18.0.9", ips) + PublicRoutesScript("", ips[:1]),
			"unreachable 34.120.0.1 \n"},
		{"unreachable then via", PublicRoutesScript("", ips) + PublicRoutesScript("172.18.0.9", ips[1:]),
			"34.120.0.2 via 172.18.0.9 dev lo \n"},
		{"removed", PublicRoutesScript("172.18.0.9", ips[:1]) + PublicRoutesScript("", ips[1:]) + PublicRoutesScript("", nil),
			""},
	} {
		out, err := ns(tc.script)
		if err != nil {
			t.Fatalf("%s: %v: %s", tc.name, err, out)
		}
		if out != tc.want {
			t.Errorf("%s: routes:\n%q\nwant:\n%q", tc.name, out, tc.want)
		}
	}
}
