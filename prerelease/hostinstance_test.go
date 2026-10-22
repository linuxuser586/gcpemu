//go:build prerelease

package prerelease

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
	"time"

	"github.com/linuxuser586/gcpemu/internal/hosttest"
)

// gcpemuBin is the binary `task build` makes, which the macOS checks and
// the Windows Resolver routes check run on the host.
var gcpemuBin = filepath.Join(repo, "bin", "gcpemu"+exeExt())

func exeExt() string {
	if goruntime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}

// requireBin fails t unless gcpemuBin is built.
func requireBin(t *testing.T) {
	t.Helper()
	if _, err := os.Stat(gcpemuBin); err != nil {
		t.Fatalf("%s: %v (task build makes it)", gcpemuBin, err)
	}
}

// hostInstanceChecks runs the macOS checks on runtime rt: the gcpemu
// binary on the host, with the VM relay, against the runtime's VM.
func hostInstanceChecks(t *testing.T, rt string) {
	done := setup(t, rt)
	requireTools(t, "docker", "task", "go", "gcloud", "gke-gcloud-auth-plugin", "kubectl")
	requireBin(t)
	useRuntime(t, rt)
	// A dev build's Sidecar agent, for this runtime's architecture.
	task(t, context.Background(), "agent")
	requireDoctor(t, doctor(t),
		fmt.Sprintf("(chosen by Docker context %q", rt),
		"[ok  ] VM relay: on (")
	done()

	step(t, rt, "1-reference-stack", 45*time.Minute, func(t *testing.T, ctx context.Context) {
		task(t, ctx, "e2e")
	})
	step(t, rt, "2-two-sql-instances", 30*time.Minute, twoSQLInstances)
	step(t, rt, "3-gke-loadbalancer", 30*time.Minute, hostLoadBalancer)
	step(t, rt, "4-vlan", 30*time.Minute, func(t *testing.T, ctx context.Context) { vlan(t, ctx, rt) })
	step(t, rt, "5-host-attachment", 30*time.Minute, hostAttachment)
	step(t, rt, "6-resolver-routes", 30*time.Minute, resolverRoutesStep)
}

// doctor returns `gcpemu doctor`'s output.
func doctor(t *testing.T) string {
	t.Helper()
	out, _ := exec.Command(gcpemuBin, "doctor").CombinedOutput()
	return string(out)
}

// hostInstance is an Ephemeral Instance of gcpemuBin, detached, in a data
// directory of its own, named name unless that is "".
type hostInstance struct{ dir, name string }

// startHost starts an Ephemeral Instance running services, with every
// listener on a free port unless ports (--port values) say otherwise, and
// stops it at cleanup.
func startHost(t *testing.T, ctx context.Context, name, services string, ports ...string) *hostInstance {
	t.Helper()
	h := &hostInstance{dir: filepath.Join(t.TempDir(), "data"), name: name}
	args := []string{"start", "--detach", "--ephemeral", "--services", services, "--wait-timeout", "300s"}
	for _, p := range append([]string{"0"}, ports...) {
		args = append(args, "--port", p)
	}
	out, err := h.gcpemu(ctx, args...)
	t.Cleanup(func() {
		if out, err := h.gcpemu(context.Background(), "stop"); err != nil {
			t.Errorf("gcpemu stop: %v\n%s", err, out)
		}
	})
	if err != nil {
		t.Fatalf("gcpemu start: %v\n%s", err, out)
	}
	return h
}

// flags select this Instance.
func (h *hostInstance) flags() []string {
	if h.name == "" {
		return []string{"--data-dir", h.dir}
	}
	return []string{"--data-dir", h.dir, "--instance", h.name}
}

// gcpemu runs gcpemuBin with args for this Instance and returns its
// output.
func (h *hostInstance) gcpemu(ctx context.Context, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, gcpemuBin, append(args, h.flags()...)...).CombinedOutput()
	return string(out), err
}

