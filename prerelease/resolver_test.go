//go:build prerelease

package prerelease

import (
	"context"
	"errors"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	goruntime "runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/linuxuser586/gcpemu/internal/hosttest"
)

// resolverTestsEnv turns on TestResolverRoutes, the Linux CI check.
const resolverTestsEnv = "GCPEMU_RESOLVER_TESTS"

// The resolver-routes check's Instance, of its own so that its install
// record never collides with a real Instance's, and its Cloud DNS zone.
const (
	dnsInstance = "prerelease-dns"
	dnsSuffix   = "prerelease.test"
	dnsRecord   = "www." + dnsSuffix
	dnsAddress  = "192.0.2.53"
)

// The suffixes `gcpemu dns install --suffix dnsSuffix` routes: the Host
// attachment's names (defaultResolverSuffixes in internal/cli/dns.go) and
// the zone's.
var dnsSuffixes = []string{"svc.cluster.local", "gke.internal", dnsSuffix}

// TestResolverRoutes is the Linux CI check of `gcpemu dns install`
// (#611): a Cloud DNS record resolves through systemd-resolved, and
// uninstall takes the dummy link and its routing domains away again. It
// changes the host's resolver, so it runs only with GCPEMU_RESOLVER_TESTS=1,
// and it needs passwordless sudo.
func TestResolverRoutes(t *testing.T) {
	if goruntime.GOOS != "linux" || os.Getenv(resolverTestsEnv) != "1" {
		t.Skipf("changes the host's resolver: runs on Linux with %s=1", resolverTestsEnv)
	}
	requireTools(t, "gcloud", "resolvectl")
	requireBin(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	resolverRoutes(t, ctx, false)
}

// resolverRoutesStep is step 6 of both suites: resolverRoutes with a Host
// attachment name. On Linux, where the suite runs only to be worked on, it
// changes the host's resolver only with GCPEMU_RESOLVER_TESTS=1 too.
func resolverRoutesStep(t *testing.T, ctx context.Context) {
	if goruntime.GOOS == "linux" && os.Getenv(resolverTestsEnv) != "1" {
		t.Skipf("changes the host's resolver: runs on Linux with %s=1", resolverTestsEnv)
	}
	resolverRoutes(t, ctx, true)
}

// resolverRoutes starts dnsInstance with a Cloud DNS zone, and with
// withService a GKE Service, installs its Resolver routes, resolves the
// zone's record (and the Service's Host attachment name) through the
// host's resolver, and checks that uninstall takes the routes away. The
// Instance is stopped and its routes uninstalled even when it fails; the
// routes of any other Instance are left alone.
func resolverRoutes(t *testing.T, ctx context.Context, withService bool) {
	services := "dns"
	var ports []string
	if withService {
		services = "dns,gke"
	}
	if goruntime.GOOS == "windows" {
		// NRPT can't name a port (ADR 0026).
		if err := port53Free(); err != nil {
			t.Skipf("port 53 is in use on the host (ICS, Hyper-V or WSL's DNS proxy may hold it), and NRPT sends queries there: %v", err)
		}
		ports = append(ports, "dns=53")
	} else {
		requireSudo(t)
	}
	if withService {
		requireAliases(t, ctx)
	}
	h := startHost(t, ctx, dnsInstance, services, ports...)
	c := newClients(t)
	c.load(hosttest.ParseEnv(h.env(t)))
	c.run(t, ctx, "gcloud", "dns", "managed-zones", "create", "prerelease", "--dns-name="+dnsSuffix+".", "--description=prerelease")
	c.run(t, ctx, "gcloud", "dns", "record-sets", "create", dnsRecord+".", "--zone=prerelease", "--type=A", "--ttl=60", "--rrdatas="+dnsAddress)
	if withService {
		c.run(t, ctx, "gcloud", "container", "clusters", "create", "dns", "--zone="+zone, "--num-nodes=1")
		c.kubeconfig(t, "dns")
		c.run(t, ctx, "kubectl", "create", "deployment", "web", "--image=nginx:1.29-alpine")
		c.run(t, ctx, "kubectl", "expose", "deployment", "web", "--port=80")
	}

	// Before install: a run that fails halfway still uninstalls, which
	// touches nothing without this Instance's record.
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if out, err := h.gcpemu(ctx, "dns", "uninstall", "--yes"); err != nil {
			t.Errorf("gcpemu dns uninstall: %v\n%s", err, out)
		}
	})
	if out, err := h.gcpemu(ctx, "dns", "install", "--yes", "--suffix", dnsSuffix); err != nil {
		if owner := routesOwner(out); owner != "" {
			t.Fatalf("instance %q already has Resolver routes on this host, so gcpemu dns install refused: run `gcpemu --instance %s dns uninstall` for it first (this check leaves them alone)\n%s", owner, owner, out)
		}
		t.Fatalf("gcpemu dns install: %v\n%s", err, out)
	}
	link := resolvedLink(t) // "" but on Linux

	if ip := resolve(t, ctx, dnsRecord); ip != dnsAddress {
		t.Errorf("%s resolves to %s through the host's resolver, want the record's %s", dnsRecord, ip, dnsAddress)
	}
	if withService {
		const name = "web.default.svc.cluster.local"
		if ip := resolve(t, ctx, name); !loopbackAlias(ip) {
			t.Errorf("%s resolves to %s through the host's resolver, want a Loopback alias", name, ip)
		}
	}

	if out, err := h.gcpemu(ctx, "dns", "uninstall", "--yes"); err != nil {
		t.Fatalf("gcpemu dns uninstall: %v\n%s", err, out)
	}
	routesGone(t, link)
}

