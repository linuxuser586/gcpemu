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

// TestExportSeedRoundTrip checks FR-CORE-013: the exported "ar" section
// recreates the same repositories on an empty Instance.
func TestExportSeedRoundTrip(t *testing.T) {
	e := start(t)
	ctx := context.Background()
	repos := "projects/" + testProject + "/locations/us-central1/repositories/"
	seed := `ar:
  repositories:
    - project: ` + testProject + `
      location: us-central1
      id: images
      description: App images
      labels: {team: web}
      immutableTags: true
    - name: ` + repos + `dockerhub
      mode: REMOTE_REPOSITORY
      remote: {publicRepository: DOCKER_HUB}
    - project: ` + testProject + `
      location: us-central1
      id: quay
      mode: remote_repository
      remote: {uri: https://quay.io}
    - project: ` + testProject + `
      location: us-central1
      id: all
      mode: VIRTUAL_REPOSITORY
      upstreams:
        - {id: mine, repository: ` + repos + `images, priority: 100}
        - {id: hub, repository: ` + repos + `dockerhub, priority: 10}
`
	if err := e.inst.ApplySeedData(ctx, []byte(seed), t.TempDir()); err != nil {
		t.Fatal(err)
	}
	got, err := e.inst.ExportSeed(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := `ar:
  repositories:
    - project: ` + testProject + `
      location: us-central1
      id: all
      mode: VIRTUAL_REPOSITORY
      upstreams:
        - id: mine
          repository: ` + repos + `images
          priority: 100
        - id: hub
          repository: ` + repos + `dockerhub
          priority: 10
    - project: ` + testProject + `
      location: us-central1
      id: dockerhub
      mode: REMOTE_REPOSITORY
      remote:
        publicRepository: DOCKER_HUB
    - project: ` + testProject + `
      location: us-central1
      id: images
      description: App images
      labels:
        team: web
      immutableTags: true
    - project: ` + testProject + `
      location: us-central1
      id: quay
      mode: REMOTE_REPOSITORY
      remote:
        uri: https://quay.io
`
	if !strings.Contains(string(got), want) {
		t.Fatalf("export =\n%s\nwant section\n%s", got, want)
	}
	if err := e.inst.Reset(ctx); err != nil {
		t.Fatal(err)
	}
	if err := e.inst.ApplySeedData(ctx, got, t.TempDir()); err != nil {
		t.Fatalf("apply export: %v", err)
	}
	again, err := e.inst.ExportSeed(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if string(again) != string(got) {
		t.Fatalf("second export =\n%s\nfirst =\n%s", again, got)
	}
}
