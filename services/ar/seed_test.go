package ar_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cloud.google.com/go/artifactregistry/apiv1/artifactregistrypb"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

func TestSeedEnvVarsAndReset(t *testing.T) {
	e := start(t)
	seed := filepath.Join(t.TempDir(), "seed.yaml")
	data := `ar:
  repositories:
    - project: ` + testProject + `
      location: us-east1
      id: seeded
      description: from seed
      labels: {team: web}
    - name: projects/` + testProject + `/locations/europe/repositories/eu
`
	if err := os.WriteFile(seed, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ { // idempotent
		if err := e.inst.ApplySeed(context.Background(), seed); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	r, err := e.c.GetRepository(e.ctx, &artifactregistrypb.GetRepositoryRequest{Name: "projects/" + testProject + "/locations/us-east1/repositories/seeded"})
	if err != nil || r.GetDescription() != "from seed" || r.GetLabels()["team"] != "web" || r.GetFormat() != artifactregistrypb.Repository_DOCKER {
		t.Fatalf("seeded = %v %v", r, err)
	}
	if _, err := e.c.GetRepository(e.ctx, &artifactregistrypb.GetRepositoryRequest{Name: "projects/" + testProject + "/locations/europe/repositories/eu"}); err != nil {
		t.Fatal(err)
	}

	vars := e.inst.EnvVars()
	if vars["CLOUDSDK_API_ENDPOINT_OVERRIDES_ARTIFACTREGISTRY"] != "http://"+e.inst.Endpoint("gateway")+"/artifactregistry/" || vars["GCPEMU_REGISTRY"] != e.registry() {
		t.Fatalf("env vars = %v", vars)
	}

	img, _ := random.Image(128, 1)
	if err := remote.Write(e.ref(testProject+"/seeded/app:v1"), img); err != nil {
		t.Fatal(err)
	}
	blobs := filepath.Join(e.inst.Env.DataDir, "ar", "blobs", "sha256")
	if n := countFiles(t, blobs); n == 0 {
		t.Fatal("no blob files after push")
	}
	if err := e.inst.Reset(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := countFiles(t, blobs); n != 0 {
		t.Fatalf("%d blob files after reset", n)
	}
	if _, err := e.c.GetRepository(e.ctx, &artifactregistrypb.GetRepositoryRequest{Name: "projects/" + testProject + "/locations/us-east1/repositories/seeded"}); err == nil || !strings.Contains(err.Error(), "NotFound") {
		t.Fatalf("repo after reset: %v", err)
	}
}

func countFiles(t *testing.T, dir string) int {
	n := 0
	_ = filepath.Walk(dir, func(_ string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() {
			n++
		}
		return nil
	})
	return n
}
