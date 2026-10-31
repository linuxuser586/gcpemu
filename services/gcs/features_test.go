package gcs_test

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/iam"
	"cloud.google.com/go/storage"
	"go.yaml.in/yaml/v3"
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

	// response-* overrides are part of the signature and applied once it checks.
	ou, err := b.SignedURL("dir/file name.txt", &storage.SignedURLOptions{
		GoogleAccessID: sa, PrivateKey: pemKey(fk), Method: "GET", Expires: time.Now().Add(time.Hour),
		Scheme: storage.SigningSchemeV4, Insecure: true,
		QueryParameters: url.Values{
			"response-content-type":        {"text/plain"},
			"response-content-disposition": {`attachment; filename="notes.txt"`},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	resp, err = http.Get(ou)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/plain" || resp.Header.Get("Content-Disposition") != `attachment; filename="notes.txt"` {
		t.Errorf("signed overrides = %d %v", resp.StatusCode, resp.Header)
	}
	resp, _ = http.Get(strings.Replace(ou, "response-content-type=text%2Fplain", "response-content-type=text%2Fhtml", 1))
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 403 || resp.Header.Get("Content-Type") == "text/html" {
		t.Errorf("tampered override = %d %s", resp.StatusCode, body)
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
	_ = inst.Internal().AdvanceClock(context.Background(), 2*time.Hour)
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
	_ = inst.Internal().AdvanceClock(context.Background(), 3*24*time.Hour)
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

// gcloudSignedURL signs a GET of /bucket/object at endpoint the way gcloud
// storage sign-url does (googlecloudsdk/command_lib/storage/sign_url_util.py):
// path style, lowercase x-goog-* parameter names.
func gcloudSignedURL(t *testing.T, f *fakeIAM, endpoint, bucket, object string, at time.Time, expires int) string {
	t.Helper()
	host := strings.TrimPrefix(endpoint, "http://")
	path := "/" + bucket + "/" + strings.ReplaceAll(url.PathEscape(object), "%2F", "/")
	scope := at.UTC().Format("20060102") + "/us-central1/storage/goog4_request"
	date := at.UTC().Format("20060102T150405Z")
	q := url.Values{
		"x-goog-algorithm":     {"GOOG4-RSA-SHA256"},
		"x-goog-credential":    {f.email + "/" + scope},
		"x-goog-date":          {date},
		"x-goog-signedheaders": {"host"},
		"x-goog-expires":       {strconv.Itoa(expires)},
	}
	query := q.Encode() // sorted, quote_plus-style, as gcloud builds it
	creq := strings.Join([]string{"GET", path, query, "host:" + host + "\n", "host", "UNSIGNED-PAYLOAD"}, "\n")
	ch := sha256.Sum256([]byte(creq))
	sts := strings.Join([]string{"GOOG4-RSA-SHA256", date, scope, hex.EncodeToString(ch[:])}, "\n")
	sum := sha256.Sum256([]byte(sts))
	sig, err := rsa.SignPKCS1v15(rand.Reader, f.key, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	return endpoint + path + "?x-goog-signature=" + hex.EncodeToString(sig) + "&" + query
}

// gcloud's signed URLs name their parameters in lowercase (#599): they are
// verified, not served as unsigned requests.
func TestSignedURLLowercaseParameters(t *testing.T) {
	inst := start(t)
	c := emulatorClient(t, inst)
	ctx := context.Background()
	const sa = "signer@test-project.iam.gserviceaccount.com"
	fk := newFakeIAM(t, sa)
	inject(inst, fk)
	b := c.Bucket("lowercase")
	if err := b.Create(ctx, testProject, nil); err != nil {
		t.Fatal(err)
	}
	write(t, ctx, b.Object("dir/obj.txt"), []byte("lower"), 0)
	ep := inst.EnvVars()["CLOUDSDK_STORAGE_GS_XML_ENDPOINT_URL"]
	get := func(u string) (int, string) {
		t.Helper()
		resp, err := http.Get(u)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp.StatusCode, string(body)
	}

	u := gcloudSignedURL(t, fk, ep, "lowercase", "dir/obj.txt", time.Now(), 600)
	if code, body := get(u); code != 200 || body != "lower" {
		t.Fatalf("signed GET = %d %q", code, body)
	}
	i := strings.Index(u, "x-goog-signature=") + len("x-goog-signature=")
	digit := "0"
	if u[i] == '0' {
		digit = "1"
	}
	tampered := u[:i] + digit + u[i+1:]
	if code, body := get(tampered); code != 403 || !strings.Contains(body, "SignatureDoesNotMatch") {
		t.Errorf("tampered = %d %s", code, body)
	}
	expired := gcloudSignedURL(t, fk, ep, "lowercase", "dir/obj.txt", time.Now().Add(-time.Hour), 60)
	if code, body := get(expired); code != 400 || !strings.Contains(body, "ExpiredToken") {
		t.Errorf("expired = %d %s", code, body)
	}
}

// gcloud storage sign-url signs for, and probes, the host in
// storage/gs_xml_endpoint_url (#599): a path-style signed URL there must
// serve the object.
func TestEnvVarsXMLEndpointServesSignedURL(t *testing.T) {
	inst := start(t)
	c := emulatorClient(t, inst)
	ctx := context.Background()
	const sa = "signer@test-project.iam.gserviceaccount.com"
	fk := newFakeIAM(t, sa)
	inject(inst, fk)
	b := c.Bucket("xml-endpoint")
	if err := b.Create(ctx, testProject, nil); err != nil {
		t.Fatal(err)
	}
	write(t, ctx, b.Object("dir/obj.txt"), []byte("via xml"), 0)

	ep, err := url.Parse(inst.EnvVars()["CLOUDSDK_STORAGE_GS_XML_ENDPOINT_URL"])
	if err != nil || ep.Scheme != "http" || ep.Host == "" || strings.Trim(ep.Path, "/") != "" {
		t.Fatalf("CLOUDSDK_STORAGE_GS_XML_ENDPOINT_URL = %q (%v), want http://host:port", ep, err)
	}
	u, err := b.SignedURL("dir/obj.txt", &storage.SignedURLOptions{
		GoogleAccessID: sa, PrivateKey: pemKey(fk), Method: "GET", Expires: time.Now().Add(time.Hour),
		Scheme: storage.SigningSchemeV4, Insecure: true, Hostname: ep.Host, Style: storage.PathStyle(),
	})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(body) != "via xml" {
		t.Fatalf("signed GET of %s = %d %q", u, resp.StatusCode, body)
	}
}

func TestSeedExportRoundTrip(t *testing.T) {
	inst := start(t)
	c := emulatorClient(t, inst)
	ctx := context.Background()
	doc := `
gcs:
  buckets:
    - name: assets
      project: test-project
      location: EU
      storageClass: NEARLINE
      versioning: true
      uniformBucketLevelAccess: true
      labels: {env: test}
      lifecycle:
        - action: {type: Delete}
          condition: {age: 30}
      website: {mainPageSuffix: index.html, notFoundPage: 404.html}
      cors:
        - origin: ["*"]
          method: [GET]
          maxAgeSeconds: 3600
      objects:
        - name: hello.txt
          content: hello
        - name: data.bin
          contentBase64: AAH/
          contentType: application/x-custom
          cacheControl: no-cache
          metadata: {k: v}
      notifications:
        - topic: projects/test-project/topics/uploads
          eventTypes: [OBJECT_FINALIZE]
        - topic: projects/test-project/topics/all
          payloadFormat: NONE
          objectNamePrefix: logs/
          customAttributes: {a: b}
    - name: locked
      project: test-project
      retentionPeriod: 60
      iamConfiguration: {publicAccessPrevention: enforced}
      softDeletePolicy: {retentionDurationSeconds: "0"}
`
	if err := inst.Internal().ApplySeedData(ctx, []byte(doc), t.TempDir()); err != nil {
		t.Fatal(err)
	}
	// Binary content written through the API; the overwritten generation
	// is noncurrent and not exported.
	b := c.Bucket("assets")
	for _, data := range [][]byte{[]byte("old"), {0xff, 0xfe, 'x'}} {
		w := b.Object("img.raw").NewWriter(ctx)
		w.Metadata = map[string]string{"src": "api"}
		_, _ = w.Write(data)
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
	}
	first, err := inst.Internal().ExportSeed(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"contentBase64: AAH/", "contentBase64: //54", "content: hello", "payloadFormat: NONE", "storageClass: NEARLINE"} {
		if !bytes.Contains(first, []byte(want)) {
			t.Errorf("export lacks %q:\n%s", want, first)
		}
	}
	for _, unwanted := range []string{"old", "timeCreated", "etag", "md5Hash", "lockedTime", "effectiveTime"} {
		if bytes.Contains(first, []byte(unwanted)) {
			t.Errorf("export contains %q:\n%s", unwanted, first)
		}
	}
	if err := inst.Internal().Reset(ctx); err != nil {
		t.Fatal(err)
	}
	if err := inst.Internal().ApplySeedData(ctx, first, t.TempDir()); err != nil {
		t.Fatalf("apply export: %v\n%s", err, first)
	}
	second, err := inst.Internal().ExportSeed(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatalf("export changed after round trip:\n--- first\n%s\n--- second\n%s", first, second)
	}
	a, err := b.Attrs(ctx)
	if err != nil || a.Location != "EU" || !a.VersioningEnabled || !a.UniformBucketLevelAccess.Enabled ||
		len(a.CORS) != 1 || len(a.Lifecycle.Rules) != 1 {
		t.Errorf("assets = %+v %v", a, err)
	}
	if a, err := c.Bucket("locked").Attrs(ctx); err != nil || a.RetentionPolicy == nil || a.RetentionPolicy.RetentionPeriod != time.Minute ||
		a.PublicAccessPrevention != storage.PublicAccessPreventionEnforced {
		t.Errorf("locked = %+v %v", a, err)
	}
	if got := read(t, ctx, b.Object("img.raw")); !bytes.Equal(got, []byte{0xff, 0xfe, 'x'}) {
		t.Errorf("img.raw = %q", got)
	}
	oa, err := b.Object("data.bin").Attrs(ctx)
	if err != nil || oa.ContentType != "application/x-custom" || oa.CacheControl != "no-cache" || oa.Metadata["k"] != "v" {
		t.Errorf("data.bin = %+v %v", oa, err)
	}
	ns, err := b.Notifications(ctx)
	if err != nil || len(ns) != 2 {
		t.Errorf("notifications = %v %v", ns, err)
	}
}
