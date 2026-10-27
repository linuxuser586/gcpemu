//go:build compat

package compat

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/linuxuser586/gcpemu/internal/escapeguard"
	emuruntime "github.com/linuxuser586/gcpemu/internal/runtime"
)

// kubeConformanceWait is how long `sonobuoy run` may take, apart from the
// limit on every other client command.
const kubeConformanceWait = 20 * time.Minute

// TestKubeConformance is Kubernetes conformance (SRS 11.1): for each
// minor that getServerConfig reports, at its newest patch, one after
// another, a one-node Standard cluster from gcloud runs Sonobuoy's quick
// mode (its e2e plugin), which must pass with at least one test passed.
// Sonobuoy runs behind the Escape guard, so a request it sends off the
// machine fails the test; the cluster's Public endpoint, on this machine,
// is exempt. A failed subtest logs the detailed results and keeps the
// retrieved tarball in kubeConformanceResults.
func TestKubeConformance(t *testing.T) {
	sonobuoy := tool(t, "sonobuoy") // skips where none is pinned, native Windows among them
	if _, err := emuruntime.Detect(); err != nil {
		missing(t, "GKE needs a container runtime: %v", err)
	}
	gcloud := onPath(t, "gcloud")
	onPath(t, "gke-gcloud-auth-plugin")
	results := kubeConformanceResults(t)
	e := startWith(t, options{services: "compute,gke"})

	t.Run("escape-guard", func(t *testing.T) {
		(&kubeConformance{e: e, bin: sonobuoy}).canary(t)
	})

	var sc struct {
		ValidMasterVersions []string `json:"validMasterVersions"`
	}
	// The output starts with "Fetching server config for ..." on stderr.
	out := e.run(t, gcloud, "container", "get-server-config", "--zone=us-central1-a", "--format=json")
	if err := json.Unmarshal([]byte(out[max(strings.Index(out, "{"), 0):]), &sc); err != nil {
		t.Fatalf("get-server-config: %v\n%s", err, out)
	}
	versions := newestPatches(sc.ValidMasterVersions)
	if len(versions) == 0 {
		t.Fatalf("get-server-config lists no master versions")
	}
	t.Logf("Kubernetes conformance at %v", versions)
	for _, v := range versions {
		t.Run(gkeMinor(v), func(t *testing.T) {
			name := "kc-" + strings.ReplaceAll(gkeMinor(v), ".", "-")
			describe := func(field string) string {
				t.Helper()
				return strings.TrimSpace(e.run(t, gcloud, "container", "clusters", "describe", name, "--zone=us-central1-a", "--format=value("+field+")"))
			}
			e.run(t, gcloud, "container", "clusters", "create", name, "--zone=us-central1-a", "--cluster-version="+v, "--num-nodes=1")
			deleted := false
			t.Cleanup(func() {
				if !deleted {
					_, _ = e.try(t, gcloud, "container", "clusters", "delete", name, "--zone=us-central1-a")
				}
			})
			e.run(t, gcloud, "container", "clusters", "get-credentials", name, "--zone=us-central1-a")
			if got := describe("currentMasterVersion"); got != v {
				t.Errorf("currentMasterVersion = %q, want %q", got, v)
			}
			k := &kubeConformance{e: e, bin: sonobuoy, endpoint: describe("endpoint")}
			k.verify(t, results, name, v)
			e.run(t, gcloud, "container", "clusters", "delete", name, "--zone=us-central1-a")
			deleted = true
		})
	}
}

