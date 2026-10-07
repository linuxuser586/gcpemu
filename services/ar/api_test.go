package ar_test

import (
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/artifactregistry/apiv1/artifactregistrypb"
	"cloud.google.com/go/iam/apiv1/iampb"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"google.golang.org/api/iterator"
	"google.golang.org/genproto/googleapis/cloud/location"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/linuxuser586/gcpemu/emutest"
	"github.com/linuxuser586/gcpemu/internal/config"
)

func code(err error) codes.Code { return status.Code(err) }

func TestRepositoryLifecycleGRPC(t *testing.T) {
	e := start(t)
	repo := e.createRepo(testLoc, "images")
	name := "projects/" + testProject + "/locations/" + testLoc + "/repositories/images"
	if repo.GetName() != name || repo.GetFormat() != artifactregistrypb.Repository_DOCKER ||
		repo.GetMode() != artifactregistrypb.Repository_STANDARD_REPOSITORY || repo.GetCreateTime() == nil ||
		repo.GetRegistryUri() != testLoc+"-docker.pkg.dev/"+testProject+"/images" {
		t.Fatalf("created = %v", repo)
	}
	// Duplicate.
	_, err := e.c.CreateRepository(e.ctx, &artifactregistrypb.CreateRepositoryRequest{
		Parent: "projects/" + testProject + "/locations/" + testLoc, RepositoryId: "images",
		Repository: &artifactregistrypb.Repository{Format: artifactregistrypb.Repository_DOCKER},
	})
	if code(err) != codes.AlreadyExists {
		t.Fatalf("duplicate = %v", err)
	}
	got, err := e.c.GetRepository(e.ctx, &artifactregistrypb.GetRepositoryRequest{Name: name})
	if err != nil || got.GetDescription() != "test" {
		t.Fatalf("get = %v %v", got, err)
	}
	upd, err := e.c.UpdateRepository(e.ctx, &artifactregistrypb.UpdateRepositoryRequest{
		Repository: &artifactregistrypb.Repository{Name: name, Description: "new", Labels: map[string]string{"a": "b"}},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"labels"}},
	})
	if err != nil || upd.GetDescription() != "test" || upd.GetLabels()["a"] != "b" {
		t.Fatalf("update = %v %v", upd, err)
	}
	_, err = e.c.UpdateRepository(e.ctx, &artifactregistrypb.UpdateRepositoryRequest{
		Repository: &artifactregistrypb.Repository{Name: name, Format: artifactregistrypb.Repository_MAVEN},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"format"}},
	})
	if code(err) != codes.InvalidArgument {
		t.Fatalf("immutable update = %v", err)
	}
	e.createRepo(testLoc, "other")
	it := e.c.ListRepositories(e.ctx, &artifactregistrypb.ListRepositoriesRequest{Parent: "projects/" + testProject + "/locations/" + testLoc, PageSize: 1})
	var names []string
	for {
		r, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, r.GetName()[strings.LastIndex(r.GetName(), "/")+1:])
	}
	if strings.Join(names, ",") != "images,other" {
		t.Fatalf("list = %v", names)
	}
	op, err := e.c.DeleteRepository(e.ctx, &artifactregistrypb.DeleteRepositoryRequest{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	if err := op.Wait(e.ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := e.c.GetRepository(e.ctx, &artifactregistrypb.GetRepositoryRequest{Name: name}); code(err) != codes.NotFound {
		t.Fatalf("get deleted = %v", err)
	}
	if _, err := e.c.DeleteRepository(e.ctx, &artifactregistrypb.DeleteRepositoryRequest{Name: name}); code(err) != codes.NotFound {
		t.Fatalf("delete missing = %v", err)
	}
}

func TestRepositoryValidation(t *testing.T) {
	e := start(t)
	cases := []struct {
		name, parent, id string
		repo             *artifactregistrypb.Repository
		want             codes.Code
	}{
		{"bad location", "projects/proj-1/locations/mars-north1", "r", &artifactregistrypb.Repository{Format: artifactregistrypb.Repository_DOCKER}, codes.InvalidArgument},
		{"bad id", "projects/proj-1/locations/us", "Bad_ID", &artifactregistrypb.Repository{Format: artifactregistrypb.Repository_DOCKER}, codes.InvalidArgument},
		{"no format", "projects/proj-1/locations/us", "r", &artifactregistrypb.Repository{}, codes.InvalidArgument},
		{"npm", "projects/proj-1/locations/us", "r", &artifactregistrypb.Repository{Format: artifactregistrypb.Repository_NPM}, codes.Unimplemented},
		{"remote without config", "projects/proj-1/locations/europe", "r", &artifactregistrypb.Repository{Format: artifactregistrypb.Repository_DOCKER, Mode: artifactregistrypb.Repository_REMOTE_REPOSITORY}, codes.InvalidArgument},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := e.c.CreateRepository(e.ctx, &artifactregistrypb.CreateRepositoryRequest{Parent: c.parent, RepositoryId: c.id, Repository: c.repo})
			if code(err) != c.want {
				t.Fatalf("err = %v, want %v", err, c.want)
			}
		})
	}
	// Methods outside the emulated subset are UNIMPLEMENTED.
	if _, err := e.c.GetProjectSettings(e.ctx, &artifactregistrypb.GetProjectSettingsRequest{Name: "projects/proj-1/projectSettings"}); code(err) != codes.Unimplemented {
		t.Fatalf("GetProjectSettings = %v", err)
	}
}

