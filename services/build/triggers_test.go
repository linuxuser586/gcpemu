package build_test

import (
	"encoding/base64"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/cloudbuild/apiv1/v2/cloudbuildpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// echoBuild is a Build whose one step echoes text, after substitution.
func echoBuild(text string) *cloudbuildpb.Build {
	return &cloudbuildpb.Build{Steps: []*cloudbuildpb.BuildStep{{Name: busybox, Args: []string{"echo", text}}}}
}

// createTrigger creates a Build trigger in the global location.
func (e *env) createTrigger(t *cloudbuildpb.BuildTrigger) *cloudbuildpb.BuildTrigger {
	e.t.Helper()
	got, err := e.cb.CreateBuildTrigger(e.ctx, &cloudbuildpb.CreateBuildTriggerRequest{ProjectId: testProject, Trigger: t})
	if err != nil {
		e.t.Fatalf("CreateBuildTrigger: %v", err)
	}
	return got
}

// triggered polls until a Build trigger has started n Builds, and
// returns them once they are finished, newest first.
func (e *env) triggered(id string, n int) []*cloudbuildpb.Build {
	e.t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for {
		bs := e.listTriggered(id)
		if len(bs) >= n {
			if len(bs) > n {
				e.t.Fatalf("Build trigger %s started %d Builds, want %d", id, len(bs), n)
			}
			for i, b := range bs {
				bs[i] = e.await(b.GetId(), cloudbuildpb.Build_SUCCESS, cloudbuildpb.Build_FAILURE, cloudbuildpb.Build_INTERNAL_ERROR)
			}
			return bs
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("Build trigger %s started %d Builds, want %d", id, len(bs), n)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// listTriggered lists the Builds a Build trigger started.
func (e *env) listTriggered(id string) []*cloudbuildpb.Build {
	e.t.Helper()
	bs, _, err := e.cb.ListBuilds(e.ctx, &cloudbuildpb.ListBuildsRequest{ProjectId: testProject, Filter: `trigger_id="` + id + `"`}).InternalFetch(100, "")
	if err != nil {
		e.t.Fatalf("ListBuilds: %v", err)
	}
	return bs
}

// do sends a REST call with a raw body and returns the status and reply.
func (e *env) do(method, path, body string) (int, string) {
	e.t.Helper()
	req, err := http.NewRequestWithContext(e.ctx, method, e.inst.GatewayURL()+path, strings.NewReader(body))
	if err != nil {
		e.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// Build triggers are created, read by ID or name, listed, patched and
// deleted, globally and in a region; a Build trigger's name is unique in
// its location.
func TestBuildTriggerCRUD(t *testing.T) {
	e := startWith(t, []string{"pubsub"})
	tr := e.createTrigger(&cloudbuildpb.BuildTrigger{
		Name: "crud", Description: "first",
		TriggerTemplate: &cloudbuildpb.RepoSource{RepoName: "repo", Revision: &cloudbuildpb.RepoSource_BranchName{BranchName: "main"}},
		BuildTemplate:   &cloudbuildpb.BuildTrigger_Build{Build: echoBuild("hi")},
	})
	if tr.GetId() == "" || tr.GetCreateTime() == nil || tr.GetResourceName() != "projects/"+testProject+"/locations/global/triggers/"+tr.GetId() {
		t.Fatalf("created = %v", tr)
	}
	for _, id := range []string{tr.GetId(), "crud"} {
		got, err := e.cb.GetBuildTrigger(e.ctx, &cloudbuildpb.GetBuildTriggerRequest{ProjectId: testProject, TriggerId: id})
		if err != nil || got.GetId() != tr.GetId() {
			t.Errorf("GetBuildTrigger %s = %v, %v", id, got, err)
		}
	}
	_, err := e.cb.CreateBuildTrigger(e.ctx, &cloudbuildpb.CreateBuildTriggerRequest{ProjectId: testProject, Trigger: &cloudbuildpb.BuildTrigger{
		Name: "crud", BuildTemplate: &cloudbuildpb.BuildTrigger_Filename{Filename: "cloudbuild.yaml"}}})
	if status.Code(err) != codes.AlreadyExists {
		t.Errorf("a second trigger named crud: %v", err)
	}
	_, err = e.cb.CreateBuildTrigger(e.ctx, &cloudbuildpb.CreateBuildTriggerRequest{ProjectId: testProject, Trigger: &cloudbuildpb.BuildTrigger{Name: "no-template"}})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("a trigger without a build template: %v", err)
	}

	regional, err := e.cb.CreateBuildTrigger(e.ctx, &cloudbuildpb.CreateBuildTriggerRequest{
		Parent:  "projects/" + testProject + "/locations/us-central1",
		Trigger: &cloudbuildpb.BuildTrigger{Name: "crud", BuildTemplate: &cloudbuildpb.BuildTrigger_Filename{Filename: "cloudbuild.yaml"}},
	})
	if err != nil || !strings.HasPrefix(regional.GetResourceName(), "projects/"+testProject+"/locations/us-central1/triggers/") {
		t.Fatalf("regional trigger = %v, %v", regional, err)
	}
	list, _, err := e.cb.ListBuildTriggers(e.ctx, &cloudbuildpb.ListBuildTriggersRequest{ProjectId: testProject}).InternalFetch(10, "")
	if err != nil || len(list) != 1 || list[0].GetId() != tr.GetId() {
		t.Errorf("global list = %v, %v", list, err)
	}

	up, err := e.cb.UpdateBuildTrigger(e.ctx, &cloudbuildpb.UpdateBuildTriggerRequest{
		ProjectId: testProject, TriggerId: tr.GetId(),
		Trigger:    &cloudbuildpb.BuildTrigger{Description: "second", Tags: []string{"x"}},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"description"}},
	})
	if err != nil || up.GetDescription() != "second" || len(up.GetTags()) != 0 || up.GetBuild() == nil || up.GetId() != tr.GetId() {
		t.Errorf("masked update = %v, %v", up, err)
	}
	// Over REST without a mask, as OpenTofu patches, the trigger is replaced.
	code, body := e.do(http.MethodPatch, "/cloudbuild/v1/"+regional.GetResourceName(),
		`{"name":"renamed","filename":"other.yaml","disabled":true}`)
	if code != http.StatusOK || !strings.Contains(body, `"renamed"`) || !strings.Contains(body, `"other.yaml"`) || !strings.Contains(body, regional.GetId()) {
		t.Errorf("REST replace = %d %s", code, body)
	}

	if _, err := e.cb.RunBuildTrigger(e.ctx, &cloudbuildpb.RunBuildTriggerRequest{ProjectId: testProject, TriggerId: "crud"}); status.Code(err) != codes.Unimplemented {
		t.Errorf("running a triggerTemplate trigger without sourceToBuild: %v", err)
	}

	if err := e.cb.DeleteBuildTrigger(e.ctx, &cloudbuildpb.DeleteBuildTriggerRequest{ProjectId: testProject, TriggerId: "crud"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.cb.GetBuildTrigger(e.ctx, &cloudbuildpb.GetBuildTriggerRequest{ProjectId: testProject, TriggerId: tr.GetId()}); status.Code(err) != codes.NotFound {
		t.Errorf("deleted trigger: %v", err)
	}
	code, _ = e.do(http.MethodDelete, "/cloudbuild/v1/"+regional.GetResourceName(), "")
	if code != http.StatusOK {
		t.Errorf("REST delete = %d", code)
	}
}

// A pubsubConfig Build trigger owns a subscription to its topic and
// starts one Build per message, binding substitutions to the message; a
// Cloud Storage notification to the topic starts one too. Deleting the
// Build trigger deletes the subscription.
func TestPubsubBuildTrigger(t *testing.T) {
	e := startWith(t, []string{"pubsub"})
	topic := "projects/" + testProject + "/topics/releases"
	e.send(http.MethodPut, "/pubsub/v1/"+topic, `{}`)
	_, err := e.cb.CreateBuildTrigger(e.ctx, &cloudbuildpb.CreateBuildTriggerRequest{ProjectId: testProject, Trigger: &cloudbuildpb.BuildTrigger{
		Name: "missing", PubsubConfig: &cloudbuildpb.PubsubConfig{Topic: "projects/" + testProject + "/topics/none"},
		BuildTemplate: &cloudbuildpb.BuildTrigger_Build{Build: echoBuild("x")}}})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("a trigger on a missing topic: %v", err)
	}
	tr := e.createTrigger(&cloudbuildpb.BuildTrigger{
		Name:          "on-release",
		PubsubConfig:  &cloudbuildpb.PubsubConfig{Topic: topic},
		Substitutions: map[string]string{"_TAG": "$(body.message.data.tag)", "_KIND": "$(body.message.attributes.kind)", "_OBJ": "$(body.message.attributes.objectId)", "_RAW": "$(body.message.data)"},
		BuildTemplate: &cloudbuildpb.BuildTrigger_Build{Build: echoBuild("tag=$_TAG kind=$_KIND obj=$_OBJ by=$TRIGGER_NAME raw=$_RAW")},
	})
	sub := tr.GetPubsubConfig().GetSubscription()
	if sub != "projects/"+testProject+"/subscriptions/gcb-on-release" || tr.GetPubsubConfig().GetState() != cloudbuildpb.PubsubConfig_OK {
		t.Fatalf("pubsubConfig = %v", tr.GetPubsubConfig())
	}
	if code, _ := e.do(http.MethodGet, "/pubsub/v1/"+sub, ""); code != http.StatusOK {
		t.Fatalf("subscription %s: %d", sub, code)
	}

	data := base64.StdEncoding.EncodeToString([]byte(`{"tag": "v1.2"}`))
	e.send(http.MethodPost, "/pubsub/v1/"+topic+":publish", `{"messages":[{"data":"`+data+`","attributes":{"kind":"release"}}]}`)
	bs := e.triggered(tr.GetId(), 1)
	if b := bs[0]; b.GetStatus() != cloudbuildpb.Build_SUCCESS || !strings.Contains(e.log(b), `tag=v1.2 kind=release obj= by=on-release raw={"tag": "v1.2"}`) {
		t.Errorf("Build %v:\n%s", b.GetStatus(), e.log(b))
	}

	e.send(http.MethodPost, "/storage/v1/b?project="+testProject, `{"name":"trigger-src"}`)
	e.send(http.MethodPost, "/storage/v1/b/trigger-src/notificationConfigs", `{"topic":"`+topic+`","payload_format":"JSON_API_V1","event_types":["OBJECT_FINALIZE"]}`)
	e.send(http.MethodPost, "/upload/storage/v1/b/trigger-src/o?uploadType=media&name=app.tgz", "data")
	bs = e.triggered(tr.GetId(), 2)
	if b := bs[0]; b.GetStatus() != cloudbuildpb.Build_SUCCESS || !strings.Contains(e.log(b), "obj=app.tgz") {
		t.Errorf("Build of the upload %v:\n%s", b.GetStatus(), e.log(b))
	}

	if err := e.cb.DeleteBuildTrigger(e.ctx, &cloudbuildpb.DeleteBuildTriggerRequest{ProjectId: testProject, TriggerId: tr.GetId()}); err != nil {
		t.Fatal(err)
	}
	if code, _ := e.do(http.MethodGet, "/pubsub/v1/"+sub, ""); code != http.StatusNotFound {
		t.Errorf("subscription after the trigger's delete: %d", code)
	}
}

// triggers.webhook starts a Build of a webhookConfig Build trigger only
// with its secret, binding substitutions to the request body.
func TestWebhookBuildTrigger(t *testing.T) {
	e := startWith(t, []string{"secrets"})
	e.secret("hook", "letmein")
	tr := e.createTrigger(&cloudbuildpb.BuildTrigger{
		Name:          "hook",
		WebhookConfig: &cloudbuildpb.WebhookConfig{AuthMethod: &cloudbuildpb.WebhookConfig_Secret{Secret: "projects/" + testProject + "/secrets/hook/versions/1"}},
		Substitutions: map[string]string{"_REF": "$(body.ref)"},
		BuildTemplate: &cloudbuildpb.BuildTrigger_Build{Build: echoBuild("ref=$_REF")},
	})
	if tr.GetWebhookConfig().GetState() != cloudbuildpb.WebhookConfig_OK {
		t.Errorf("webhookConfig = %v", tr.GetWebhookConfig())
	}
	url := "/cloudbuild/v1/projects/" + testProject + "/triggers/hook:webhook"
	for _, q := range []string{"", "?secret=wrong"} {
		if code, body := e.do(http.MethodPost, url+q, `{"ref":"refs/heads/main"}`); code != http.StatusForbidden {
			t.Errorf("webhook with secret %q = %d %s", q, code, body)
		}
	}
	if code, body := e.do(http.MethodPost, url+"?secret=letmein&key=api-key", `{"ref":"refs/heads/main"}`); code != http.StatusOK {
		t.Fatalf("webhook = %d %s", code, body)
	}
	if b := e.triggered(tr.GetId(), 1)[0]; b.GetStatus() != cloudbuildpb.Build_SUCCESS || !strings.Contains(e.log(b), "ref=refs/heads/main") {
		t.Errorf("Build %v:\n%s", b.GetStatus(), e.log(b))
	}
}

// triggers.run builds a Build trigger's inline build at the revision the
// request names, with the built-in substitutions of the trigger and the
// revision. A filename is read from sourceToBuild.
func TestRunBuildTrigger(t *testing.T) {
	e := start(t)
	const repo, commit = "https://github.com/octocat/Hello-World", "553c2077f0edc3d5dc5d17262f6aa498e69d6f8e"
	b := echoBuild("$TRIGGER_NAME $COMMIT_SHA $REPO_NAME $_WHO")
	b.Source = &cloudbuildpb.Source{Source: &cloudbuildpb.Source_GitSource{GitSource: &cloudbuildpb.GitSource{Url: repo}}}
	tr := e.createTrigger(&cloudbuildpb.BuildTrigger{Name: "manual", BuildTemplate: &cloudbuildpb.BuildTrigger_Build{Build: b},
		Substitutions: map[string]string{"_WHO": "trigger"}})
	op, err := e.cb.RunBuildTrigger(e.ctx, &cloudbuildpb.RunBuildTriggerRequest{ProjectId: testProject, TriggerId: "manual",
		Source: &cloudbuildpb.RepoSource{Revision: &cloudbuildpb.RepoSource_CommitSha{CommitSha: commit}, Substitutions: map[string]string{"_WHO": "run"}}})
	if err != nil {
		t.Fatalf("RunBuildTrigger: %v", err)
	}
	got, err := op.Wait(e.ctx)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if got.GetBuildTriggerId() != tr.GetId() || got.GetSourceProvenance().GetResolvedGitSource().GetRevision() != commit {
		t.Errorf("Build = trigger %q, revision %q", got.GetBuildTriggerId(), got.GetSourceProvenance().GetResolvedGitSource().GetRevision())
	}
	if log := e.log(got); !strings.Contains(log, "manual "+commit+" Hello-World run") {
		t.Errorf("log:\n%s", log)
	}

	// Hello-World's README is not a Build config.
	e.createTrigger(&cloudbuildpb.BuildTrigger{Name: "from-file", BuildTemplate: &cloudbuildpb.BuildTrigger_Filename{Filename: "README"},
		SourceToBuild: &cloudbuildpb.GitRepoSource{Uri: repo, Ref: "refs/heads/master", RepoType: cloudbuildpb.GitFileSource_GITHUB}})
	_, err = e.cb.RunBuildTrigger(e.ctx, &cloudbuildpb.RunBuildTriggerRequest{ProjectId: testProject, TriggerId: "from-file"})
	if status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), "README") {
		t.Errorf("running a trigger whose filename is no Build config: %v", err)
	}
	e.createTrigger(&cloudbuildpb.BuildTrigger{Name: "gone", BuildTemplate: &cloudbuildpb.BuildTrigger_Filename{Filename: "cloudbuild.yaml"},
		SourceToBuild: &cloudbuildpb.GitRepoSource{Uri: repo, Ref: "refs/heads/master"}})
	_, err = e.cb.RunBuildTrigger(e.ctx, &cloudbuildpb.RunBuildTriggerRequest{ProjectId: testProject, TriggerId: "gone"})
	if status.Code(err) != codes.FailedPrecondition {
		t.Errorf("running a trigger whose filename is missing: %v", err)
	}
	e.createTrigger(&cloudbuildpb.BuildTrigger{Name: "gh", BuildTemplate: &cloudbuildpb.BuildTrigger_Build{Build: echoBuild("x")},
		Github: &cloudbuildpb.GitHubEventsConfig{Owner: "octocat", Name: "Hello-World",
			Event: &cloudbuildpb.GitHubEventsConfig_Push{Push: &cloudbuildpb.PushFilter{GitRef: &cloudbuildpb.PushFilter_Branch{Branch: "^main$"}}}}})
	if _, err := e.cb.RunBuildTrigger(e.ctx, &cloudbuildpb.RunBuildTriggerRequest{ProjectId: testProject, TriggerId: "gh"}); status.Code(err) != codes.Unimplemented {
		t.Errorf("running a github trigger without sourceToBuild: %v", err)
	}
}

// Worker pools are Recorded through Operations; a Build naming one runs.
func TestWorkerPool(t *testing.T) {
	e := start(t)
	parent := "projects/" + testProject + "/locations/us-central1"
	op, err := e.cb.CreateWorkerPool(e.ctx, &cloudbuildpb.CreateWorkerPoolRequest{Parent: parent, WorkerPoolId: "private",
		WorkerPool: &cloudbuildpb.WorkerPool{DisplayName: "Private", Config: &cloudbuildpb.WorkerPool_PrivatePoolV1Config{
			PrivatePoolV1Config: &cloudbuildpb.PrivatePoolV1Config{WorkerConfig: &cloudbuildpb.PrivatePoolV1Config_WorkerConfig{MachineType: "e2-standard-4", DiskSizeGb: 100}}}}})
	if err != nil {
		t.Fatalf("CreateWorkerPool: %v", err)
	}
	wp, err := op.Wait(e.ctx)
	if err != nil || wp.GetState() != cloudbuildpb.WorkerPool_RUNNING || wp.GetName() != parent+"/workerPools/private" || wp.GetEtag() == "" {
		t.Fatalf("worker pool = %v, %v", wp, err)
	}
	if _, err := e.cb.CreateWorkerPool(e.ctx, &cloudbuildpb.CreateWorkerPoolRequest{Parent: parent, WorkerPoolId: "private", WorkerPool: &cloudbuildpb.WorkerPool{}}); status.Code(err) != codes.AlreadyExists {
		t.Errorf("a second pool named private: %v", err)
	}
	upOp, err := e.cb.UpdateWorkerPool(e.ctx, &cloudbuildpb.UpdateWorkerPoolRequest{
		WorkerPool: &cloudbuildpb.WorkerPool{Name: wp.GetName(), DisplayName: "Renamed"}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"display_name"}}})
	if err != nil {
		t.Fatal(err)
	}
	if up, err := upOp.Wait(e.ctx); err != nil || up.GetDisplayName() != "Renamed" || up.GetPrivatePoolV1Config().GetWorkerConfig().GetMachineType() != "e2-standard-4" {
		t.Errorf("updated pool = %v, %v", up, err)
	}
	if list, _, err := e.cb.ListWorkerPools(e.ctx, &cloudbuildpb.ListWorkerPoolsRequest{Parent: parent}).InternalFetch(10, ""); err != nil || len(list) != 1 {
		t.Errorf("list = %v, %v", list, err)
	}

	b := echoBuild("pooled")
	b.Options = &cloudbuildpb.BuildOptions{Pool: &cloudbuildpb.BuildOptions_PoolOption{Name: wp.GetName()}}
	if got := e.run(b); got.GetStatus() != cloudbuildpb.Build_SUCCESS {
		t.Errorf("Build on the pool = %v", got.GetStatus())
	}

	delOp, err := e.cb.DeleteWorkerPool(e.ctx, &cloudbuildpb.DeleteWorkerPoolRequest{Name: wp.GetName()})
	if err != nil {
		t.Fatal(err)
	}
	if err := delOp.Wait(e.ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := e.cb.GetWorkerPool(e.ctx, &cloudbuildpb.GetWorkerPoolRequest{Name: wp.GetName()}); status.Code(err) != codes.NotFound {
		t.Errorf("deleted pool: %v", err)
	}
}

// A Seed file's Build triggers and worker pools exist after Seeding,
// Seeding again changes nothing, and the export recreates them.
func TestSeedBuildTriggers(t *testing.T) {
	e := startWith(t, []string{"pubsub"})
	seed := `pubsub:
  topics:
    - {name: seeded, project: ` + testProject + `}
build:
  triggers:
    - project: ` + testProject + `
      name: seeded
      pubsubConfig: {topic: projects/` + testProject + `/topics/seeded}
      substitutions: {_TAG: $(body.message.data)}
      build:
        steps:
          - {name: ` + busybox + `, args: [echo, $_TAG]}
  workerPools:
    - project: ` + testProject + `
      location: us-central1
      id: seeded
      displayName: Seeded
`
	for range 2 {
		if code, body := e.do(http.MethodPost, "/_emu/v1/seed", seed); code != http.StatusOK {
			t.Fatalf("seed = %d %s", code, body)
		}
	}
	list, _, err := e.cb.ListBuildTriggers(e.ctx, &cloudbuildpb.ListBuildTriggersRequest{ProjectId: testProject}).InternalFetch(10, "")
	if err != nil || len(list) != 1 || list[0].GetName() != "seeded" || list[0].GetPubsubConfig().GetSubscription() == "" {
		t.Fatalf("triggers after Seeding twice = %v, %v", list, err)
	}
	_, first := e.do(http.MethodGet, "/_emu/v1/seed", "")
	for _, want := range []string{"name: seeded", "$(body.message.data)", "displayName: Seeded"} {
		if !strings.Contains(first, want) {
			t.Errorf("export lacks %q:\n%s", want, first)
		}
	}
	if strings.Contains(first, "gcb-seeded") {
		t.Errorf("export has the trigger's subscription:\n%s", first)
	}
	e.do(http.MethodPost, "/_emu/v1/reset", "")
	if code, body := e.do(http.MethodPost, "/_emu/v1/seed", first); code != http.StatusOK {
		t.Fatalf("seed of the export = %d %s", code, body)
	}
	if _, second := e.do(http.MethodGet, "/_emu/v1/seed", ""); second != first {
		t.Errorf("export after re-seeding differs:\n%s\nwant\n%s", second, first)
	}
	list, _, _ = e.cb.ListBuildTriggers(e.ctx, &cloudbuildpb.ListBuildTriggersRequest{ProjectId: testProject}).InternalFetch(10, "")
	if len(list) != 1 {
		t.Fatalf("triggers after re-seeding = %v", list)
	}
	e.send(http.MethodPost, "/pubsub/v1/projects/"+testProject+"/topics/seeded:publish", `{"messages":[{"data":"`+base64.StdEncoding.EncodeToString([]byte("from-seed"))+`"}]}`)
	if b := e.triggered(list[0].GetId(), 1)[0]; !strings.Contains(e.log(b), "from-seed") {
		t.Errorf("Build of the seeded trigger:\n%s", e.log(b))
	}
}
