package ar_test

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	artifactregistry "cloud.google.com/go/artifactregistry/apiv1"
	"cloud.google.com/go/artifactregistry/apiv1/artifactregistrypb"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"

	"github.com/linuxuser586/gcpemu/internal/config"
)

// rest issues a JSON request against the gateway REST surface.
func rest(t *testing.T, e *env, method, path, body string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(method, e.inst.GatewayURL()+"/artifactregistry"+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("%s %s: decode %q: %v", method, path, b, err)
	}
	return resp.StatusCode, out
}

func TestRESTRepositoryAndOperations(t *testing.T) {
	e := start(t)
	parent := "/v1/projects/" + testProject + "/locations/" + testLoc
	st, op := rest(t, e, "POST", parent+"/repositories?repositoryId=web", `{"format":"DOCKER","description":"d","labels":{"env":"dev"},"dockerConfig":{"immutableTags":true}}`)
	if st != 200 || op["done"] != true {
		t.Fatalf("create = %d %v", st, op)
	}
	opName, _ := op["name"].(string)
	if !strings.HasPrefix(opName, "projects/"+testProject+"/locations/"+testLoc+"/operations/") {
		t.Fatalf("operation name %q", opName)
	}
	resp, _ := op["response"].(map[string]any)
	if resp["@type"] != "type.googleapis.com/google.devtools.artifactregistry.v1.Repository" || resp["format"] != "DOCKER" {
		t.Fatalf("operation response = %v", resp)
	}
	st, got := rest(t, e, "GET", "/v1/"+opName, "")
	if st != 200 || got["name"] != opName || got["done"] != true {
		t.Fatalf("get operation = %d %v", st, got)
	}
	st, list := rest(t, e, "GET", parent+"/repositories?pageSize=10", "")
	repos, _ := list["repositories"].([]any)
	if st != 200 || len(repos) != 1 {
		t.Fatalf("list = %d %v", st, list)
	}
	r0 := repos[0].(map[string]any)
	if r0["name"] != "projects/"+testProject+"/locations/"+testLoc+"/repositories/web" || r0["mode"] != "STANDARD_REPOSITORY" ||
		r0["dockerConfig"].(map[string]any)["immutableTags"] != true || r0["createTime"] == nil {
		t.Fatalf("repo = %v", r0)
	}
	st, patched := rest(t, e, "PATCH", parent+"/repositories/web?updateMask=description", `{"description":"patched"}`)
	if st != 200 || patched["description"] != "patched" || patched["labels"].(map[string]any)["env"] != "dev" {
		t.Fatalf("patch = %d %v", st, patched)
	}
	st, errBody := rest(t, e, "POST", "/v1/projects/"+testProject+"/locations/nowhere/repositories?repositoryId=x", `{"format":"DOCKER"}`)
	if st != 400 || errBody["error"].(map[string]any)["status"] != "INVALID_ARGUMENT" {
		t.Fatalf("bad location = %d %v", st, errBody)
	}
	st, errBody = rest(t, e, "POST", parent+"/repositories?repositoryId=py", `{"format":"PYTHON"}`)
	if st != 501 {
		t.Fatalf("python repo = %d %v", st, errBody)
	}
	st, del := rest(t, e, "DELETE", parent+"/repositories/web", "")
	if st != 200 || del["done"] != true {
		t.Fatalf("delete = %d %v", st, del)
	}
	st, errBody = rest(t, e, "GET", parent+"/repositories/web", "")
	if st != 404 || errBody["error"].(map[string]any)["status"] != "NOT_FOUND" {
		t.Fatalf("get deleted = %d %v", st, errBody)
	}
	st, locs := rest(t, e, "GET", "/v1/projects/"+testProject+"/locations", "")
	if st != 200 || len(locs["locations"].([]any)) < 40 {
		t.Fatalf("locations = %d", st)
	}
}

func TestRESTOperationPollingWithLatency(t *testing.T) {
	e := start(t, func(c *config.Config) { c.LROLatency["ar"] = "200ms" })
	parent := "/v1/projects/" + testProject + "/locations/" + testLoc
	st, op := rest(t, e, "POST", parent+"/repositories?repositoryId=slow", `{"format":"DOCKER"}`)
	if st != 200 || op["done"] == true {
		t.Fatalf("create = %d %v", st, op)
	}
	name := op["name"].(string)
	deadline := time.Now().Add(10 * time.Second)
	for {
		_, got := rest(t, e, "GET", "/v1/"+name, "")
		if got["done"] == true {
			if got["response"] == nil {
				t.Fatalf("done without response: %v", got)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("operation never completed")
		}
		time.Sleep(50 * time.Millisecond)
	}
	st, list := rest(t, e, "GET", "/v1/projects/"+testProject+"/locations/"+testLoc+"/operations", "")
	if st != 200 || len(list["operations"].([]any)) != 1 {
		t.Fatalf("list operations = %d %v", st, list)
	}
	if st, _ := rest(t, e, "GET", parent+"/repositories/slow", ""); st != 200 {
		t.Fatalf("repo after op = %d", st)
	}
}

// The official REST client exercises path escaping of nested image names.
func TestRESTClientDockerImages(t *testing.T) {
	e := start(t)
	e.createRepo(testLoc, "images")
	img, _ := random.Image(128, 1)
	if err := remote.Write(e.ref(testProject+"/images/team/svc:v1"), img); err != nil {
		t.Fatal(err)
	}
	c, err := artifactregistry.NewRESTClient(e.ctx, option.WithEndpoint(e.inst.GatewayURL()+"/artifactregistry"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	repo := "projects/" + testProject + "/locations/" + testLoc + "/repositories/images"
	it := c.ListDockerImages(e.ctx, &artifactregistrypb.ListDockerImagesRequest{Parent: repo})
	d, err := it.Next()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := it.Next(); !errors.Is(err, iterator.Done) {
		t.Fatalf("extra image: %v", err)
	}
	got, err := c.GetDockerImage(e.ctx, &artifactregistrypb.GetDockerImageRequest{Name: d.GetName()})
	if err != nil || got.GetUri() != d.GetUri() || !strings.Contains(d.GetName(), "/dockerImages/team%2Fsvc@sha256:") {
		t.Fatalf("get = %v %v (%s)", got, err, d.GetName())
	}
	tags := c.ListTags(e.ctx, &artifactregistrypb.ListTagsRequest{Parent: repo + "/packages/team%2Fsvc"})
	tg, err := tags.Next()
	if err != nil || !strings.HasSuffix(tg.GetName(), "/packages/team%2Fsvc/tags/v1") {
		t.Fatalf("tag = %v %v", tg, err)
	}
	op, err := c.DeleteRepository(e.ctx, &artifactregistrypb.DeleteRepositoryRequest{Name: repo})
	if err != nil {
		t.Fatal(err)
	}
	if err := op.Wait(e.ctx); err != nil {
		t.Fatal(err)
	}
}
