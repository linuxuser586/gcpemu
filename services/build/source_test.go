package build_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"cloud.google.com/go/cloudbuild/apiv1/v2/cloudbuildpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// tgz is a gzipped tarball of files, as gcloud builds submit uploads.
func tgz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(tw, body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// upload stores data as bucket/name, creating the bucket, and returns the
// object's generation.
func (e *env) upload(bucket, name string, data []byte) int64 {
	e.t.Helper()
	e.rest(http.MethodPost, "/storage/v1/b?project="+testProject, map[string]string{"name": bucket}, nil)
	resp, err := http.Post(e.inst.GatewayURL()+"/upload/storage/v1/b/"+bucket+"/o?uploadType=media&name="+name, "application/gzip", bytes.NewReader(data))
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	var o struct {
		Generation string `json:"generation"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&o); err != nil || resp.StatusCode != http.StatusOK {
		e.t.Fatalf("upload %s/%s: %d %v", bucket, name, resp.StatusCode, err)
	}
	gen, _ := strconv.ParseInt(o.Generation, 10, 64)
	return gen
}

// A storageSource tarball is extracted into /workspace before the steps
// run, and the generation used is resolved on the Build; a generation
// that is gone fails the Build fetching its source.
func TestStorageSource(t *testing.T) {
	e := start(t)
	bucket := testProject + "_cloudbuild"
	gen := e.upload(bucket, "source/src.tgz", tgz(t, map[string]string{"hello.txt": "from source\n", "sub/b.txt": "nested\n"}))
	src := &cloudbuildpb.Source{Source: &cloudbuildpb.Source_StorageSource{StorageSource: &cloudbuildpb.StorageSource{Bucket: bucket, Object: "source/src.tgz"}}}
	op, _ := e.create(&cloudbuildpb.Build{Source: src, Steps: []*cloudbuildpb.BuildStep{step("", "cat hello.txt sub/b.txt")}})
	b, err := op.Wait(e.ctx)
	if err != nil {
		t.Fatalf("Wait: %v (%s)", err, b.GetStatusDetail())
	}
	log := e.storedLog(b)
	for _, want := range []string{"FETCHSOURCE\n", "Step #0: from source\n", "Step #0: nested\n"} {
		if !strings.Contains(log, want) {
			t.Errorf("log lacks %q:\n%s", want, log)
		}
	}
	if got := b.GetSourceProvenance().GetResolvedStorageSource().GetGeneration(); got != gen {
		t.Errorf("resolved generation = %d, want %d", got, gen)
	}
	e.noBuildObjects()

	e.upload(bucket, "source/src.tgz", tgz(t, map[string]string{"hello.txt": "newer\n"}))
	stale := &cloudbuildpb.Source{Source: &cloudbuildpb.Source_StorageSource{StorageSource: &cloudbuildpb.StorageSource{Bucket: bucket, Object: "source/src.tgz", Generation: gen}}}
	op, created := e.create(&cloudbuildpb.Build{Source: stale, Steps: []*cloudbuildpb.BuildStep{step("", "true")}})
	if _, err := op.Wait(e.ctx); err == nil {
		t.Fatal("a stale generation's Build succeeded")
	}
	b = e.get(created.GetId())
	if b.GetStatus() != cloudbuildpb.Build_FAILURE || b.GetFailureInfo().GetType() != cloudbuildpb.Build_FailureInfo_FETCH_SOURCE_FAILED {
		t.Errorf("stale generation: %v, failure %v", b.GetStatus(), b.GetFailureInfo())
	}
	if s := b.GetSteps()[0].GetStatus(); s != cloudbuildpb.Build_QUEUED {
		t.Errorf("step after a failed fetch = %v", s)
	}
	e.noBuildObjects()
}

// The sources this emulator does not have answer UNIMPLEMENTED, naming
// the field.
func TestUnemulatedSources(t *testing.T) {
	e := start(t)
	for field, src := range map[string]*cloudbuildpb.Source{
		"repoSource":            {Source: &cloudbuildpb.Source_RepoSource{RepoSource: &cloudbuildpb.RepoSource{RepoName: "r"}}},
		"connectedRepository":   {Source: &cloudbuildpb.Source_ConnectedRepository{ConnectedRepository: &cloudbuildpb.ConnectedRepository{Repository: "r", Revision: "main"}}},
		"storageSourceManifest": {Source: &cloudbuildpb.Source_StorageSourceManifest{StorageSourceManifest: &cloudbuildpb.StorageSourceManifest{Bucket: "b", Object: "o"}}},
	} {
		_, err := e.cb.CreateBuild(e.ctx, &cloudbuildpb.CreateBuildRequest{ProjectId: testProject, Build: &cloudbuildpb.Build{Source: src, Steps: []*cloudbuildpb.BuildStep{step("", "true")}}})
		if status.Code(err) != codes.Unimplemented || !strings.Contains(err.Error(), field) {
			t.Errorf("%s: err = %v, want UNIMPLEMENTED naming it", field, err)
		}
	}
	// The client library predates developerConnectConfig; REST carries it.
	var out struct {
		Error struct{ Status, Message string }
	}
	body := map[string]any{
		"source": map[string]any{"developerConnectConfig": map[string]any{"gitRepositoryLink": "l", "revision": "main"}},
		"steps":  []map[string]any{{"name": busybox, "args": []string{"true"}}},
	}
	code := e.rest(http.MethodPost, "/cloudbuild/v1/projects/"+testProject+"/builds", body, &out)
	if code != http.StatusNotImplemented || !strings.Contains(out.Error.Message, "developerConnectConfig") {
		t.Errorf("developerConnectConfig over REST = %d %+v", code, out)
	}
}

// A gitSource is cloned at the requested revision, a commit or a branch,
// which the Build resolves to the commit checked out.
func TestGitSource(t *testing.T) {
	e := start(t)
	const repo = "https://github.com/octocat/Hello-World"
	for rev, commit := range map[string]string{
		"553c2077f0edc3d5dc5d17262f6aa498e69d6f8e": "553c2077f0edc3d5dc5d17262f6aa498e69d6f8e",
		"test": "b3cbd5bbd7e81436d2eee04537ea2b4c0cad4cdf",
	} {
		src := &cloudbuildpb.Source{Source: &cloudbuildpb.Source_GitSource{GitSource: &cloudbuildpb.GitSource{Url: repo, Revision: rev}}}
		op, _ := e.create(&cloudbuildpb.Build{Source: src, Steps: []*cloudbuildpb.BuildStep{step("", "cat .git/HEAD && test -f README")}})
		b, err := op.Wait(e.ctx)
		if err != nil {
			t.Fatalf("%s: Wait: %v (%s)", rev, err, b.GetStatusDetail())
		}
		if log := e.storedLog(b); !strings.Contains(log, "Step #0: "+commit+"\n") {
			t.Errorf("%s: log lacks the commit %s:\n%s", rev, commit, log)
		}
		if got := b.GetSourceProvenance().GetResolvedGitSource().GetRevision(); got != commit {
			t.Errorf("%s: resolved revision = %q, want %s", rev, got, commit)
		}
	}
	e.noBuildObjects()
}