// kubeConformanceResults returns the directory a failed subtest leaves its
// Sonobuoy tarball in: $GCPEMU_KUBE_CONFORMANCE_RESULTS, by default
// compat/kube-conformance-results.
func kubeConformanceResults(t *testing.T) string {
	t.Helper()
	d, err := filepath.Abs(cmp.Or(os.Getenv("GCPEMU_KUBE_CONFORMANCE_RESULTS"), "kube-conformance-results"))
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// kubeConformance runs sonobuoy against the cluster the kubeconfig's
// current context names.
type kubeConformance struct {
	e        *emulator
	bin      string
	endpoint string // the cluster's Public endpoint, which the Escape guard lets through
}

// verify runs Sonobuoy's quick mode at version ("1.36.5-gke.100"),
// retrieves the results and fails t unless judgeReport passes them.
func (k *kubeConformance) verify(t *testing.T, results, name, version string) {
	t.Helper()
	// The conformance image's version, not the one Sonobuoy would read off
	// the server, which carries k3s's "+k3s1".
	kube := "v" + strings.SplitN(version, "-", 2)[0]
	start := time.Now()
	out, runErr := k.sonobuoy(t, kubeConformanceWait, "run", "--mode", "quick", "--plugin", "e2e",
		"--kubernetes-version", kube, "--wait="+strconv.Itoa(int(kubeConformanceWait/time.Minute)))
	t.Logf("sonobuoy run on %s (%v)", kube, time.Since(start).Round(time.Second))
	if runErr != nil {
		t.Errorf("sonobuoy run: %v\n%s", runErr, tail(out, 40))
	}
	file := name + ".tar.gz"
	tmp := t.TempDir()
	if out, err := k.sonobuoy(t, 2*time.Minute, "retrieve", tmp, "--filename", file); err != nil {
		// A run that never finished has no results to retrieve.
		status, _ := k.sonobuoy(t, time.Minute, "status")
		t.Fatalf("sonobuoy retrieve: %v\n%s\nsonobuoy status:\n%s", err, tail(out, 40), status)
	}
	tarball := filepath.Join(tmp, file)
	report, err := k.sonobuoy(t, time.Minute, "results", tarball, "--plugin", "e2e")
	if err == nil {
		t.Logf("sonobuoy results:\n%s", report)
		err = judgeReport(report)
	} else {
		err = fmt.Errorf("sonobuoy results: %v\n%s", err, tail(report, 40))
	}
	kept := filepath.Join(results, file)
	_ = os.Remove(kept) // a tarball from an earlier run
	if err == nil && runErr == nil && !t.Failed() {
		return
	}
	if err != nil {
		t.Errorf("Kubernetes conformance on %s: %v", kube, err)
	}
	detailed, _ := k.sonobuoy(t, time.Minute, "results", tarball, "--plugin", "e2e", "--mode", "detailed")
	t.Logf("sonobuoy results --mode detailed (skipped tests left out):\n%s", withoutSkipped(detailed))
	if err := os.MkdirAll(results, 0o755); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(tarball)
	if err == nil {
		err = os.WriteFile(kept, b, 0o644)
	}
	if err != nil {
		t.Fatalf("keeping the results tarball: %v", err)
	}
	t.Logf("results tarball: %s", kept)
}

// sonobuoy runs sonobuoy behind the Escape guard, which lets through only
// loopback and the cluster's Public endpoint, and fails t on each request the
// guard refused. gke-gcloud-auth-plugin runs gcloud under it, so gcloud's
// universe descriptor fetch is not an escape.
func (k *kubeConformance) sonobuoy(t *testing.T, timeout time.Duration, args ...string) (string, error) {
	t.Helper()
	out, escaped, err := k.exec(timeout, args...)
	for _, h := range escapeguard.Gcloud(escaped) {
		t.Errorf("sonobuoy %s: Escaped request to %s", args[0], h)
	}
	return out, err
}

func (k *kubeConformance) exec(timeout time.Duration, args ...string) (string, []string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	env := append(slices.Clip(k.e.env), k.e.guard.Env()...)
	if k.endpoint != "" {
		host := k.endpoint
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		// Later entries win in exec.Cmd.Env.
		noProxy := "127.0.0.1,localhost,::1," + host
		env = append(env, "NO_PROXY="+noProxy, "no_proxy="+noProxy)
	}
	cmd := exec.CommandContext(ctx, k.bin, args...)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		err = fmt.Errorf("timed out after %v: %w", timeout, err)
	}
	return string(out), k.e.guard.Take(), err
}

