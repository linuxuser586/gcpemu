//go:build prerelease

package prerelease

import (
	"context"
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

// gcpemuBin is the binary `task build` makes, which the macOS checks run
// on the host.
var gcpemuBin = filepath.Join(repo, "bin", "gcpemu")

// hostInstanceChecks runs the macOS checks on runtime rt: the gcpemu
// binary on the host, with the VM relay, against the runtime's VM.
func hostInstanceChecks(t *testing.T, rt string) {
	done := setup(t, rt)
	requireTools(t, "docker", "task", "go", "gcloud", "gke-gcloud-auth-plugin", "kubectl")
	if _, err := os.Stat(gcpemuBin); err != nil {
		t.Fatalf("%s: %v (task build makes it)", gcpemuBin, err)
	}
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
}

// doctor returns `gcpemu doctor`'s output.
func doctor(t *testing.T) string {
	t.Helper()
	out, _ := exec.Command(gcpemuBin, "doctor").CombinedOutput()
	return string(out)
}

// hostInstance is an Ephemeral Instance of gcpemuBin, detached, in a data
// directory of its own.
type hostInstance struct{ dir string }

// startHost starts an Ephemeral Instance running services, and stops it
// at cleanup.
func startHost(t *testing.T, ctx context.Context, services string) *hostInstance {
	t.Helper()
	h := &hostInstance{dir: filepath.Join(t.TempDir(), "data")}
	out, err := exec.CommandContext(ctx, gcpemuBin, "start", "--detach", "--ephemeral", "--data-dir", h.dir,
		"--port", "0", "--services", services, "--wait-timeout", "300s").CombinedOutput()
	t.Cleanup(func() {
		if out, err := exec.Command(gcpemuBin, "stop", "--data-dir", h.dir).CombinedOutput(); err != nil {
			t.Errorf("gcpemu stop: %v\n%s", err, out)
		}
	})
	if err != nil {
		t.Fatalf("gcpemu start: %v\n%s", err, out)
	}
	return h
}

// env returns what `gcpemu env` prints now: it grows as resources appear.
func (h *hostInstance) env(t *testing.T) string {
	t.Helper()
	out, err := exec.Command(gcpemuBin, "env", "--data-dir", h.dir).Output()
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
		if out, err := exec.Command("sudo", "-n", "true").CombinedOutput(); err != nil {
			t.Fatalf("sudo needs a password: run `sudo -v` first (task prerelease does): %v\n%s", err, out)
		}
		sudo(t, ctx, "install")
		// Only aliases this check added come out again.
		t.Cleanup(func() { sudo(t, context.Background(), "uninstall") })
	}
	requireDoctor(t, doctor(t), aliasesPresent)
}

// twoSQLInstances is macOS step 2: two Cloud SQL instances at once, each on
// a loopback alias of its own, which host clients reach together.
func twoSQLInstances(t *testing.T, ctx context.Context) {
	requireAliases(t, ctx)
	h := startHost(t, ctx, "sql")
	c := newClients(t)
	c.load(hosttest.ParseEnv(h.env(t)))
	for _, name := range []string{"one", "two"} {
		c.run(t, ctx, "gcloud", append([]string{"sql", "instances", "create", name}, sqlFlags...)...)
	}
	for _, name := range []string{"one", "two"} {
		c.run(t, ctx, "gcloud", "sql", "users", "set-password", "postgres", "--instance="+name, "--password=pw")
	}
	env := func() string { return h.env(t) }
	one := hosttest.EnvValue(t, env, "GCPEMU_SQL_ONE")
	two := hosttest.EnvValue(t, env, "GCPEMU_SQL_TWO")
	hostOne, _, _ := net.SplitHostPort(one)
	hostTwo, _, _ := net.SplitHostPort(two)
	if !strings.HasPrefix(hostOne, "127.0.0.") || !strings.HasPrefix(hostTwo, "127.0.0.") || hostOne == hostTwo {
		t.Fatalf("GCPEMU_SQL_ONE=%s and GCPEMU_SQL_TWO=%s, want different 127.0.0.x addresses", one, two)
	}
	// Both connections open before either queries: at the same time.
	connOne, connTwo := pgConnect(t, ctx, one), pgConnect(t, ctx, two)
	selectOne(t, ctx, connOne, "one at "+one)
	selectOne(t, ctx, connTwo, "two at "+two)
}

const aliasesPresent = "[ok  ] loopback aliases 127.0.0.2-127.0.0.254 present"

// sudo runs `sudo gcpemu loopback <verb>`.
func sudo(t *testing.T, ctx context.Context, verb string) {
	t.Helper()
	bin, err := filepath.Abs(gcpemuBin)
	if err != nil {
		t.Fatal(err)
	}
	if out, err := exec.CommandContext(ctx, "sudo", "-n", bin, "loopback", verb).CombinedOutput(); err != nil {
		t.Errorf("sudo gcpemu loopback %s: %v\n%s", verb, err, out)
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
	h := startHost(t, ctx, "gke")
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
