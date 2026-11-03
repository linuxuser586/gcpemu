//go:build prerelease

package prerelease

import (
	"context"
	"encoding/json"
	"errors"
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
	"github.com/linuxuser586/gcpemu/internal/instance"
	"github.com/linuxuser586/gcpemu/internal/runtime"
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
	step(t, rt, "7-vm-relay", 15*time.Minute, vmRelay)
	step(t, rt, "8-registry-relay", 15*time.Minute, func(t *testing.T, ctx context.Context) { registryRelay(t, ctx, rt) })
	step(t, rt, "9-containerized-registry", 30*time.Minute, func(t *testing.T, ctx context.Context) { containerizedRegistryStep(t, ctx, rt) })
}

// doctor returns `gcpemu doctor`'s output.
func doctor(t *testing.T) string {
	t.Helper()
	out, _ := exec.Command(gcpemuBin, "doctor").CombinedOutput()
	return string(out)
}

// testDirRoot holds the Test data dirs (ADR 0057) of the Instances the
// checks start with gcpemuBin, so that a run finds those a killed run
// left behind (#921).
var testDirRoot = filepath.Join(os.TempDir(), "gcpemu-prerelease")

// detectRuntime returns a client for the runtime of the current docker
// context; tests fake it.
var detectRuntime = runtime.Detect

// hostInstance is an Ephemeral Instance of gcpemuBin, detached, in a Test
// data dir of its own under testDirRoot, named name unless that is "".
type hostInstance struct {
	dir, name string
	// release lets go of the dir's owner lock.
	release func()
}

// newHostInstance makes a Test data dir for an Instance named name and
// takes its owner lock, which end lets go of. The lock is not inherited
// by gcpemuBin's processes, so it ends with the test's.
func newHostInstance(name string) (*hostInstance, error) {
	if err := os.MkdirAll(testDirRoot, 0o700); err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp(testDirRoot, "data-")
	if err != nil {
		return nil, err
	}
	release, err := instance.HoldOwnerLock(dir)
	if err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	return &hostInstance{dir: dir, name: name, release: release}, nil
}

// startHost starts an Ephemeral Instance running services, with every
// listener on a free port unless ports (--port values) say otherwise, and
// ends it at cleanup.
func startHost(t *testing.T, ctx context.Context, name, services string, ports ...string) *hostInstance {
	t.Helper()
	h, err := newHostInstance(name)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		if err := h.end(ctx); err != nil {
			t.Error(err)
		}
	})
	args := []string{"start", "--detach", "--ephemeral", "--services", services, "--wait-timeout", "300s"}
	for _, p := range append([]string{"0"}, ports...) {
		args = append(args, "--port", p)
	}
	if out, err := h.gcpemu(ctx, args...); err != nil {
		t.Fatalf("gcpemu start: %v\n%s", err, out)
	}
	return h
}

// end stops the Instance with gcpemu stop, then lets go of the dir's
// owner lock and reclaims the dir as the next run would: a process still
// running is killed if it is gcpemu, and the Instance's containers,
// networks and volumes and the dir are removed, so that none outlives the
// check even when stop fails (#921). A dir the reclaim keeps, because it
// holds no Instance ID, is removed too.
func (h *hostInstance) end(ctx context.Context) error {
	var errs []error
	if out, err := h.gcpemu(ctx, "stop"); err != nil {
		errs = append(errs, fmt.Errorf("gcpemu stop: %w\n%s", err, out))
	}
	// The reclaim takes the lock itself, which proves the test has ended; a
	// reclaim in another process that takes it first does the same.
	h.release()
	cl, err := detectRuntime()
	if err == nil {
		_, err = instance.ReclaimEndedDir(ctx, cl, h.dir)
	}
	if err != nil {
		// The dir keeps the Instance ID, so the next run reclaims it.
		return errors.Join(append(errs, fmt.Errorf("reclaiming %s: %w", h.dir, err))...)
	}
	_ = os.RemoveAll(h.dir)
	return errors.Join(errs...)
}

// reclaimEndedTests is the run-start reclaim (ADR 0057): before the
// first check it reclaims the Instances of ended tests on each runtime in
// rts, those that a killed run left behind among them.
func reclaimEndedTests(t *testing.T, rts []string) {
	t.Helper()
	// Detection reads the docker context from the environment; the
	// caller's comes back after.
	for _, k := range []string{"DOCKER_HOST", "DOCKER_CONTEXT"} {
		if v, ok := os.LookupEnv(k); ok {
			defer os.Setenv(k, v)
		} else {
			defer os.Unsetenv(k)
		}
	}
	cls := make([]*runtime.Client, len(rts))
	for i, rt := range rts {
		os.Setenv("DOCKER_HOST", "")
		os.Setenv("DOCKER_CONTEXT", rt)
		cls[i], _ = detectRuntime()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := reclaimEnded(ctx, testDirRoot, cls); err != nil {
		t.Logf("reclaiming the Instances of ended tests: %v", err)
	}
}

// reclaimEnded reclaims every Test data dir whose test has ended that an
// object on one of cls records, then each under root that none records,
// such as one whose Instance made no object (#921). A nil client is a
// runtime that did not answer; while there is one, or a runtime fails to
// list its objects, the dirs that no object records are kept: their
// objects may be there, and only their dir records whose they are.
func reclaimEnded(ctx context.Context, root string, cls []*runtime.Client) error {
	var errs []error
	recorded := map[string]bool{}
	complete := len(cls) > 0
	for _, cl := range cls {
		if cl == nil {
			complete = false
			continue
		}
		tagged, err := instance.FindTagged(ctx, cl)
		if err != nil {
			errs = append(errs, err)
			complete = false
			continue
		}
		for _, a := range tagged {
			recorded[a.ID] = true
			for _, dir := range a.Dirs {
				if _, err := instance.ReclaimEnded(ctx, cl, a, dir); err != nil {
					errs = append(errs, fmt.Errorf("Instance %s in %s: %w", a.ID, dir, err))
				}
			}
		}
	}
	if !complete {
		return errors.Join(errs...)
	}
	// Glob fails only on a malformed pattern.
	dirs, _ := filepath.Glob(filepath.Join(root, "*"))
	for _, dir := range dirs {
		id, _ := os.ReadFile(filepath.Join(dir, instance.IDFile))
		if recorded[string(id)] {
			continue // a reclaim above failed, or its test still runs
		}
		if _, err := instance.ReclaimEndedDir(ctx, cls[0], dir); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", dir, err))
		}
	}
	return errors.Join(errs...)
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