// kubeConformanceCanaryHost is the API server the escape-guard subtest's
// kubeconfig names: not on this machine, so the guard must refuse it.
const kubeConformanceCanaryHost = "kube-conformance.gcpemu.invalid"

// canary asks for the server version of a cluster at
// kubeConformanceCanaryHost and fails unless the Escape guard refuses the
// request. Without it, a sonobuoy that ignored HTTPS_PROXY would pass
// every run with the guard never seeing a request.
func (k *kubeConformance) canary(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(cfg, []byte(`apiVersion: v1
kind: Config
clusters:
- name: canary
  cluster:
    server: https://`+kubeConformanceCanaryHost+`
contexts:
- name: canary
  context: {cluster: canary, user: canary}
current-context: canary
users:
- name: canary
  user: {token: canary}
`), 0o600); err != nil {
		t.Fatal(err)
	}
	out, escaped, _ := k.exec(time.Minute, "version", "--kubeconfig", cfg)
	if !slices.Equal(escaped, []string{kubeConformanceCanaryHost}) {
		t.Errorf("the Escape guard refused %v, want [%s]; sonobuoy said:\n%s", escaped, kubeConformanceCanaryHost, out)
	}
}

// withoutSkipped drops the skipped tests from `sonobuoy results --mode
// detailed`, one JSON object a line: quick mode skips all but one.
func withoutSkipped(detailed string) string {
	var kept []string
	for _, l := range strings.Split(detailed, "\n") {
		if !strings.Contains(l, `"status":"skipped"`) {
			kept = append(kept, l)
		}
	}
	return strings.Join(kept, "\n")
}

// newestPatches returns the newest version of each minor in versions
// ("1.36.5-gke.100"), oldest minor first.
func newestPatches(versions []string) []string {
	newest := map[string]string{}
	for _, v := range versions {
		m := gkeMinor(v)
		if cur, ok := newest[m]; !ok || slices.Compare(versionNums(v), versionNums(cur)) > 0 {
			newest[m] = v
		}
	}
	out := slices.Collect(maps.Values(newest))
	slices.SortFunc(out, func(a, b string) int { return slices.Compare(versionNums(a), versionNums(b)) })
	return out
}

// judgeReport passes the e2e plugin's report (`sonobuoy results --plugin
// e2e`) when its status is passed and at least one test passed.
func judgeReport(report string) error {
	fields := map[string]string{}
	for _, l := range strings.Split(report, "\n") {
		if k, v, ok := strings.Cut(l, ": "); ok {
			if _, seen := fields[k]; !seen { // the first block is the plugin's
				fields[k] = strings.TrimSpace(v)
			}
		}
	}
	status, ok := fields["Status"]
	if !ok {
		return fmt.Errorf("no e2e plugin status: the plugin did not finish")
	}
	if status != "passed" {
		return fmt.Errorf("e2e plugin status %q, want passed", status)
	}
	passed, err := strconv.Atoi(fields["Passed"])
	if err != nil {
		return fmt.Errorf("e2e plugin report has no passed count")
	}
	if passed == 0 {
		return fmt.Errorf("e2e plugin passed with no test run")
	}
	return nil
}

// gkeMinor returns "1.36" for "1.36.5-gke.100".
func gkeMinor(v string) string {
	p := strings.SplitN(v, ".", 3)
	if len(p) < 2 {
		return v
	}
	return p[0] + "." + p[1]
}

// versionNums returns the numbers of a GKE version: [1 36 5 100] for
// "1.36.5-gke.100".
func versionNums(v string) []int {
	var out []int
	for _, f := range strings.FieldsFunc(v, func(r rune) bool { return r < '0' || r > '9' }) {
		n, _ := strconv.Atoi(f)
		out = append(out, n)
	}
	return out
}
