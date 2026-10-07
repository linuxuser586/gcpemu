package ar_test

import (
	"net/http"
	"os"
	"strings"
	"testing"

	"cloud.google.com/go/artifactregistry/apiv1/artifactregistrypb"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"gopkg.in/yaml.v3"

	"github.com/linuxuser586/gcpemu/services/ar"
)

// createModeRepo creates a repository with a mode configuration.
func (e *env) createModeRepo(id string, repo *artifactregistrypb.Repository) (*artifactregistrypb.Repository, error) {
	e.t.Helper()
	repo.Format = artifactregistrypb.Repository_DOCKER
	op, err := e.c.CreateRepository(e.ctx, &artifactregistrypb.CreateRepositoryRequest{
		Parent: "projects/" + testProject + "/locations/" + testLoc, RepositoryId: id, Repository: repo,
	})
	if err != nil {
		return nil, err
	}
	return op.Wait(e.ctx)
}

func dockerHubRemote() *artifactregistrypb.Repository {
	return &artifactregistrypb.Repository{
		Mode: artifactregistrypb.Repository_REMOTE_REPOSITORY,
		ModeConfig: &artifactregistrypb.Repository_RemoteRepositoryConfig{RemoteRepositoryConfig: &artifactregistrypb.RemoteRepositoryConfig{
			RemoteSource: &artifactregistrypb.RemoteRepositoryConfig_DockerRepository_{DockerRepository: &artifactregistrypb.RemoteRepositoryConfig_DockerRepository{
				Upstream: &artifactregistrypb.RemoteRepositoryConfig_DockerRepository_PublicRepository_{PublicRepository: artifactregistrypb.RemoteRepositoryConfig_DockerRepository_DOCKER_HUB},
			}},
		}},
	}
}

func customRemote(uri string) *artifactregistrypb.Repository {
	return &artifactregistrypb.Repository{
		Mode: artifactregistrypb.Repository_REMOTE_REPOSITORY,
		ModeConfig: &artifactregistrypb.Repository_RemoteRepositoryConfig{RemoteRepositoryConfig: &artifactregistrypb.RemoteRepositoryConfig{
			RemoteSource: &artifactregistrypb.RemoteRepositoryConfig_DockerRepository_{DockerRepository: &artifactregistrypb.RemoteRepositoryConfig_DockerRepository{
				Upstream: &artifactregistrypb.RemoteRepositoryConfig_DockerRepository_CustomRepository_{CustomRepository: &artifactregistrypb.RemoteRepositoryConfig_DockerRepository_CustomRepository{Uri: uri}},
			}},
		}},
	}
}

func virtualRepo(policies ...*artifactregistrypb.UpstreamPolicy) *artifactregistrypb.Repository {
	return &artifactregistrypb.Repository{
		Mode: artifactregistrypb.Repository_VIRTUAL_REPOSITORY,
		ModeConfig: &artifactregistrypb.Repository_VirtualRepositoryConfig{VirtualRepositoryConfig: &artifactregistrypb.VirtualRepositoryConfig{
			UpstreamPolicies: policies,
		}},
	}
}

func repoName(loc, id string) string {
	return "projects/" + testProject + "/locations/" + loc + "/repositories/" + id
}

// FR-AR-006: a Docker Hub remote repository pulls through the cache and
// lists cached images as its content; pushes are rejected.
func TestRemoteRepositoryDockerHub(t *testing.T) {
	up := newFakeUpstream(t)
	img := up.put("library/nginx:1.27", 512, 2)
	e := start(t)
	arService(t, e.inst).SetUpstream("docker.io", up.pub.URL)
	repo, err := e.createModeRepo("dockerhub", dockerHubRemote())
	if err != nil {
		t.Fatal(err)
	}
	if repo.GetMode() != artifactregistrypb.Repository_REMOTE_REPOSITORY ||
		repo.GetRemoteRepositoryConfig().GetDockerRepository().GetPublicRepository() != artifactregistrypb.RemoteRepositoryConfig_DockerRepository_DOCKER_HUB {
		t.Fatalf("created = %v", repo)
	}
	got, err := remote.Image(e.ref(testLoc + "-docker.pkg.dev/" + testProject + "/dockerhub/nginx:1.27"))
	if err != nil {
		t.Fatalf("pull: %v", err)
	}
	assertSameImage(t, img, got)
	// Cached content is visible through the API.
	it := e.c.ListDockerImages(e.ctx, &artifactregistrypb.ListDockerImagesRequest{Parent: repoName(testLoc, "dockerhub")})
	var uris []string
	for {
		di, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		uris = append(uris, di.GetUri())
		if len(di.GetTags()) != 1 || di.GetTags()[0] != "1.27" {
			t.Fatalf("tags = %v", di.GetTags())
		}
	}
	wd, _ := img.Digest()
	if len(uris) != 1 || uris[0] != testLoc+"-docker.pkg.dev/"+testProject+"/dockerhub/nginx@"+wd.String() {
		t.Fatalf("docker images = %v", uris)
	}
	// Pushes are rejected.
	other, _ := random.Image(64, 1)
	if err := remote.Write(e.ref(testProject+"/dockerhub/mine:v1"), other); err == nil || !strings.Contains(err.Error(), "DENIED") {
		t.Fatalf("push to remote = %v", err)
	}
	// Served from the cache when the upstream is down.
	up.down.Store(true)
	arService(t, e.inst).SetMirrorTagTTL(0)
	if _, err := remote.Image(e.ref(testProject + "/dockerhub/nginx:1.27")); err != nil {
		t.Fatalf("pull from cache: %v", err)
	}
}