func TestLocations(t *testing.T) {
	e := start(t)
	it := e.c.ListLocations(e.ctx, &location.ListLocationsRequest{Name: "projects/" + testProject})
	seen := map[string]bool{}
	for {
		l, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		seen[l.GetLocationId()] = true
	}
	for _, want := range []string{"us", "europe", "asia", "us-central1", "europe-west1"} {
		if !seen[want] {
			t.Errorf("missing location %s", want)
		}
	}
	l, err := e.c.GetLocation(e.ctx, &location.GetLocationRequest{Name: "projects/" + testProject + "/locations/us-east1"})
	if err != nil || l.GetLocationId() != "us-east1" {
		t.Fatalf("get = %v %v", l, err)
	}
}

func TestLROLatency(t *testing.T) {
	e := start(t, func(c *config.Config) { c.LROLatency["ar"] = "300ms" })
	op, err := e.c.CreateRepository(e.ctx, &artifactregistrypb.CreateRepositoryRequest{
		Parent: "projects/" + testProject + "/locations/" + testLoc, RepositoryId: "slow",
		Repository: &artifactregistrypb.Repository{Format: artifactregistrypb.Repository_DOCKER},
	})
	if err != nil {
		t.Fatal(err)
	}
	if op.Done() {
		t.Fatal("operation done immediately despite latency")
	}
	if !strings.HasPrefix(op.Name(), "projects/"+testProject+"/locations/"+testLoc+"/operations/") {
		t.Fatalf("op name %q", op.Name())
	}
	start := time.Now()
	repo, err := op.Wait(e.ctx)
	if err != nil || repo.GetName() == "" {
		t.Fatalf("wait = %v %v", repo, err)
	}
	if time.Since(start) < 200*time.Millisecond {
		t.Fatalf("completed too fast: %v", time.Since(start))
	}
}