// env returns what `gcpemu env` prints now: it grows as resources appear.
func (h *hostInstance) env(t *testing.T) string {
	t.Helper()
	out, err := exec.Command(gcpemuBin, append([]string{"env"}, h.flags()...)...).Output()
	if err != nil {
		t.Fatalf("gcpemu env: %v", err)
	}
	return string(out)
}

// requireAliases installs the loopback aliases on macOS for the rest of t,
// unless they are present already. Linux and Windows route all of
// 127.0.0.0/8 already, and doctor says nothing about aliases there.
func requireAliases(t *testing.T, ctx context.Context) {
	t.Helper()
	if goruntime.GOOS != "darwin" {
		return
	}
	if !strings.Contains(doctor(t), aliasesPresent) {
		requireSudo(t)
		sudo(t, ctx, "install")
		// Only aliases this check added come out again.
		t.Cleanup(func() { sudo(t, context.Background(), "uninstall") })
	}
	requireDoctor(t, doctor(t), aliasesPresent)
}

// twoSQLInstances is macOS step 2: two Cloud SQL instances at once, each
// with a loopback alias of its own as its PRIMARY address, on which host
// clients reach both together. GCPEMU_SQL_<INSTANCE> is a published port
// on 127.0.0.1 whatever the aliases, so it isn't what this checks.
func twoSQLInstances(t *testing.T, ctx context.Context) {
	requireAliases(t, ctx)
	h := startHost(t, ctx, "", "sql")
	c := newClients(t)
	c.load(hosttest.ParseEnv(h.env(t)))
	for _, name := range []string{"one", "two"} {
		c.run(t, ctx, "gcloud", append([]string{"sql", "instances", "create", name}, sqlFlags...)...)
	}
	for _, name := range []string{"one", "two"} {
		c.run(t, ctx, "gcloud", "sql", "users", "set-password", "postgres", "--instance="+name, "--password=pw")
	}
	one, two := c.primaryIP(t, ctx, "one"), c.primaryIP(t, ctx, "two")
	if !loopbackAlias(one) || !loopbackAlias(two) || one == two {
		t.Fatalf("the PRIMARY addresses of one and two are %s and %s, want different 127.0.0.x other than 127.0.0.1", one, two)
	}
	// Both connections open before either queries: at the same time.
	one, two = net.JoinHostPort(one, "5432"), net.JoinHostPort(two, "5432")
	connOne, connTwo := pgConnect(t, ctx, one), pgConnect(t, ctx, two)
	selectOne(t, ctx, connOne, "one at "+one)
	selectOne(t, ctx, connTwo, "two at "+two)
}

// primaryIP returns Cloud SQL instance name's PRIMARY address.
func (c *clients) primaryIP(t *testing.T, ctx context.Context, name string) string {
	t.Helper()
	var inst struct {
		IPAddresses []struct{ Type, IPAddress string }
	}
	out := c.run(t, ctx, "gcloud", "sql", "instances", "describe", name, "--format=json")
	if err := json.Unmarshal([]byte(out), &inst); err != nil {
		t.Fatalf("gcloud sql instances describe %s: %v\n%s", name, err, out)
	}
	for _, a := range inst.IPAddresses {
		if a.Type == "PRIMARY" {
			return a.IPAddress
		}
	}
	t.Fatalf("Cloud SQL instance %s has no PRIMARY address:\n%s", name, out)
	return ""
}

const aliasesPresent = "[ok  ] loopback aliases 127.0.0.2-127.0.0.254 present"

// sudo runs `sudo gcpemu loopback <verb> --yes`: without a terminal to
// confirm on, it would abort.
func sudo(t *testing.T, ctx context.Context, verb string) {
	t.Helper()
	bin, err := filepath.Abs(gcpemuBin)
	if err != nil {
		t.Fatal(err)
	}
	if out, err := exec.CommandContext(ctx, "sudo", "-n", bin, "loopback", verb, "--yes").CombinedOutput(); err != nil {
		t.Errorf("sudo gcpemu loopback %s --yes: %v\n%s", verb, err, out)
	}
}

