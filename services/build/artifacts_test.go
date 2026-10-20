package build_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"cloud.google.com/go/cloudbuild/apiv1/v2/cloudbuildpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Once the steps succeed, the files artifacts.objects names in the
// workspace are uploaded under its location, and listed in the manifest
// results names, with their count and the upload's timing.
func TestArtifactObjects(t *testing.T) {
	e := start(t)
	const bucket = "artifacts-out"
	e.upload(bucket, "keep", []byte("x"))
	b := e.run(&cloudbuildpb.Build{
		Steps: []*cloudbuildpb.BuildStep{step("make", "mkdir -p out && echo alpha > out/a.txt && echo beta > out/b.txt && echo skipped > c.log")},
		Artifacts: &cloudbuildpb.Artifacts{Objects: &cloudbuildpb.Artifacts_ArtifactObjects{
			Location: "gs://" + bucket + "/dir/",
			Paths:    []string{"out/*.txt"},
		}},
	})
	for name, want := range map[string]string{"dir/a.txt": "alpha\n", "dir/b.txt": "beta\n"} {
		if code, got := e.object(bucket, name, 0); code != http.StatusOK || got != want {
			t.Errorf("%s = %d %q, want %q", name, code, got, want)
		}
	}
	if code, _ := e.object(bucket, "dir/c.log", 0); code != http.StatusNotFound {
		t.Errorf("an unmatched file was uploaded: %d", code)
	}
	res := b.GetResults()
	manifest := "gs://" + bucket + "/dir/artifacts-" + b.GetId() + ".json"
	if res.GetNumArtifacts() != 2 || res.GetArtifactManifest() != manifest || res.GetArtifactTiming().GetEndTime() == nil {
		t.Fatalf("results = %v, want 2 artifacts in %s", res, manifest)
	}
	_, body := e.object(bucket, "dir/artifacts-"+b.GetId()+".json", 0)
	var locations []string
	for _, line := range strings.Split(strings.TrimSpace(body), "\n") {
		var entry struct {
			Location string `json:"location"`
		}
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("manifest line %q: %v", line, err)
		}
		locations = append(locations, strings.Split(entry.Location, "#")[0])
	}
	if strings.Join(locations, " ") != "gs://"+bucket+"/dir/a.txt gs://"+bucket+"/dir/b.txt" {
		t.Errorf("manifest lists %v", locations)
	}
}

// The artifact kinds other than images and objects are Absent, and the
// error names the field.
func TestAbsentArtifacts(t *testing.T) {
	e := start(t)
	for field, a := range map[string]*cloudbuildpb.Artifacts{
		"artifacts.mavenArtifacts": {MavenArtifacts: []*cloudbuildpb.Artifacts_MavenArtifact{{}}},
		"artifacts.pythonPackages": {PythonPackages: []*cloudbuildpb.Artifacts_PythonPackage{{}}},
		"artifacts.npmPackages":    {NpmPackages: []*cloudbuildpb.Artifacts_NpmPackage{{}}},
		"artifacts.goModules":      {GoModules: []*cloudbuildpb.Artifacts_GoModule{{}}},
	} {
		_, err := e.cb.CreateBuild(e.ctx, &cloudbuildpb.CreateBuildRequest{ProjectId: testProject, Build: &cloudbuildpb.Build{
			Steps: []*cloudbuildpb.BuildStep{step("", "true")}, Artifacts: a,
		}})
		if status.Code(err) != codes.Unimplemented || !strings.Contains(err.Error(), field) {
			t.Errorf("%s: err = %v, want UNIMPLEMENTED naming it", field, err)
		}
	}
}