func TestArtifactsAgreeWithRegistry(t *testing.T) {
	e := start(t)
	e.createRepo(testLoc, "images")
	repo := "projects/" + testProject + "/locations/" + testLoc + "/repositories/images"
	img1, _ := random.Image(256, 2)
	img2, _ := random.Image(256, 1)
	if err := remote.Write(e.ref(testProject+"/images/app:v1"), img1); err != nil {
		t.Fatal(err)
	}
	if err := remote.Tag(e.ref(testProject+"/images/app:v1").Context().Tag("stable"), img1); err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(e.ref(testProject+"/images/team/svc:v2"), img2); err != nil {
		t.Fatal(err)
	}
	d1, _ := img1.Digest()
	d2, _ := img2.Digest()

	var imgs []*artifactregistrypb.DockerImage
	it := e.c.ListDockerImages(e.ctx, &artifactregistrypb.ListDockerImagesRequest{Parent: repo})
	for {
		d, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		imgs = append(imgs, d)
	}
	if len(imgs) != 2 {
		t.Fatalf("docker images = %d", len(imgs))
	}
	byURI := map[string]*artifactregistrypb.DockerImage{}
	for _, d := range imgs {
		byURI[d.GetUri()] = d
	}
	u1 := testLoc + "-docker.pkg.dev/" + testProject + "/images/app@" + d1.String()
	di := byURI[u1]
	if di == nil {
		t.Fatalf("missing %s in %v", u1, byURI)
	}
	sort.Strings(di.Tags)
	if strings.Join(di.Tags, ",") != "stable,v1" || di.GetMediaType() != "application/vnd.docker.distribution.manifest.v2+json" || di.GetImageSizeBytes() == 0 || di.GetUploadTime() == nil {
		t.Fatalf("docker image = %v", di)
	}
	nested := repo + "/dockerImages/team%2Fsvc@" + d2.String()
	got, err := e.c.GetDockerImage(e.ctx, &artifactregistrypb.GetDockerImageRequest{Name: nested})
	if err != nil || got.GetUri() != testLoc+"-docker.pkg.dev/"+testProject+"/images/team/svc@"+d2.String() {
		t.Fatalf("get nested = %v %v", got, err)
	}

	pit := e.c.ListPackages(e.ctx, &artifactregistrypb.ListPackagesRequest{Parent: repo})
	var pkgs []string
	for {
		p, err := pit.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		pkgs = append(pkgs, p.GetName())
	}
	if strings.Join(pkgs, " ") != repo+"/packages/app "+repo+"/packages/team%2Fsvc" {
		t.Fatalf("packages = %v", pkgs)
	}
	pkg := repo + "/packages/app"
	vit := e.c.ListVersions(e.ctx, &artifactregistrypb.ListVersionsRequest{Parent: pkg, View: artifactregistrypb.VersionView_FULL})
	v, err := vit.Next()
	if err != nil || v.GetName() != pkg+"/versions/"+d1.String() || len(v.GetRelatedTags()) != 2 {
		t.Fatalf("version = %v %v", v, err)
	}
	tit := e.c.ListTags(e.ctx, &artifactregistrypb.ListTagsRequest{Parent: pkg, Filter: `version="` + v.GetName() + `"`})
	var tags []string
	for {
		tg, err := tit.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if tg.GetVersion() != v.GetName() {
			t.Fatalf("tag version = %s", tg.GetVersion())
		}
		tags = append(tags, tg.GetName()[strings.LastIndex(tg.GetName(), "/")+1:])
	}
	if strings.Join(tags, ",") != "stable,v1" {
		t.Fatalf("tags = %v", tags)
	}
	// Tag operations are reflected in the registry.
	if _, err := e.c.CreateTag(e.ctx, &artifactregistrypb.CreateTagRequest{Parent: pkg, TagId: "prod", Tag: &artifactregistrypb.Tag{Version: v.GetName()}}); err != nil {
		t.Fatal(err)
	}
	if err := e.c.DeleteTag(e.ctx, &artifactregistrypb.DeleteTagRequest{Name: pkg + "/tags/v1"}); err != nil {
		t.Fatal(err)
	}
	rtags, err := remote.List(e.ref(testProject + "/images/app").Context())
	sort.Strings(rtags)
	if err != nil || strings.Join(rtags, ",") != "prod,stable" {
		t.Fatalf("registry tags = %v %v", rtags, err)
	}
	// A tagged version needs force.
	if _, err := e.c.DeleteVersion(e.ctx, &artifactregistrypb.DeleteVersionRequest{Name: v.GetName()}); code(err) != codes.FailedPrecondition {
		t.Fatalf("delete tagged version = %v", err)
	}
	op, err := e.c.DeleteVersion(e.ctx, &artifactregistrypb.DeleteVersionRequest{Name: v.GetName(), Force: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := op.Wait(e.ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := remote.Image(e.ref(testProject + "/images/app:stable")); err == nil {
		t.Fatal("image still pullable after DeleteVersion")
	}
	// DeletePackage removes the nested image.
	pop, err := e.c.DeletePackage(e.ctx, &artifactregistrypb.DeletePackageRequest{Name: repo + "/packages/team%2Fsvc"})
	if err != nil {
		t.Fatal(err)
	}
	if err := pop.Wait(e.ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := e.c.GetDockerImage(e.ctx, &artifactregistrypb.GetDockerImageRequest{Name: nested}); code(err) != codes.NotFound {
		t.Fatalf("get deleted image = %v", err)
	}
}

func TestIndexDockerImage(t *testing.T) {
	e := start(t)
	e.createRepo(testLoc, "images")
	idx, _ := random.Index(128, 1, 2)
	if err := remote.WriteIndex(e.ref(testProject+"/images/multi:v1"), idx); err != nil {
		t.Fatal(err)
	}
	d, _ := idx.Digest()
	got, err := e.c.GetDockerImage(e.ctx, &artifactregistrypb.GetDockerImageRequest{
		Name: "projects/" + testProject + "/locations/" + testLoc + "/repositories/images/dockerImages/multi@" + d.String(),
	})
	if err != nil || len(got.GetImageManifests()) != 2 || got.GetMediaType() != "application/vnd.oci.image.index.v1+json" {
		t.Fatalf("index image = %v %v", got, err)
	}
}

func TestRepositoryIAM(t *testing.T) {
	e := start(t, emutest.WithIAMMode(config.IAMOff))
	e.createRepo(testLoc, "images")
	res := "projects/" + testProject + "/locations/" + testLoc + "/repositories/images"
	p, err := e.c.GetIamPolicy(e.ctx, &iampb.GetIamPolicyRequest{Resource: res})
	if err != nil {
		t.Fatal(err)
	}
	p.Bindings = []*iampb.Binding{{Role: "roles/artifactregistry.reader", Members: []string{"user:a@example.com"}}}
	set, err := e.c.SetIamPolicy(e.ctx, &iampb.SetIamPolicyRequest{Resource: res, Policy: p})
	if err != nil || len(set.GetBindings()) != 1 {
		t.Fatalf("set = %v %v", set, err)
	}
	got, err := e.c.GetIamPolicy(e.ctx, &iampb.GetIamPolicyRequest{Resource: res})
	if err != nil || len(got.GetBindings()) != 1 || got.GetBindings()[0].GetRole() != "roles/artifactregistry.reader" {
		t.Fatalf("get = %v %v", got, err)
	}
	tp, err := e.c.TestIamPermissions(e.ctx, &iampb.TestIamPermissionsRequest{Resource: res, Permissions: []string{"artifactregistry.repositories.get"}})
	if err != nil || len(tp.GetPermissions()) != 1 {
		t.Fatalf("test = %v %v", tp, err)
	}
}