// keepSudo keeps sudo's timestamp, which `task prerelease` takes before
// the run, alive for the run on macOS, so loopback install and uninstall
// don't ask for a password halfway through.
func keepSudo() (stop func()) {
	if goruntime.GOOS != "darwin" {
		return func() {}
	}
	done := make(chan struct{})
	go func() {
		tick := time.NewTicker(time.Minute)
		defer tick.Stop()
		for {
			select {
			case <-done:
				return
			case <-tick.C:
				_ = exec.Command("sudo", "-n", "-v").Run()
			}
		}
	}()
	return func() { close(done) }
}

// hostLoadBalancer is macOS step 3: a GKE LoadBalancer Service reached
// from the host at the loopback address it reports.
func hostLoadBalancer(t *testing.T, ctx context.Context) {
	h := startHost(t, ctx, "", "gke")
	c := newClients(t)
	c.load(hosttest.ParseEnv(h.env(t)))
	c.run(t, ctx, "gcloud", "container", "clusters", "create", "lb", "--zone="+zone, "--num-nodes=1")
	c.run(t, ctx, "gcloud", "container", "clusters", "get-credentials", "lb", "--zone="+zone)
	ip := c.lbService(t, ctx)
	if !net.ParseIP(ip).IsLoopback() {
		t.Fatalf("Service web's EXTERNAL-IP is %s, want a loopback address", ip)
	}
	addr := hosttest.EnvValue(t, func() string { return h.env(t) }, "GCPEMU_GKE_LB_DEFAULT_WEB_80")
	if host, _, _ := net.SplitHostPort(addr); host != ip {
		t.Errorf("GCPEMU_GKE_LB_DEFAULT_WEB_80=%s, not at the Service's EXTERNAL-IP %s", addr, ip)
	}
	getBody(t, "http://"+addr+"/", "Welcome to nginx")
}

// vlanTests make a Translated network, which needs VLAN (8021q) support.
var vlanTests = []string{"TestEgressThroughNATTranslated", "TestTranslatedNetworkSurvivesRestart"}

// vlan is macOS step 4: it runs the Translated network tests, which pass
// on a runtime with VLAN support and fail on one without it with an error
// naming the runtime, and records which in README.md's runtime table.
func vlan(t *testing.T, ctx context.Context, rt string) {
	row := tableRow(rt)
	if row == "" {
		t.Logf("README.md's runtime table has no row for %s; the result is not recorded there", rt)
	}
	runs, out := goTests(ctx, vlanTests, "./services/compute")
	supported := true
	for _, name := range vlanTests {
		log := runs[name].log
		switch runs[name].ended {
		case "pass":
		case "fail":
			if !strings.Contains(log, "has no VLAN (8021q) support") {
				t.Fatalf("%s failed, but not for want of VLAN support:\n%s", name, log)
			}
			supported = false
		case "skip":
			t.Fatalf("%s skipped, so the VLAN check didn't run:\n%s", name, log)
		default:
			t.Fatalf("%s didn't run:\n%s", name, out)
		}
	}
	summary.Lock()
	v := summary.versions[rt]
	summary.Unlock()
	cell := "supported"
	if !supported {
		cell = "not supported"
	}
	cell += " (" + v + ")"
	t.Logf("%s: VLAN (8021q) %s", rt, cell)
	if row != "" {
		if err := setTableRow(filepath.Join(repo, "README.md"), row, cell); err != nil {
			t.Fatal(err)
		}
	}
}

// setTableRow sets the VLAN cell of row in README.md's runtime table.
func setTableRow(path, row, cell string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	lines := strings.Split(string(b), "\n")
	header := false
	for i, l := range lines {
		if strings.HasPrefix(l, "| Runtime | VLAN (8021q) support |") {
			header = true
		}
		if header && strings.HasPrefix(l, "| "+row+" |") {
			lines[i] = "| " + row + " | " + cell + " |"
			return os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o644)
		}
	}
	return fmt.Errorf("%s: no %q row in the runtime table", path, row)
}
