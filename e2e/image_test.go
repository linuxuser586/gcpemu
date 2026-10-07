//go:build e2e

package e2e

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

// imageArches are the platforms of the app image (the host's first, so a
// failure to cross-compile the others never blocks the test).
func imageArches() []string {
	arches := []string{goruntime.GOARCH}
	for _, a := range []string{"amd64", "arm64"} {
		if a != goruntime.GOARCH {
			arches = append(arches, a)
		}
	}
	return arches
}

// buildAppImage cross-compiles ./app (static, CGO_ENABLED=0) for every
// arch and assembles a multi-arch image index FROM scratch: one layer with
// the binary, no shell, no CA certificates (the emulator injects its CA
// bundle into pods, FR-INT-007).
func buildAppImage(t *testing.T) v1.ImageIndex {
	t.Helper()
	idx := mutate.IndexMediaType(empty.Index, types.OCIImageIndex)
	for _, arch := range imageArches() {
		bin := filepath.Join(t.TempDir(), "app-"+arch)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		cmd := exec.CommandContext(ctx, "go", "build", "-trimpath", "-ldflags", "-s -w", "-o", bin, "./app")
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+arch)
		out, err := cmd.CombinedOutput()
		cancel()
		if err != nil {
			t.Fatalf("go build app (%s): %v\n%s", arch, err, out)
		}
		img := appImage(t, bin, arch)
		idx = mutate.AppendManifests(idx, mutate.IndexAddendum{Add: img,
			Descriptor: v1.Descriptor{Platform: &v1.Platform{OS: "linux", Architecture: arch}}})
	}
	return idx
}

func appImage(t *testing.T, bin, arch string) v1.Image {
	t.Helper()
	data, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	var tb bytes.Buffer
	tw := tar.NewWriter(&tb)
	_ = tw.WriteHeader(&tar.Header{Name: "app", Mode: 0o755, Size: int64(len(data)), Typeflag: tar.TypeReg, ModTime: time.Unix(0, 0)})
	_, _ = tw.Write(data)
	_ = tw.Close()
	layer, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(tb.Bytes())), nil },
		tarball.WithMediaType(types.OCILayer))
	if err != nil {
		t.Fatal(err)
	}
	img := mutate.MediaType(empty.Image, types.OCIManifestSchema1)
	img = mutate.ConfigMediaType(img, types.OCIConfigJSON)
	if img, err = mutate.AppendLayers(img, layer); err != nil {
		t.Fatal(err)
	}
	cf, err := img.ConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	cf = cf.DeepCopy()
	cf.OS, cf.Architecture = "linux", arch
	cf.Config = v1.Config{Entrypoint: []string{"/app"}, User: "65532:65532", Env: []string{"PORT=8080"},
		ExposedPorts: map[string]struct{}{"8080/tcp": {}}}
	if img, err = mutate.ConfigFile(img, cf); err != nil {
		t.Fatal(err)
	}
	return img
}

// pushIndex pushes idx to the emulated Artifact Registry as ref
// (LOCATION-docker.pkg.dev/PROJECT/REPO/IMAGE:TAG) with an access token,
// like `docker push` after `gcloud auth configure-docker`.
func pushIndex(t *testing.T, registry, ref, token string, idx v1.ImageIndex) {
	t.Helper()
	r, err := name.ParseReference(registry+"/"+ref, name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.WriteIndex(r, idx, remote.WithAuth(&authn.Basic{Username: "oauth2accesstoken", Password: token})); err != nil {
		t.Fatalf("push %s: %v", ref, err)
	}
}
