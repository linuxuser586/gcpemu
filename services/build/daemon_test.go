package build_test

import (
	"net/http"
	"regexp"
	"strings"
	"testing"

	"cloud.google.com/go/cloudbuild/apiv1/v2/cloudbuildpb"

	"github.com/linuxuser586/gcpemu/emutest"
	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/services/build"
)

// dockerImage is the image of the test steps that use docker, the Build
// daemon's own, which has the docker CLI.
const dockerImage = "docker:29.9.0-dind"

// dockerStep is a step running a shell script with the docker CLI.
func dockerStep(id, script string) *cloudbuildpb.BuildStep {
	return &cloudbuildpb.BuildStep{Name: dockerImage, Id: id, Entrypoint: "sh", Args: []string{"-c", script}}
}

// Steps reach the Build daemon at /var/run/docker.sock: a container one
// step starts on the cloudbuild network is reached by name from a
// container a later step runs there. The host runtime gets no container
// of theirs, and the daemon's containers are gone once the Build ends.
func TestStepsUseTheBuildDaemon(t *testing.T) {
	e := start(t)
	b := e.run(&cloudbuildpb.Build{Steps: []*cloudbuildpb.BuildStep{
		dockerStep("serve", "test -S /var/run/docker.sock && mkdir -p /tmp/www && "+
			"docker run -d --name web --network cloudbuild "+busybox+" sh -c 'echo served > /tmp/i && httpd -f -p 8080 -h /tmp'"),
		dockerStep("reach", "docker run --rm --network=cloudbuild "+busybox+" wget -q -O- http://web:8080/i"),
	}})
	if log := e.log(b); !strings.Contains(log, `Step #1 - "reach": served`) {
		t.Errorf("log lacks the served page:\n%s", log)
	}
	e.noBuildObjects()
	if leaked := e.hostContainers("web"); len(leaked) > 0 {
		t.Errorf("the host runtime runs the steps' containers: %v", leaked)
	}
	b = e.run(&cloudbuildpb.Build{Steps: []*cloudbuildpb.BuildStep{dockerStep("ps", "docker ps -a --format '{{.Names}}' | grep -c . || true")}})
	if log := e.log(b); !strings.Contains(log, `Step #0 - "ps": 0`) {
		t.Errorf("an earlier Build's containers are left in the daemon:\n%s", log)
	}
}

// hostContainers returns the host runtime's containers named name.
func (e *env) hostContainers(name string) []string {
	e.t.Helper()
	rt, err := e.inst.Internal().Env.Containers.Runtime(e.ctx)
	if err != nil {
		e.t.Fatal(err)
	}
	cs, err := rt.ListContainers(e.ctx, nil)
	if err != nil {
		e.t.Fatal(err)
	}
	var found []string
	for _, c := range cs {
		if strings.TrimPrefix(c.Name, "/") == name {
			found = append(found, c.ID)
		}
	}
	return found
}

// dockerfile is a step script writing a Dockerfile that builds on
// busybox, with a layer printing what.
func dockerfile(what string) string {
	return "printf 'FROM " + busybox + "\\nRUN echo " + what + " > /what\\n' > Dockerfile && "
}

// Under enforce, a Build's images are pushed to Artifact Registry as its
// service account once its steps succeed, and reported with the digest
// the registry has; a step's own docker push authenticates as that
// account too.
func TestImagesArePushed(t *testing.T) {
	e := startWith(t, []string{"ar", "iam"}, emutest.WithIAMMode(config.IAMEnforce))
	e.ctx = e.as(e.inst.Internal().Config.DefaultPrincipal)
	e.repository("us-central1", "apps")
	sa := e.serviceAccount("pusher")
	e.grant("roles/artifactregistry.writer", "serviceAccount:"+sa)
	img := "us-central1-docker.pkg.dev/" + testProject + "/apps/app:1"
	b := e.run(&cloudbuildpb.Build{
		ServiceAccount: "projects/" + testProject + "/serviceAccounts/" + sa,
		Steps: []*cloudbuildpb.BuildStep{
			dockerStep("build", dockerfile("built")+"docker build -t "+img+" . && docker tag "+img+" "+img+"-step && docker push "+img+"-step"),
		},
		Images: []string{img},
	})
	imgs := b.GetResults().GetImages()
	if len(imgs) != 1 || imgs[0].GetName() != img || imgs[0].GetPushTiming().GetEndTime() == nil {
		t.Fatalf("results.images = %v", imgs)
	}
	if want := e.manifestDigest("apps/app", "1"); imgs[0].GetDigest() != want {
		t.Errorf("digest = %q, registry has %q", imgs[0].GetDigest(), want)
	}
	if e.manifestDigest("apps/app", "1-step") == "" {
		t.Error("the step's own push is not in the registry")
	}
}

