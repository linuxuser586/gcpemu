//go:build compat

package compat

import (
	"cmp"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	emuruntime "github.com/linuxuser586/gcpemu/internal/runtime"
	"github.com/linuxuser586/gcpemu/internal/sonobuoytest"
)

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
		if err := guardedSonobuoy(e, sonobuoy, "").Canary(t.TempDir()); err != nil {
			t.Error(err)
		}
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
			verifyKubeConformance(t, guardedSonobuoy(e, sonobuoy, describe("endpoint")), results, name, v)
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

// guardedSonobuoy runs sonobuoy, behind the Escape guard, against the
// cluster the kubeconfig's current context names, whose Public endpoint, on
// this machine, the guard lets through.
func guardedSonobuoy(e *emulator, bin, endpoint string) sonobuoytest.Guarded {
	return sonobuoytest.Guarded{Bin: bin, Env: e.env, Guard: e.guard, APIServer: endpoint}
}

// verifyKubeConformance runs Kubernetes conformance (sonobuoytest.Quick)
// at version ("1.36.5-gke.100"), keeping a failed run's tarball in
// results.
func verifyKubeConformance(t *testing.T, g sonobuoytest.Guarded, results, name, version string) {
	t.Helper()
	q := sonobuoytest.Quick{
		Sonobuoy: g.Sonobuoy(t.Errorf),
		Keep:     filepath.Join(results, name+".tar.gz"),
		Logf:     t.Logf,
	}
	if err := q.Run(t.TempDir(), version); err != nil {
		t.Error(err)
	}
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
