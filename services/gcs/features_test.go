package gcs_test

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/iam"
	"cloud.google.com/go/storage"
	"gopkg.in/yaml.v3"
)

func pemKey(f *fakeIAM) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(f.key)})
}

func TestSignedURL(t *testing.T) {
	inst := start(t)
	c := emulatorClient(t, inst)
	ctx := context.Background()
	const sa = "signer@test-project.iam.gserviceaccount.com"
	fk := newFakeIAM(t, sa)
	inject(inst, fk)
	b := c.Bucket("signed")
	if err := b.Create(ctx, testProject, nil); err != nil {
		t.Fatal(err)
	}
	write(t, ctx, b.Object("dir/file name.txt"), []byte("secret"), 0)

	opts := func(method string, exp time.Duration) *storage.SignedURLOptions {
		return &storage.SignedURLOptions{
			GoogleAccessID: sa, PrivateKey: pemKey(fk), Method: method, Expires: time.Now().Add(exp),
			Scheme: storage.SigningSchemeV4, Insecure: true,
		}
	}
	u, err := b.SignedURL("dir/file name.txt", opts("GET", time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(body) != "secret" {
		t.Fatalf("signed GET = %d %q", resp.StatusCode, body)
	}

	// Tampered signature.
	bad := strings.Replace(u, "X-Goog-Signature=", "X-Goog-Signature=00", 1)
	resp, _ = http.Get(bad)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 403 || !strings.Contains(string(body), "SignatureDoesNotMatch") {
		t.Errorf("tampered = %d %s", resp.StatusCode, body)
	}

	// Signed PUT.
	pu, err := b.SignedURL("put.txt", &storage.SignedURLOptions{
		GoogleAccessID: sa, PrivateKey: pemKey(fk), Method: "PUT", Expires: time.Now().Add(time.Hour),
		Scheme: storage.SigningSchemeV4, Insecure: true, ContentType: "text/plain",
	})
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest("PUT", pu, strings.NewReader("uploaded"))
	req.Header.Set("Content-Type", "text/plain")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("signed PUT = %d", resp.StatusCode)
	}
	if got := read(t, ctx, b.Object("put.txt")); string(got) != "uploaded" {
		t.Errorf("put content = %q", got)
	}

	// Expiry is checked against the emulator clock.
	_ = inst.AdvanceClock(context.Background(), 2*time.Hour)
	resp, _ = http.Get(u)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 400 || !strings.Contains(string(body), "ExpiredToken") {
		t.Errorf("expired = %d %s", resp.StatusCode, body)
	}
}

func TestPostPolicy(t *testing.T) {
	inst := start(t)
	c := emulatorClient(t, inst)
	ctx := context.Background()
	const sa = "poster@test-project.iam.gserviceaccount.com"
	fk := newFakeIAM(t, sa)
	inject(inst, fk)
	b := c.Bucket("posts")
	if err := b.Create(ctx, testProject, nil); err != nil {
		t.Fatal(err)
	}
	pp, err := b.GenerateSignedPostPolicyV4("form.txt", &storage.PostPolicyV4Options{
		GoogleAccessID: sa, PrivateKey: pemKey(fk), Expires: time.Now().Add(time.Hour), Insecure: true,
		Fields:     &storage.PolicyV4Fields{ContentType: "text/plain", StatusCodeOnSuccess: 201},
		Conditions: []storage.PostPolicyV4Condition{storage.ConditionContentLengthRange(0, 100)},
	})
	if err != nil {
		t.Fatal(err)
	}
	post := func(content string) *http.Response {
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		for k, v := range pp.Fields {
			_ = mw.WriteField(k, v)
		}
		fw, _ := mw.CreateFormFile("file", "form.txt")
		_, _ = fw.Write([]byte(content))
		_ = mw.Close()
		resp, err := http.Post(pp.URL, mw.FormDataContentType(), &buf)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	resp := post("posted!")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 201 {
		t.Fatalf("post = %d %s", resp.StatusCode, body)
	}
	a, err := b.Object("form.txt").Attrs(ctx)
	if err != nil || a.ContentType != "text/plain" || a.Size != 7 {
		t.Fatalf("attrs = %+v %v", a, err)
	}
	resp = post(strings.Repeat("x", 200))
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Errorf("oversized post = %d", resp.StatusCode)
	}
}

func TestNotifications(t *testing.T) {
	inst := start(t)
	c := emulatorClient(t, inst)
	ctx := context.Background()
	ps := &fakePubSub{topics: map[string]bool{"projects/test-project/topics/events": true}}
	inject(inst, ps)
	b := c.Bucket("notify")
	if err := b.Create(ctx, testProject, &storage.BucketAttrs{VersioningEnabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.AddNotification(ctx, &storage.Notification{TopicProjectID: testProject, TopicID: "missing", PayloadFormat: storage.JSONPayload}); httpCode(err) != 400 {
		t.Errorf("missing topic err = %v", err)
	}
	n, err := b.AddNotification(ctx, &storage.Notification{
		TopicProjectID: testProject, TopicID: "events", PayloadFormat: storage.JSONPayload,
		ObjectNamePrefix: "in/", CustomAttributes: map[string]string{"app": "x"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if n.ID == "" || n.TopicID != "events" || n.TopicProjectID != testProject {
		t.Errorf("notification = %+v", n)
	}
	all, err := b.Notifications(ctx)
	if err != nil || len(all) != 1 {
		t.Fatalf("list = %v %v", all, err)
	}

	o := b.Object("in/a.txt")
	a1 := write(t, ctx, o, []byte("1"), 0)
	a2 := write(t, ctx, o, []byte("22"), 0) // archives a1
	if _, err := o.Update(ctx, storage.ObjectAttrsToUpdate{ContentType: "text/plain"}); err != nil {
		t.Fatal(err)
	}
	if err := o.Generation(a1.Generation).Delete(ctx); err != nil {
		t.Fatal(err)
	}
	write(t, ctx, b.Object("out/ignored"), []byte("x"), 0)

	msgs := ps.messages()
	var types []string
	for _, m := range msgs {
		types = append(types, m.attrs["eventType"])
		if m.topic != "projects/test-project/topics/events" || m.attrs["bucketId"] != "notify" || m.attrs["objectId"] != "in/a.txt" ||
			m.attrs["payloadFormat"] != "JSON_API_V1" || m.attrs["app"] != "x" || m.attrs["eventTime"] == "" ||
			m.attrs["notificationConfig"] != "projects/_/buckets/notify/notificationConfigs/"+n.ID {
			t.Errorf("attrs = %v", m.attrs)
		}
		var obj map[string]any
		if err := json.Unmarshal(m.data, &obj); err != nil || obj["kind"] != "storage#object" || obj["name"] != "in/a.txt" {
			t.Errorf("payload = %s", m.data)
		}
	}
	want := []string{"OBJECT_FINALIZE", "OBJECT_ARCHIVE", "OBJECT_FINALIZE", "OBJECT_METADATA_UPDATE", "OBJECT_DELETE"}
	if !equal(types, want) {
		t.Fatalf("events = %v, want %v", types, want)
	}
	if msgs[1].attrs["overwrittenByGeneration"] != fmt.Sprint(a2.Generation) || msgs[2].attrs["overwroteGeneration"] != fmt.Sprint(a1.Generation) {
		t.Errorf("overwrite attrs: %v / %v", msgs[1].attrs, msgs[2].attrs)
	}
	if err := b.DeleteNotification(ctx, n.ID); err != nil {
		t.Fatal(err)
	}
}

func TestBucketIAM(t *testing.T) {
	inst := start(t)
	c := emulatorClient(t, inst)
	ctx := context.Background()
	b := c.Bucket("iam-bucket")
	if err := b.Create(ctx, testProject, nil); err != nil {
		t.Fatal(err)
	}
	h := b.IAM()
	p, err := h.Policy(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !p.HasRole("projectOwner:"+testProject, "roles/storage.legacyBucketOwner") {
		t.Errorf("default policy roles = %v", p.Roles())
	}
	p.Add("user:alice@example.com", iam.RoleName("roles/storage.objectViewer"))
	if err := h.SetPolicy(ctx, p); err != nil {
		t.Fatal(err)
	}
	p2, err := h.Policy(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !p2.HasRole("user:alice@example.com", "roles/storage.objectViewer") {
		t.Errorf("policy after set = %v", p2.Roles())
	}
	// Stale etag.
	if err := h.SetPolicy(ctx, p); httpCode(err) != 412 {
		t.Errorf("stale etag err = %v", err)
	}
	perms, err := h.TestPermissions(ctx, []string{"storage.buckets.get", "storage.objects.list"})
	if err != nil || len(perms) != 2 {
		t.Errorf("test permissions = %v %v", perms, err)
	}
}

func TestHMACKeys(t *testing.T) {
	inst := start(t)
	c := emulatorClient(t, inst)
	ctx := context.Background()
	const sa = "hmac@test-project.iam.gserviceaccount.com"
	k, err := c.CreateHMACKey(ctx, testProject, sa)
	if err != nil {
		t.Fatal(err)
	}
	if k.AccessID == "" || k.Secret == "" || k.State != storage.Active {
		t.Fatalf("key = %+v", k)
	}
	h := c.HMACKeyHandle(testProject, k.AccessID)
	if err := h.Delete(ctx); httpCode(err) != 400 {
		t.Errorf("delete active err = %v", err)
	}
	if _, err := h.Update(ctx, storage.HMACKeyAttrsToUpdate{State: storage.Inactive}); err != nil {
		t.Fatal(err)
	}
	it := c.ListHMACKeys(ctx, testProject)
	if got, err := it.Next(); err != nil || got.AccessID != k.AccessID || got.State != storage.Inactive {
		t.Errorf("list = %+v %v", got, err)
	}
	if err := h.Delete(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestLifecycle(t *testing.T) {
	inst := start(t)
	c := emulatorClient(t, inst)
	ctx := context.Background()
	b := c.Bucket("lifecycle")
	err := b.Create(ctx, testProject, &storage.BucketAttrs{Lifecycle: storage.Lifecycle{Rules: []storage.LifecycleRule{
		{Action: storage.LifecycleAction{Type: storage.DeleteAction}, Condition: storage.LifecycleCondition{AgeInDays: 2, MatchesPrefix: []string{"tmp/"}}},
		{Action: storage.LifecycleAction{Type: storage.SetStorageClassAction, StorageClass: "COLDLINE"}, Condition: storage.LifecycleCondition{AgeInDays: 1}},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	write(t, ctx, b.Object("tmp/x"), []byte("x"), 0)
	write(t, ctx, b.Object("keep"), []byte("y"), 0)
	svc := gcsService(t, inst)
	if err := svc.EvaluateLifecycle(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Object("tmp/x").Attrs(ctx); err != nil {
		t.Fatalf("deleted too early: %v", err)
	}
	_ = inst.AdvanceClock(context.Background(), 3*24*time.Hour)
	if err := svc.EvaluateLifecycle(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Object("tmp/x").Attrs(ctx); !errors.Is(err, storage.ErrObjectNotExist) {
		t.Errorf("tmp/x after lifecycle: %v", err)
	}
	if a, err := b.Object("keep").Attrs(ctx); err != nil || a.StorageClass != "COLDLINE" {
		t.Errorf("keep = %+v %v", a, err)
	}
}

func TestSeed(t *testing.T) {
	inst := start(t)
	c := emulatorClient(t, inst)
	ctx := context.Background()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "site", "css"), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(dir, "site", "index.html"), []byte("<h1>hi</h1>"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "site", "css", "a.css"), []byte("body{}"), 0o644)
	doc := `
buckets:
  - name: seeded
    project: test-project
    versioning: true
    labels: {env: test}
    website: {mainPageSuffix: index.html}
    objects:
      - path: site
        name: static/
      - name: hello.txt
        content: hello
`
	var node yaml.Node
	if err := yaml.Unmarshal([]byte(doc), &node); err != nil {
		t.Fatal(err)
	}
	svc := gcsService(t, inst)
	for i := 0; i < 2; i++ { // idempotent
		if err := svc.ApplySeed(ctx, node.Content[0], dir); err != nil {
			t.Fatal(err)
		}
	}
	b := c.Bucket("seeded")
	a, err := b.Attrs(ctx)
	if err != nil || !a.VersioningEnabled || a.Labels["env"] != "test" || a.Website == nil {
		t.Fatalf("bucket = %+v %v", a, err)
	}
	if got := read(t, ctx, b.Object("static/css/a.css")); string(got) != "body{}" {
		t.Errorf("a.css = %q", got)
	}
	oa, err := b.Object("static/index.html").Attrs(ctx)
	if err != nil || !strings.HasPrefix(oa.ContentType, "text/html") {
		t.Errorf("index attrs = %+v %v", oa, err)
	}
	if got := read(t, ctx, b.Object("hello.txt")); string(got) != "hello" {
		t.Errorf("hello = %q", got)
	}
	// Re-applying did not create new generations.
	n := 0
	it := b.Objects(ctx, &storage.Query{Versions: true})
	for {
		if _, err := it.Next(); err != nil {
			break
		}
		n++
	}
	if n != 3 {
		t.Errorf("versions after re-seed = %d, want 3", n)
	}
}

func TestEnvVars(t *testing.T) {
	inst := start(t)
	ev := inst.EnvVars()
	if ev["STORAGE_EMULATOR_HOST"] != inst.Endpoint("gcs") || ev["CLOUDSDK_API_ENDPOINT_OVERRIDES_STORAGE"] != inst.GatewayURL()+"/storage/v1/" {
		t.Errorf("env = %v", ev)
	}
}