// requireSudo fails t unless sudo runs without a password.
func requireSudo(t *testing.T) {
	t.Helper()
	if out, err := exec.Command("sudo", "-n", "true").CombinedOutput(); err != nil {
		t.Fatalf("sudo needs a password: run `sudo -v` first (task prerelease does): %v\n%s", err, out)
	}
}

// port53Free returns why the dns Service endpoint can't have port 53 on
// the host's loopback, if it can't.
func port53Free() error {
	l, err := net.Listen("tcp", "127.0.0.1:53")
	if err != nil {
		return err
	}
	l.Close()
	p, err := net.ListenPacket("udp", "127.0.0.1:53")
	if err != nil {
		return err
	}
	return p.Close()
}

// ownerRE matches resolverPlan's refusal in internal/cli/dns.go.
var ownerRE = regexp.MustCompile(`instance "([^"]+)" has the Resolver route for`)

// routesOwner returns the Instance that `gcpemu dns install`'s output says
// holds a route already, if it says so.
func routesOwner(out string) string {
	if m := ownerRE.FindStringSubmatch(out); m != nil {
		return m[1]
	}
	return ""
}

var ipv4RE = regexp.MustCompile(`\b\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}\b`)

// addresses returns the IPv4 addresses in a resolver's answer.
func addresses(out string) []string {
	var ips []string
	for _, s := range ipv4RE.FindAllString(out, -1) {
		if net.ParseIP(s) != nil {
			ips = append(ips, s)
		}
	}
	return ips
}

// resolve waits for the host's resolver, as its own clients ask it, to
// answer name with an IPv4 address, and returns it.
func resolve(t *testing.T, ctx context.Context, name string) string {
	t.Helper()
	var args []string
	switch goruntime.GOOS {
	case "darwin":
		args = []string{"dscacheutil", "-q", "host", "-a", "name", name}
	case "linux":
		args = []string{"resolvectl", "query", "-4", "--cache=no", name}
	case "windows":
		args = powershellArgs("(Resolve-DnsName -Name '" + name + "' -Type A -DnsOnly -ErrorAction Stop | Where-Object Type -eq 'A').IPAddress")
	}
	var out string
	var ips []string
	hosttest.Eventually(t, time.Minute, name+" to resolve", func() bool {
		b, _ := exec.CommandContext(ctx, args[0], args[1:]...).CombinedOutput()
		out = string(b)
		ips = addresses(out)
		return len(ips) > 0
	}, func() string { return strings.Join(args, " ") + ":\n" + out })
	return ips[0]
}

func powershellArgs(script string) []string {
	return []string{"powershell", "-NoProfile", "-NonInteractive", "-Command", script}
}

// resolvedLink returns the link that holds dnsSuffix's routing domain in
// systemd-resolved, on Linux.
func resolvedLink(t *testing.T) string {
	t.Helper()
	if goruntime.GOOS != "linux" {
		return ""
	}
	out, err := exec.Command("resolvectl", "domain").Output()
	if err != nil {
		t.Fatalf("resolvectl domain: %v", err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		head, domains, _ := strings.Cut(line, ":")
		i, j := strings.Index(head, "("), strings.LastIndex(head, ")")
		if i >= 0 && j > i && slices.Contains(strings.Fields(domains), "~"+dnsSuffix) {
			return head[i+1 : j]
		}
	}
	t.Fatalf("no link has the routing domain ~%s after gcpemu dns install:\n%s", dnsSuffix, out)
	return ""
}

// routesGone fails t unless what install added for dnsInstance is gone:
// link and its routing domains on Linux, the /etc/resolver files on
// macOS, the NRPT rules on Windows.
func routesGone(t *testing.T, link string) {
	t.Helper()
	switch goruntime.GOOS {
	case "darwin":
		for _, s := range dnsSuffixes {
			if _, err := os.Stat(filepath.Join("/etc/resolver", s)); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("/etc/resolver/%s is still there after gcpemu dns uninstall (%v)", s, err)
			}
		}
	case "linux":
		if out, err := exec.Command("ip", "link", "show", link).CombinedOutput(); err == nil {
			t.Errorf("link %s is still there after gcpemu dns uninstall:\n%s", link, out)
		}
		out, _ := exec.Command("resolvectl", "domain").Output()
		for _, s := range dnsSuffixes {
			if slices.Contains(strings.Fields(string(out)), "~"+s) {
				t.Errorf("resolvectl still lists the routing domain ~%s after gcpemu dns uninstall:\n%s", s, out)
			}
		}
	case "windows":
		// The comment `gcpemu dns install` gives its rules (resolverMarker
		// in internal/cli/dns.go).
		comment := "written by gcpemu (" + dnsInstance + ")"
		args := powershellArgs("@(Get-DnsClientNrptRule | Where-Object Comment -eq '" + comment + "').Count")
		out, err := exec.Command(args[0], args[1:]...).CombinedOutput()
		if n := strings.TrimSpace(string(out)); err != nil || n != "0" {
			t.Errorf("NRPT rules commented %q after gcpemu dns uninstall: %s (%v)", comment, n, err)
		}
	}
}