// repository creates a Docker repository in Artifact Registry.
func (e *env) repository(location, id string) {
	e.t.Helper()
	e.send(http.MethodPost, "/artifactregistry/v1/projects/"+testProject+"/locations/"+location+"/repositories?repositoryId="+id, `{"format":"DOCKER"}`)
}

// manifestDigest returns the digest Artifact Registry serves for image
// (REPO/IMAGE in the test project) at ref, "" when it has none.
func (e *env) manifestDigest(image, ref string) string {
	e.t.Helper()
	req, err := http.NewRequestWithContext(e.ctx, http.MethodHead, "http://"+e.inst.Endpoint("ar")+"/v2/"+testProject+"/"+image+"/manifests/"+ref, nil)
	if err != nil {
		e.t.Fatal(err)
	}
	req.Header.Set("Accept", "application/vnd.oci.image.index.v1+json, application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json, application/vnd.docker.distribution.manifest.list.v2+json")
	req.Header.Set("Authorization", e.token())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	return resp.Header.Get("Docker-Content-Digest")
}

// daemonStatus is GET /_emu/v1/build/daemon.
type daemonStatus struct {
	Running           bool   `json:"running"`
	CacheBytes        int64  `json:"cacheBytes"`
	Error             string `json:"error"`
	PrivilegedRefused bool   `json:"privilegedRefused"`
}

func (e *env) daemon() daemonStatus {
	e.t.Helper()
	var st daemonStatus
	if code := e.rest(http.MethodGet, "/_emu/v1/build/daemon", nil, &st); code != http.StatusOK {
		e.t.Fatalf("GET /_emu/v1/build/daemon: %d", code)
	}
	return st
}

// cachedRun runs a Build that builds the same image each time, and
// reports whether its RUN layer came from the Build daemon's cache.
func (e *env) cachedRun() bool {
	e.t.Helper()
	b := e.run(&cloudbuildpb.Build{Steps: []*cloudbuildpb.BuildStep{
		dockerStep("build", dockerfile("cached")+"docker build --progress=plain -t cache-test ."),
	}})
	log := e.log(b)
	m := regexp.MustCompile(`#(\d+) \[2/2\] RUN`).FindStringSubmatch(log)
	if m == nil {
		e.t.Fatalf("no RUN layer in the log:\n%s", log)
	}
	return strings.Contains(log, "#"+m[1]+" CACHED")
}

// A second identical Build reuses the layers the first built, also after
// a Reset; pruning through the Admin API empties the cache, its size
// drops, and the next Build builds the layer again.
func TestBuildDaemonCache(t *testing.T) {
	e := start(t)
	if e.daemon().Running {
		t.Error("the Build daemon runs before any Build")
	}
	if e.cachedRun() {
		t.Error("the first Build found its layer cached")
	}
	if !e.cachedRun() {
		t.Error("the second Build built its layer again")
	}
	if err := e.inst.Internal().Reset(e.ctx); err != nil {
		t.Fatal(err)
	}
	if !e.cachedRun() {
		t.Error("after Reset, the Build built its layer again")
	}
	before := e.daemon()
	if !before.Running || before.CacheBytes <= 0 {
		t.Fatalf("daemon = %+v", before)
	}
	var pruned struct {
		ReclaimedBytes int64 `json:"reclaimedBytes"`
		CacheBytes     int64 `json:"cacheBytes"`
	}
	if code := e.rest(http.MethodPost, "/_emu/v1/build/daemon/prune", nil, &pruned); code != http.StatusOK {
		t.Fatalf("prune: %d", code)
	}
	if pruned.CacheBytes >= before.CacheBytes || pruned.ReclaimedBytes <= 0 || e.daemon().CacheBytes != pruned.CacheBytes {
		t.Errorf("prune = %+v, from %d bytes", pruned, before.CacheBytes)
	}
	if e.cachedRun() {
		t.Error("after prune, the Build found its layer cached")
	}
}

// On a runtime that refuses privileged containers, a Build fails saying
// so, and the Admin API reports it for gcpemu doctor.
func TestBuildDaemonRefused(t *testing.T) {
	e := start(t)
	svc, _ := e.inst.Internal().Env.Lookup("build")
	build.SetUnprivileged(svc)
	op, created := e.create(&cloudbuildpb.Build{Steps: []*cloudbuildpb.BuildStep{step("", "true")}})
	_, _ = op.Wait(e.ctx)
	b := e.get(created.GetId())
	if b.GetStatus() != cloudbuildpb.Build_INTERNAL_ERROR || !strings.Contains(b.GetStatusDetail(), "privileged container") {
		t.Errorf("Build = %v: %s", b.GetStatus(), b.GetStatusDetail())
	}
	if st := e.daemon(); !st.PrivilegedRefused || st.Running || !strings.Contains(st.Error, "privileged") {
		t.Errorf("daemon = %+v", st)
	}
}
