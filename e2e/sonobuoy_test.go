//go:build e2e

package e2e

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
	"time"
)

// Kubernetes conformance (SRS 11.4) runs Sonobuoy's quick mode, its e2e
// plugin only: the systemd-logs plugin's DaemonSet is privileged and
// mounts host paths, which Autopilot refuses, as GKE does.

// sonobuoyVersion is the pinned Sonobuoy release; sonobuoySHA256 its
// archive checksums (sonobuoy_<version>_checksums.txt).
const sonobuoyVersion = "0.57.5"

var sonobuoySHA256 = map[string]string{
	"linux_amd64":   "7c5c2250e5103c98f4dad2bcab86baf954e319c57c419c113f28f5060e62f129",
	"linux_arm64":   "ec482b5d1ec237f4c94b6fd7be5a69f95678a4331001b3edcb6ad7835ec40695",
	"darwin_amd64":  "4202d3a960a812e68b555829eefbe0cc25de8b2e5cb2fc8e5cc38dfc83515a9d",
	"darwin_arm64":  "d889c4a4485f3899f8d860506ed4da5380372f23f91cd0113e52f036d842e978",
	"windows_amd64": "041500d35af9720cb8e0ec3584b32243e6555c1e7413caadf9ddb8c336354f63",
}

// sonobuoy downloads the pinned sonobuoy for the host into the cache
// directory (once), verifying its checksum.
func sonobuoy(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(cacheDir(t), "sonobuoy-"+sonobuoyVersion)
	name := "sonobuoy"
	if goruntime.GOOS == "windows" {
		name = "sonobuoy.exe"
	}
	bin := filepath.Join(dir, name)
	if _, err := os.Stat(bin); err == nil {
		return bin
	}
	plat := goruntime.GOOS + "_" + goruntime.GOARCH
	want, ok := sonobuoySHA256[plat]
	if !ok {
		t.Skipf("no pinned sonobuoy for %s", plat)
	}
	asset := "sonobuoy_" + sonobuoyVersion + "_" + plat + ".tar.gz"
	archive := fetch(t, "https://github.com/vmware-tanzu/sonobuoy/releases/download/v"+sonobuoyVersion+"/"+asset)
	if got := sha256.Sum256(archive); hex.EncodeToString(got[:]) != want {
		t.Fatalf("%s: checksum mismatch (got %x, want %s)", asset, got, want)
	}
	if err := untar(archive, dir, func(n string) (string, bool) {
		return name, path.Base(n) == name
	}); err != nil {
		t.Fatalf("%s: %v", asset, err)
	}
	return bin
}

// kubernetesConformance runs Sonobuoy's quick mode against k's cluster, whose
// Kubernetes version is version ("1.36.5"), and fails unless the e2e
// plugin passes.
func kubernetesConformance(t *testing.T, k *kube, version string) {
	t.Helper()
	bin := sonobuoy(t)
	run := func(timeout time.Duration, args ...string) string {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		out, err := exec.CommandContext(ctx, bin, args...).CombinedOutput()
		if err != nil {
			t.Fatalf("sonobuoy %s: %v\n%s\n%s", args[0], err, tail(string(out), 40), k.logs("sonobuoy", "component=sonobuoy"))
		}
		return string(out)
	}
	start := time.Now()
	// The Kubernetes conformance image is the server's version without k3s's suffix.
	run(25*time.Minute, "run", "--mode", "quick", "--plugin", "e2e", "--kubernetes-version", "v"+version,
		"--wait=20", "--kubeconfig", k.file)
	out := strings.Fields(run(2*time.Minute, "retrieve", t.TempDir(), "--kubeconfig", k.file))
	if len(out) == 0 {
		t.Fatal("sonobuoy retrieve: no results archive")
	}
	results := run(time.Minute, "results", out[len(out)-1])
	t.Logf("sonobuoy quick mode on %s (%v):\n%s", version, time.Since(start).Round(time.Second), results)
	if !strings.Contains(results, "Status: passed") || !strings.Contains(results, "Failed: 0") {
		t.Errorf("Kubernetes conformance on %s did not pass:\n%s", version, run(time.Minute, "results", out[len(out)-1], "--mode", "detailed"))
	}
}
