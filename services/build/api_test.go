package build_test

import (
	"net/http"
	"testing"
	"time"

	"cloud.google.com/go/cloudbuild/apiv1/v2/cloudbuildpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// restBuild is a Build as the REST API returns it.
type restBuild struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
	Tags   []string
}

// restOp is an Operation as the REST API returns it.
type restOp struct {
	Name     string `json:"name"`
	Done     bool   `json:"done"`
	Metadata struct {
		Type  string    `json:"@type"`
		Build restBuild `json:"build"`
	} `json:"metadata"`
	Response *restBuild `json:"response"`
	Error    *struct {
		Code int `json:"code"`
	} `json:"error"`
}

// The regional resource path serves create, the Operation, get, list with
// a filter and retry over REST, as gcloud uses them.
func TestRegionalBuildsOverREST(t *testing.T) {
	e := start(t)
	parent := "/cloudbuild/v1/projects/" + testProject + "/locations/us-central1"
	body := map[string]any{"tags": []string{"red"}, "steps": []map[string]any{{"name": busybox, "args": []string{"true"}}}}
	var op restOp
	if code := e.rest("POST", parent+"/builds", body, &op); code != http.StatusOK {
		t.Fatalf("create: %d %+v", code, op)
	}
	if op.Metadata.Type != "type.googleapis.com/google.devtools.cloudbuild.v1.BuildOperationMetadata" || op.Metadata.Build.Status != "QUEUED" {
		t.Fatalf("metadata = %+v", op.Metadata)
	}
	id := op.Metadata.Build.ID
	if want := "projects/" + testProject + "/locations/us-central1/builds/" + id; op.Metadata.Build.Name != want {
		t.Errorf("name = %q, want %q", op.Metadata.Build.Name, want)
	}
	deadline := time.Now().Add(time.Minute)
	for !op.Done && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
		e.rest("GET", "/cloudbuild/v1/"+op.Name, nil, &op)
	}
	if !op.Done || op.Response == nil || op.Response.Status != "SUCCESS" || op.Metadata.Build.Status != "SUCCESS" {
		t.Fatalf("Operation = %+v", op)
	}

	var b restBuild
	if code := e.rest("GET", parent+"/builds/"+id, nil, &b); code != http.StatusOK || b.Status != "SUCCESS" {
		t.Fatalf("get: %d %+v", code, b)
	}
	var retried restOp
	if code := e.rest("POST", parent+"/builds/"+id+":retry", map[string]any{}, &retried); code != http.StatusOK {
		t.Fatalf("retry: %d", code)
	}
	if r := retried.Metadata.Build; r.ID == id || len(r.Tags) != 1 || r.Tags[0] != "red" {
		t.Errorf("retried Build = %+v, want a new Build from the same config", r)
	}
	e.await2(parent, retried.Metadata.Build.ID, "SUCCESS")

	var list struct{ Builds []restBuild }
	e.rest("GET", parent+"/builds", nil, &list)
	if len(list.Builds) != 2 || list.Builds[0].ID != retried.Metadata.Build.ID {
		t.Errorf("list = %+v, want the retry first", list.Builds)
	}
	e.rest("GET", parent+"/builds?filter=build_id%3D%22"+id+"%22", nil, &list)
	if len(list.Builds) != 1 || list.Builds[0].ID != id {
		t.Errorf("filtered list = %+v, want only %s", list.Builds, id)
	}
	var global struct{ Builds []restBuild }
	e.rest("GET", "/cloudbuild/v1/projects/"+testProject+"/builds", nil, &global)
	if len(global.Builds) != 0 {
		t.Errorf("the global location lists %+v", global.Builds)
	}
}