// FR-AR-006: custom upstream URIs (with a path prefix) and virtual
// repositories resolving across members by priority.
func TestRemoteCustomAndVirtual(t *testing.T) {
	up := newFakeUpstream(t)
	upImg := up.put("mirror/app:v1", 256, 1)
	upOnly := up.put("mirror/tool:v1", 256, 1)
	e := start(t)
	if _, err := e.createModeRepo("upstream", customRemote(up.pub.URL+"/mirror")); err != nil {
		t.Fatal(err)
	}
	e.createRepo(testLoc, "mine")
	mine, _ := random.Image(256, 1)
	if err := remote.Write(e.ref(testProject+"/mine/app:v1"), mine); err != nil {
		t.Fatal(err)
	}
	got, err := remote.Image(e.ref(testProject + "/upstream/app:v1"))
	if err != nil {
		t.Fatalf("pull custom remote: %v", err)
	}
	assertSameImage(t, upImg, got)

	_, err = e.createModeRepo("all", virtualRepo(
		&artifactregistrypb.UpstreamPolicy{Id: "remote", Repository: repoName(testLoc, "upstream"), Priority: 10},
		&artifactregistrypb.UpstreamPolicy{Id: "local", Repository: repoName(testLoc, "mine"), Priority: 100},
	))
	if err != nil {
		t.Fatal(err)
	}
	// Both members have app:v1; the higher priority member wins.
	got, err = remote.Image(e.ref(testProject + "/all/app:v1"))
	if err != nil {
		t.Fatalf("pull virtual: %v", err)
	}
	assertSameImage(t, mine, got)
	// Only the remote member has tool:v1.
	got, err = remote.Image(e.ref(testProject + "/all/tool:v1"))
	if err != nil {
		t.Fatalf("pull virtual from remote member: %v", err)
	}
	assertSameImage(t, upOnly, got)
	if tags, err := remote.List(e.ref(testProject + "/all/app").Context()); err != nil || len(tags) != 1 || tags[0] != "v1" {
		t.Fatalf("virtual tags = %v, %v", tags, err)
	}
	if _, err := remote.Image(e.ref(testProject + "/all/none:v1")); err == nil {
		t.Fatal("pull of unknown image through virtual repository succeeded")
	}
	other, _ := random.Image(64, 1)
	if err := remote.Write(e.ref(testProject+"/all/app:v2"), other); err == nil {
		t.Fatal("push to virtual repository succeeded")
	}
	// Re-prioritise: the remote member now wins.
	_, err = e.c.UpdateRepository(e.ctx, &artifactregistrypb.UpdateRepositoryRequest{
		Repository: virtualRepo(
			&artifactregistrypb.UpstreamPolicy{Id: "remote", Repository: repoName(testLoc, "upstream"), Priority: 200},
			&artifactregistrypb.UpstreamPolicy{Id: "local", Repository: repoName(testLoc, "mine"), Priority: 100},
		),
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"virtual_repository_config"}},
	})
	if err == nil {
		t.Fatal("update without name succeeded")
	}
	upd := virtualRepo(
		&artifactregistrypb.UpstreamPolicy{Id: "remote", Repository: repoName(testLoc, "upstream"), Priority: 200},
		&artifactregistrypb.UpstreamPolicy{Id: "local", Repository: repoName(testLoc, "mine"), Priority: 100},
	)
	upd.Name = repoName(testLoc, "all")
	if _, err := e.c.UpdateRepository(e.ctx, &artifactregistrypb.UpdateRepositoryRequest{
		Repository: upd, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"virtual_repository_config"}},
	}); err != nil {
		t.Fatal(err)
	}
	got, err = remote.Image(e.ref(testProject + "/all/app:v1"))
	if err != nil {
		t.Fatal(err)
	}
	assertSameImage(t, upImg, got)
	// A patch without mask on a remote repository keeps its config.
	r, err := e.c.UpdateRepository(e.ctx, &artifactregistrypb.UpdateRepositoryRequest{
		Repository: &artifactregistrypb.Repository{Name: repoName(testLoc, "upstream"), Description: "patched"},
	})
	if err != nil || r.GetRemoteRepositoryConfig().GetDockerRepository().GetCustomRepository().GetUri() != up.pub.URL+"/mirror" {
		t.Fatalf("patched remote = %v, %v", r, err)
	}
}

