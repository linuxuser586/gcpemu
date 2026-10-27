//go:build e2e

package e2e

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path"
	"path/filepath"
	goruntime "runtime"
	"testing"

	"github.com/linuxuser586/gcpemu/internal/escapeguard"
	"github.com/linuxuser586/gcpemu/internal/sonobuoytest"
)

// sonobuoy downloads the pinned sonobuoy for the host into the cache
// directory (once), verifying its checksum.
func sonobuoy(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(cacheDir(t), "sonobuoy-"+sonobuoytest.Version)
	name := "sonobuoy"
	if goruntime.GOOS == "windows" {
		name = "sonobuoy.exe"
	}
	bin := filepath.Join(dir, name)
	if _, err := os.Stat(bin); err == nil {
		return bin
	}
	plat := goruntime.GOOS + "/" + goruntime.GOARCH
	want, ok := sonobuoytest.SHA256[plat]
	if !ok {
		t.Skipf("no pinned sonobuoy for %s", plat)
	}
	u := sonobuoytest.URL(plat)
	archive := fetch(t, u)
	if got := sha256.Sum256(archive); hex.EncodeToString(got[:]) != want {
		t.Fatalf("%s: checksum mismatch (got %x, want %s)", u, got, want)
	}
	if err := untar(archive, dir, func(n string) (string, bool) {
		return name, path.Base(n) == name
	}); err != nil {
		t.Fatalf("%s: %v", u, err)
	}
	return bin
}

// kubernetesConformance runs Kubernetes conformance (sonobuoytest.Quick)
// against k's cluster at version ("1.36.5-gke.100"). Sonobuoy runs behind
// the Escape guard, which lets through only loopback and the cluster's API
// server, so a request it sends off the machine fails the test; the
// escape_guard subtest first proves the guard sees Sonobuoy's requests.
func kubernetesConformance(t *testing.T, k *kube, version string) {
	t.Helper()
	g := sonobuoytest.Guarded{
		Bin:       sonobuoy(t),
		Env:       append(os.Environ(), "KUBECONFIG="+k.file),
		Guard:     escapeguard.New(t),
		APIServer: k.endpoint,
	}
	t.Run("escape_guard", func(t *testing.T) {
		if err := g.Canary(t.TempDir()); err != nil {
			t.Error(err)
		}
	})
	q := sonobuoytest.Quick{
		Sonobuoy: g.Sonobuoy(t.Errorf),
		Diagnose: func() string { return k.logs("sonobuoy", "component=sonobuoy") },
		Logf:     t.Logf,
	}
	if err := q.Run(t.TempDir(), version); err != nil {
		t.Error(err)
	}
}