// await2 polls a regional Build over REST until it has status want.
func (e *env) await2(parent, id, want string) {
	e.t.Helper()
	deadline := time.Now().Add(time.Minute)
	for {
		var b restBuild
		e.rest("GET", parent+"/builds/"+id, nil, &b)
		if b.Status == want {
			return
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("Build %s is %s, want %s", id, b.Status, want)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// The global path, projects/*/builds, serves get, list with a filter and
// retry to the Go client.
func TestGlobalBuilds(t *testing.T) {
	e := start(t)
	op, b := e.create(&cloudbuildpb.Build{Tags: []string{"blue"}, Steps: []*cloudbuildpb.BuildStep{step("", "true")}})
	if _, err := op.Wait(e.ctx); err != nil {
		t.Fatal(err)
	}
	failing, _ := e.create(&cloudbuildpb.Build{Steps: []*cloudbuildpb.BuildStep{step("", "false")}})
	_, _ = failing.Wait(e.ctx)
	retry, err := e.cb.RetryBuild(e.ctx, &cloudbuildpb.RetryBuildRequest{ProjectId: testProject, Id: b.GetId()})
	if err != nil {
		t.Fatal(err)
	}
	again, err := retry.Wait(e.ctx)
	if err != nil || again.GetId() == b.GetId() || again.GetStatus() != cloudbuildpb.Build_SUCCESS {
		t.Fatalf("retry = %v %v, %v", again.GetId(), again.GetStatus(), err)
	}
	for filter, want := range map[string]int{"": 3, `status="SUCCESS"`: 2, `status="FAILURE"`: 1, `tags="blue" AND status="SUCCESS"`: 2, `tags="green"`: 0} {
		it := e.cb.ListBuilds(e.ctx, &cloudbuildpb.ListBuildsRequest{ProjectId: testProject, Filter: filter})
		n := 0
		for {
			_, err := it.Next()
			if err != nil {
				break
			}
			n++
		}
		if n != want {
			t.Errorf("filter %q lists %d Builds, want %d", filter, n, want)
		}
	}
	_, err = e.cb.GetBuild(e.ctx, &cloudbuildpb.GetBuildRequest{ProjectId: testProject, Id: "nope"})
	if status.Code(err) != codes.NotFound {
		t.Errorf("get of a missing Build: %v", err)
	}
	it := e.cb.ListBuilds(e.ctx, &cloudbuildpb.ListBuildsRequest{ProjectId: testProject, Filter: `nonsense="x"`})
	if _, err := it.Next(); status.Code(err) != codes.InvalidArgument {
		t.Errorf("unknown filter field: %v", err)
	}
}

// Cloud Build API v2 is not emulated.
func TestV2Unimplemented(t *testing.T) {
	e := start(t)
	var out struct {
		Error struct{ Status string }
	}
	code := e.rest("GET", "/cloudbuild/v2/projects/"+testProject+"/locations/us-central1/connections", nil, &out)
	if code != http.StatusNotImplemented || out.Error.Status != "UNIMPLEMENTED" {
		t.Errorf("v2: %d %+v", code, out)
	}
}

// A Build the API would refuse is refused.
func TestInvalidBuilds(t *testing.T) {
	e := start(t)
	for name, b := range map[string]*cloudbuildpb.Build{
		"no steps":        {},
		"unknown waitFor": {Steps: []*cloudbuildpb.BuildStep{step("a", "true", "b")}},
		"duplicate id":    {Steps: []*cloudbuildpb.BuildStep{step("a", "true"), step("a", "true")}},
		"script and args": {Steps: []*cloudbuildpb.BuildStep{{Name: busybox, Script: "true", Args: []string{"x"}}}},
		"reserved volume": {Steps: []*cloudbuildpb.BuildStep{{Name: busybox, Volumes: []*cloudbuildpb.Volume{{Name: "v", Path: "/workspace"}}}}},
		"missing image":   {Steps: []*cloudbuildpb.BuildStep{{Args: []string{"true"}}}},
	} {
		_, err := e.cb.CreateBuild(e.ctx, &cloudbuildpb.CreateBuildRequest{ProjectId: testProject, Build: b})
		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s: %v", name, err)
		}
	}
}