func TestRemoteVirtualValidation(t *testing.T) {
	e := start(t)
	cases := []struct {
		name string
		repo *artifactregistrypb.Repository
		want codes.Code
	}{
		{"remote ok", dockerHubRemote(), codes.OK},
		{"remote bad uri", customRemote("ftp://x"), codes.InvalidArgument},
		{"remote no upstream", &artifactregistrypb.Repository{Mode: artifactregistrypb.Repository_REMOTE_REPOSITORY,
			ModeConfig: &artifactregistrypb.Repository_RemoteRepositoryConfig{RemoteRepositoryConfig: &artifactregistrypb.RemoteRepositoryConfig{}}}, codes.InvalidArgument},
		{"virtual other location", virtualRepo(&artifactregistrypb.UpstreamPolicy{Repository: repoName("europe-west1", "x")}), codes.InvalidArgument},
		{"virtual bad name", virtualRepo(&artifactregistrypb.UpstreamPolicy{Repository: "nope"}), codes.InvalidArgument},
		{"virtual without config", &artifactregistrypb.Repository{Mode: artifactregistrypb.Repository_VIRTUAL_REPOSITORY}, codes.InvalidArgument},
		{"standard with remote config", func() *artifactregistrypb.Repository {
			r := dockerHubRemote()
			r.Mode = artifactregistrypb.Repository_STANDARD_REPOSITORY
			return r
		}(), codes.InvalidArgument},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := e.createModeRepo("r"+string(rune('a'+i)), c.repo)
			if code(err) != c.want {
				t.Fatalf("err = %v, want %v", err, c.want)
			}
		})
	}
}

// RegistriesYAML renders a k3s registries.yaml mirroring public registries
// and Artifact Registry hosts to the emulator.
func TestRegistriesYAML(t *testing.T) {
	b := ar.RegistriesYAML("10.0.0.2:5000", []string{"us-central1", "us"}, ar.RegistryAuth{Username: "oauth2accesstoken", Password: "tok"})
	var cfg struct {
		Mirrors map[string]struct {
			Endpoint []string `yaml:"endpoint"`
		} `yaml:"mirrors"`
		Configs map[string]struct {
			Auth struct{ Username, Password string } `yaml:"auth"`
		} `yaml:"configs"`
	}
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		t.Fatalf("%v\n%s", err, b)
	}
	for _, h := range append(ar.DefaultMirrorHosts, "*", "10.0.0.2:5000", "us-central1-docker.pkg.dev", "us-docker.pkg.dev") {
		if m := cfg.Mirrors[h]; len(m.Endpoint) != 1 || m.Endpoint[0] != "http://10.0.0.2:5000" {
			t.Fatalf("mirror %q = %v\n%s", h, m, b)
		}
	}
	if len(cfg.Mirrors) != len(ar.DefaultMirrorHosts)+4 {
		t.Fatalf("mirrors = %d\n%s", len(cfg.Mirrors), b)
	}
	if a := cfg.Configs["10.0.0.2:5000"].Auth; a.Username != "oauth2accesstoken" || a.Password != "tok" {
		t.Fatalf("auth = %+v\n%s", a, b)
	}
	all := ar.RegistriesYAML("r:5000", nil)
	if !strings.Contains(string(all), "europe-west1-docker.pkg.dev:") || strings.Contains(string(all), "configs") {
		t.Fatalf("all locations:\n%s", all)
	}
}

// Optional real-internet test: GCPEMU_NET_TESTS=1 pulls Docker Hub's
// busybox through the mirror, then again with Docker Hub unreachable.
func TestMirrorDockerHubNet(t *testing.T) {
	if os.Getenv("GCPEMU_NET_TESTS") != "1" {
		t.Skip("set GCPEMU_NET_TESTS=1 to pull from Docker Hub")
	}
	e := start(t)
	rt := remote.WithTransport(&mirrorTransport{registry: e.registry(), ns: "docker.io"})
	ref := name.MustParseReference("docker.io/library/busybox:latest")
	idx, err := remote.Index(ref, rt)
	if err != nil {
		t.Fatalf("pull busybox index: %v", err)
	}
	im, err := idx.IndexManifest()
	if err != nil || len(im.Manifests) == 0 {
		t.Fatalf("index = %v, %v", im, err)
	}
	img, err := remote.Image(ref, rt) // linux/amd64 by default
	if err != nil {
		t.Fatal(err)
	}
	assertSameImage(t, img, img)
	// Cached: point Docker Hub at a closed port and pull again.
	svc := arService(t, e.inst)
	svc.SetUpstream("docker.io", "http://127.0.0.1:1")
	svc.SetMirrorTagTTL(0)
	again, err := remote.Image(ref, rt)
	if err != nil {
		t.Fatalf("pull from cache: %v", err)
	}
	assertSameImage(t, img, again)
	resp, err := http.Get("http://" + e.registry() + "/v2/library/busybox/manifests/latest?ns=docker.io")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("containerd-style cached pull = %d", resp.StatusCode)
	}
}
