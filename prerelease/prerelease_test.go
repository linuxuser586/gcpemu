//go:build prerelease

package prerelease

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	goruntime "runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/linuxuser586/gcpemu/internal/hosttest"
)

// The variables `task prerelease` passes RUNTIMES and IMAGE in.
const (
	runtimesEnv = "GCPEMU_PRERELEASE_RUNTIMES"
	imageEnv    = "GCPEMU_PRERELEASE_IMAGE"
)

const (
	repo    = ".."
	project = "prerelease"
	region  = "us-central1"
	zone    = region + "-a"
)

// sqlFlags create the checklist's Cloud SQL instances. db-f1-micro is an
// ENTERPRISE tier, and POSTGRES_16 defaults to ENTERPRISE_PLUS. Host
// clients connect from 127.0.0.1, which a connection straight to the
// public IP needs in the authorized networks; the connectors don't.
var sqlFlags = []string{"--database-version=POSTGRES_16", "--edition=ENTERPRISE", "--tier=db-f1-micro", "--region=" + region,
	"--authorized-networks=127.0.0.1/32"}

// required lists the Required runtimes of each OS, by docker context.
var required = map[string][]string{
	"darwin":  {"desktop-linux", "colima"},
	"windows": {"desktop-linux"},
}

func isRequired(rt string) bool { return slices.Contains(required[goruntime.GOOS], rt) }

// runtimes returns the docker contexts to check: RUNTIMES, or the
// Required runtimes of this OS.
func runtimes() []string {
	if v := os.Getenv(runtimesEnv); v != "" {
		return strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ' ' })
	}
	return required[goruntime.GOOS]
}

// TestHostChecks runs the suite of this OS on each runtime in turn.
func TestHostChecks(t *testing.T) {
	rts := runtimes()
	if len(rts) == 0 {
		t.Skipf("no runtime to check on %s: name VM runtimes' docker contexts in RUNTIMES", goruntime.GOOS)
	}
	suite := hostInstanceChecks
	if goruntime.GOOS == "windows" {
		suite = containerizedChecks
	}
	for _, rt := range rts {
		t.Run(rt, func(t *testing.T) { suite(t, rt) })
	}
}

// ---- The summary ----

type result struct{ runtime, step, outcome string }

var summary struct {
	sync.Mutex
	commit   string
	results  []result
	versions map[string]string
}

func record(rt, step, outcome string) {
	summary.Lock()
	defer summary.Unlock()
	summary.results = append(summary.results, result{rt, step, outcome})
}

func outcome(t *testing.T) string {
	switch {
	case t.Failed():
		return "fail"
	case t.Skipped():
		return "skip"
	}
	return "pass"
}

// step runs fn as subtest name of runtime rt, with its own deadline, and
// records how it ended. It reports whether it passed or skipped.
func step(t *testing.T, rt, name string, timeout time.Duration, fn func(t *testing.T, ctx context.Context)) bool {
	return t.Run(name, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		defer func() { record(rt, name, outcome(t)) }()
		fn(t, ctx)
	})
}

// setup records the setup of runtime rt as its first row: a fail or skip
// when t ends before done is called, a pass once it is.
func setup(t *testing.T, rt string) (done func()) {
	finished := false
	t.Cleanup(func() {
		if !finished {
			record(rt, "setup", outcome(t))
		}
	})
	return func() {
		finished = true
		record(rt, "setup", "pass")
	}
}

func TestMain(m *testing.M) {
	summary.commit = commit()
	summary.versions = map[string]string{}
	stop := keepSudo()
	code := m.Run()
	stop()
	if report() && code == 0 {
		code = 1
	}
	os.Exit(code)
}

// commit names the commit under test, with "-dirty" for local changes.
func commit() string {
	out, err := exec.Command("git", "-C", repo, "rev-parse", "--short", "HEAD").Output()
	if err != nil {
		return "unknown"
	}
	c := strings.TrimSpace(string(out))
	if st, _ := exec.Command("git", "-C", repo, "status", "--porcelain").Output(); len(st) > 0 {
		c += "-dirty"
	}
	return c
}

