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
	"testing"
	"time"

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
// against k's cluster at version ("1.36.5-gke.100").
func kubernetesConformance(t *testing.T, k *kube, version string) {
	t.Helper()
	bin := sonobuoy(t)
	q := sonobuoytest.Quick{
		Sonobuoy: func(timeout time.Duration, args ...string) (string, error) {
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			cmd := exec.CommandContext(ctx, bin, args...)
			cmd.Env = append(os.Environ(), "KUBECONFIG="+k.file)
			out, err := cmd.CombinedOutput()
			return string(out), err
		},
		Diagnose: func() string { return k.logs("sonobuoy", "component=sonobuoy") },
		Logf:     t.Logf,
	}
	if err := q.Run(t.TempDir(), version); err != nil {
		t.Error(err)
	}
}