// report prints the summary and writes it to dist/. It reports whether a
// Required runtime was skipped, which fails the run: a release waits for
// every Required runtime.
func report() (requiredSkipped bool) {
	summary.Lock()
	defer summary.Unlock()
	if len(summary.results) == 0 {
		return false
	}
	var rts, steps []string
	cell := map[[2]string]string{}
	for _, r := range summary.results {
		if !slices.Contains(rts, r.runtime) {
			rts = append(rts, r.runtime)
		}
		if !slices.Contains(steps, r.step) {
			steps = append(steps, r.step)
		}
		cell[[2]string{r.runtime, r.step}] = r.outcome
		if r.outcome == "skip" && r.step == "setup" && isRequired(r.runtime) {
			requiredSkipped = true
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# Pre-release host checks\n\n%s/%s, commit %s, %s\n\n", goruntime.GOOS, goruntime.GOARCH, summary.commit, time.Now().UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "| Runtime | Version | %s |\n|---|---|%s\n", strings.Join(steps, " | "), strings.Repeat("---|", len(steps)))
	for _, rt := range rts {
		kind := "best-effort"
		if isRequired(rt) {
			kind = "required"
		}
		fmt.Fprintf(&b, "| %s (%s) | %s |", rt, kind, summary.versions[rt])
		for _, s := range steps {
			fmt.Fprintf(&b, " %s |", cmp.Or(cell[[2]string{rt, s}], "-"))
		}
		b.WriteByte('\n')
	}
	if requiredSkipped {
		b.WriteString("\nA Required runtime was skipped, so the release is not checked.\n")
	}
	fmt.Print("\n" + b.String())
	name := filepath.Join("dist", fmt.Sprintf("prerelease-%s-%s.md", goruntime.GOOS, summary.commit))
	path := filepath.Join(repo, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err == nil {
		if err := os.WriteFile(path, []byte(b.String()), 0o644); err == nil {
			fmt.Printf("\nwritten to %s\n", name)
		}
	}
	return requiredSkipped
}

// ---- Runtimes ----

// useRuntime points docker and gcpemu at the docker context rt for the
// rest of t, and records the runtime's version. A Required runtime that
// is missing or not running fails t; a Best-effort one skips it.
func useRuntime(t *testing.T, rt string) {
	t.Helper()
	missing := t.Skipf
	if isRequired(rt) {
		missing = t.Fatalf
	}
	if out, err := exec.Command("docker", "context", "inspect", rt).CombinedOutput(); err != nil {
		missing("runtime %s: no docker context %q: %v\n%s", rt, rt, err, out)
	}
	t.Setenv("DOCKER_HOST", "")
	t.Setenv("DOCKER_CONTEXT", rt)
	if out, err := exec.Command("docker", "info", "--format", "{{.OperatingSystem}}").CombinedOutput(); err != nil {
		missing("runtime %s does not answer: %v\n%s", rt, err, out)
	}
	v := runtimeVersion(rt)
	summary.Lock()
	summary.versions[rt] = v
	summary.Unlock()
	t.Logf("runtime %s: %s", rt, v)
}

var versionRE = regexp.MustCompile(`\d+\.\d+(\.\d+)?`)

// runtimeVersion is the version of the runtime behind docker context rt
// (Docker Desktop's, Colima's, ...), or its engine's when that is all
// there is to go by.
func runtimeVersion(rt string) string {
	if out, err := exec.Command("docker", "version", "--format", "{{.Server.Platform.Name}}").Output(); err == nil {
		if name := strings.TrimSpace(string(out)); strings.HasPrefix(name, "Docker Desktop") {
			if v := versionRE.FindString(name); v != "" {
				return v
			}
		}
	}
	var cmd []string
	switch tableRow(rt) {
	case "Colima":
		cmd = []string{"colima", "version"}
	case "OrbStack":
		cmd = []string{"orbctl", "version"}
	case "Podman machine":
		cmd = []string{"podman", "version", "--format", "{{.Server.Version}}"}
	}
	if cmd != nil {
		if out, err := exec.Command(cmd[0], cmd[1:]...).Output(); err == nil {
			if v := versionRE.FindString(string(out)); v != "" {
				return v
			}
		}
	}
	out, _ := exec.Command("docker", "version", "--format", "{{.Server.Version}}").Output()
	return "engine " + strings.TrimSpace(string(out))
}

// tableRow is the row of README.md's runtime table for docker context
// rt, or "" when it has none.
func tableRow(rt string) string {
	switch {
	case rt == "desktop-linux":
		return "Docker Desktop"
	case strings.HasPrefix(rt, "colima"):
		return "Colima"
	case rt == "orbstack":
		return "OrbStack"
	case strings.Contains(rt, "podman"):
		return "Podman machine"
	}
	return ""
}

// requireDoctor fails t unless every line in want appears in doctor's
// output, whatever it exits with: a check that fails elsewhere on the
// host is not this caller's concern.
func requireDoctor(t *testing.T, out string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Fatalf("gcpemu doctor does not report %q:\n%s", w, out)
		}
	}
}

// ---- Clients ----

// requireTools fails t, naming every tool in names that is not on PATH.
func requireTools(t *testing.T, names ...string) {
	t.Helper()
	var missing []string
	for _, n := range names {
		if _, err := exec.LookPath(n); err != nil {
			missing = append(missing, n)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("not on PATH: %s", strings.Join(missing, ", "))
	}
}

// clients runs the host's clients in an environment of their own: a
// gcloud configuration with the default principal's token, and a
// kubeconfig of their own, under the variables `gcpemu env` printed.
type clients struct {
	dir  string
	vars map[string]string
}

func newClients(t *testing.T) *clients {
	t.Helper()
	dir := t.TempDir()
	// Under Python 3.12, gcloud storage cp matches no file for a path
	// with 8.3 short names (RUNNER~1).
	if d, err := filepath.EvalSymlinks(dir); err == nil {
		dir = d
	}
	token := filepath.Join(dir, "token")
	if err := os.WriteFile(token, []byte("owner"), 0o600); err != nil {
		t.Fatal(err)
	}
	return &clients{dir: dir, vars: map[string]string{
		"CLOUDSDK_CONFIG":                                 filepath.Join(dir, "gcloud"),
		"CLOUDSDK_CORE_PROJECT":                           project,
		"CLOUDSDK_CORE_DISABLE_PROMPTS":                   "1",
		"CLOUDSDK_CORE_DISABLE_USAGE_REPORTING":           "1",
		"CLOUDSDK_COMPONENT_MANAGER_DISABLE_UPDATE_CHECK": "1",
		"CLOUDSDK_AUTH_ACCESS_TOKEN_FILE":                 token,
		"KUBECONFIG":                                      filepath.Join(dir, "kubeconfig"),
	}}
}

// load takes in the variables `gcpemu env` printed, as eval or
// Invoke-Expression would, keeping the clients' own configuration.
func (c *clients) load(env map[string]string) {
	for k, v := range env {
		switch k {
		case "CLOUDSDK_CONFIG", "CLOUDSDK_CORE_PROJECT", "KUBECONFIG":
			continue
		}
		c.vars[k] = v
	}
}

// environ is the clients' environment, without the variables in unset.
func (c *clients) environ(unset ...string) []string {
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if _, ok := c.vars[k]; !ok && !slices.Contains(unset, k) {
			env = append(env, kv)
		}
	}
	for k, v := range c.vars {
		if !slices.Contains(unset, k) {
			env = append(env, k+"="+v)
		}
	}
	return env
}

// run runs a client and fails t on error.
func (c *clients) run(t *testing.T, ctx context.Context, bin string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = c.environ()
	cmd.Dir = c.dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", bin, strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// lbService deploys nginx behind LoadBalancer Service web on port 80, as
// the checklist does, and returns the Service's ingress IP.
func (c *clients) lbService(t *testing.T, ctx context.Context) string {
	t.Helper()
	c.run(t, ctx, "kubectl", "create", "deployment", "web", "--image=nginx:1.29-alpine")
	c.run(t, ctx, "kubectl", "expose", "deployment", "web", "--type=LoadBalancer", "--port=80")
	var ip string
	hosttest.Eventually(t, 3*time.Minute, "an EXTERNAL-IP for Service web", func() bool {
		ip = c.run(t, ctx, "kubectl", "get", "service", "web", "-o", "jsonpath={.status.loadBalancer.ingress[0].ip}")
		return ip != ""
	}, func() string { return c.run(t, ctx, "kubectl", "get", "service", "web", "-o", "wide") })
	return ip
}

// ---- Checks from the host ----

// getBody waits for url to answer 200 with a body containing want, and
// returns the body.
func getBody(t *testing.T, url, want string) string {
	t.Helper()
	var body string
	hosttest.Eventually(t, 2*time.Minute, want+" at "+url, func() bool {
		resp, err := (&http.Client{Timeout: 5 * time.Second}).Get(url)
		if err != nil {
			body = err.Error()
			return false
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		body = string(b)
		return resp.StatusCode == http.StatusOK && strings.Contains(body, want)
	}, func() string { return "last answer: " + body })
	return body
}

// pgConnect connects to PostgreSQL at addr as postgres with password pw.
func pgConnect(t *testing.T, ctx context.Context, addr string) *pgx.Conn {
	t.Helper()
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		host, port = addr, "5432"
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, fmt.Sprintf("host=%s port=%s user=postgres password=pw dbname=postgres sslmode=disable", host, port))
	if err != nil {
		t.Fatalf("connecting to %s: %v", addr, err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	return conn
}

// selectOne runs `select 1` on conn.
func selectOne(t *testing.T, ctx context.Context, conn *pgx.Conn, what string) {
	t.Helper()
	var n int
	if err := conn.QueryRow(ctx, "select 1").Scan(&n); err != nil || n != 1 {
		t.Fatalf("select 1 on %s: %d, %v", what, n, err)
	}
}

// loopbackAlias reports whether addr's host is a 127.0.0.x other than
// 127.0.0.1.
func loopbackAlias(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	ip := net.ParseIP(host).To4()
	return ip != nil && ip[0] == 127 && ip[1] == 0 && ip[2] == 0 && ip[3] > 1
}

// inRange reports whether addr's port lies in "LO-HI".
func inRange(addr, r string) bool {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	var p, lo, hi int
	fmt.Sscan(port, &p)
	if _, err := fmt.Sscanf(r, "%d-%d", &lo, &hi); err != nil {
		return false
	}
	return p >= lo && p <= hi
}

// task runs a Task task from the repository's root, streaming its output.
func task(t *testing.T, ctx context.Context, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(ctx, "task", args...)
	cmd.Dir = repo
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("task %s: %v", strings.Join(args, " "), err)
	}
}
