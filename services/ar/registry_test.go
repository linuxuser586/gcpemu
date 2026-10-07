package ar_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/authn"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/types"

	"github.com/linuxuser586/gcpemu/emutest"
	"github.com/linuxuser586/gcpemu/internal/config"
)

func TestRegistryPushPullImage(t *testing.T) {
	e := start(t)
	e.createRepo(testLoc, "images")
	img, err := random.Image(1024, 3)
	if err != nil {
		t.Fatal(err)
	}
	ref := e.ref(testProject + "/images/app:v1")
	if err := remote.Write(ref, img); err != nil {
		t.Fatalf("push: %v", err)
	}
	got, err := remote.Image(ref)
	if err != nil {
		t.Fatalf("pull: %v", err)
	}
	wd, _ := img.Digest()
	gd, _ := got.Digest()
	if wd != gd {
		t.Fatalf("digest = %s, want %s", gd, wd)
	}
	layers, err := got.Layers()
	if err != nil || len(layers) != 3 {
		t.Fatalf("layers = %d, %v", len(layers), err)
	}
	// Reading layer bytes verifies blob content against its digest.
	for _, l := range layers {
		rc, err := l.Compressed()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.Copy(io.Discard, rc); err != nil {
			t.Fatalf("read layer: %v", err)
		}
		rc.Close()
	}
	// Same image through the location-in-path form.
	alt := e.ref(testLoc + "-docker.pkg.dev/" + testProject + "/images/app:v1")
	if d, err := remote.Head(alt); err != nil || d.Digest != wd {
		t.Fatalf("head via location path: %v %v", d, err)
	}
	tags, err := remote.List(ref.Context())
	if err != nil || len(tags) != 1 || tags[0] != "v1" {
		t.Fatalf("tags = %v, %v", tags, err)
	}
}

func TestRegistryIndexAndMount(t *testing.T) {
	e := start(t)
	e.createRepo(testLoc, "images")
	idx, err := random.Index(512, 2, 3)
	if err != nil {
		t.Fatal(err)
	}
	ref := e.ref(testProject + "/images/multi:latest")
	if err := remote.WriteIndex(ref, idx); err != nil {
		t.Fatalf("push index: %v", err)
	}
	got, err := remote.Index(ref)
	if err != nil {
		t.Fatal(err)
	}
	im, err := got.IndexManifest()
	if err != nil || len(im.Manifests) != 3 {
		t.Fatalf("index manifests = %v, %v", im, err)
	}
	if mt, _ := got.MediaType(); mt != types.OCIImageIndex {
		t.Fatalf("media type = %s", mt)
	}
	child, err := got.Image(im.Manifests[0].Digest)
	if err != nil {
		t.Fatal(err)
	}
	// Re-push a pulled image to another image path: layers are mounted
	// from the source repository rather than re-uploaded.
	dst := e.ref(testProject + "/images/copy:v1")
	if err := remote.Write(dst, child); err != nil {
		t.Fatalf("push with mount: %v", err)
	}
	if _, err := remote.Image(dst); err != nil {
		t.Fatal(err)
	}
	cl, _ := child.Layers()
	d, _ := cl[0].Digest()
	resp := do(t, "HEAD", "http://"+e.registry()+"/v2/"+testProject+"/images/copy/blobs/"+d.String(), nil, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("mounted blob HEAD = %d", resp.StatusCode)
	}
}

func TestRegistryReferrers(t *testing.T) {
	e := start(t)
	e.createRepo(testLoc, "images")
	img, _ := random.Image(256, 1)
	ref := e.ref(testProject + "/images/app:v1")
	if err := remote.Write(ref, img); err != nil {
		t.Fatal(err)
	}
	desc, err := remote.Get(ref)
	if err != nil {
		t.Fatal(err)
	}
	subject := desc.Descriptor
	for i, at := range []string{"application/vnd.example.sbom", "application/vnd.example.sig"} {
		art := mutate.MediaType(empty.Image, types.OCIManifestSchema1)
		art = mutate.ConfigMediaType(art, types.MediaType(at))
		art = mutate.Annotations(art, map[string]string{"n": fmt.Sprint(i)}).(v1.Image)
		art = mutate.Subject(art, subject).(v1.Image)
		ad, _ := art.Digest()
		dref := e.ref(testProject + "/images/app@" + ad.String())
		if err := remote.Write(dref, art); err != nil {
			t.Fatalf("push referrer: %v", err)
		}
	}
	dig := ref.Context().Digest(subject.Digest.String())
	idx, err := remote.Referrers(dig)
	if err != nil {
		t.Fatal(err)
	}
	im, _ := idx.IndexManifest()
	if len(im.Manifests) != 2 {
		t.Fatalf("referrers = %d, want 2", len(im.Manifests))
	}
	idx, err = remote.Referrers(dig, remote.WithFilter("artifactType", "application/vnd.example.sig"))
	if err != nil {
		t.Fatal(err)
	}
	im, _ = idx.IndexManifest()
	if len(im.Manifests) != 1 || im.Manifests[0].ArtifactType != "application/vnd.example.sig" {
		t.Fatalf("filtered referrers = %+v", im.Manifests)
	}
	// OCI-Subject header on push.
	art := mutate.Subject(mutate.ConfigMediaType(mutate.MediaType(empty.Image, types.OCIManifestSchema1), "application/x"), subject).(v1.Image)
	raw, _ := art.RawManifest()
	cfg, _ := art.RawConfigFile()
	cd, _ := art.ConfigName()
	base := "http://" + e.registry() + "/v2/" + testProject + "/images/app"
	if r := do(t, "POST", base+"/blobs/uploads/?digest="+cd.String(), bytes.NewReader(cfg), nil); r.StatusCode != 201 {
		t.Fatalf("monolithic upload = %d", r.StatusCode)
	}
	r := do(t, "PUT", base+"/manifests/sub", bytes.NewReader(raw), map[string]string{"Content-Type": string(types.OCIManifestSchema1)})
	if r.StatusCode != 201 || r.Header.Get("OCI-Subject") != subject.Digest.String() {
		t.Fatalf("PUT = %d, OCI-Subject %q", r.StatusCode, r.Header.Get("OCI-Subject"))
	}
}

// do issues a raw registry request with an anonymous token.
func do(t *testing.T, method, url string, body io.Reader, hdr map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer anonymous")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(b))
	return resp
}

func sha(b []byte) string {
	s := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(s[:])
}

func TestRegistryChunkedUploadAndRange(t *testing.T) {
	e := start(t)
	e.createRepo(testLoc, "images")
	base := "http://" + e.registry() + "/v2/" + testProject + "/images/raw"
	data := bytes.Repeat([]byte("0123456789"), 100)
	r := do(t, "POST", base+"/blobs/uploads/", nil, nil)
	if r.StatusCode != 202 || r.Header.Get("Docker-Upload-UUID") == "" || r.Header.Get("Range") != "0-0" {
		t.Fatalf("POST = %d %v", r.StatusCode, r.Header)
	}
	loc := "http://" + e.registry() + r.Header.Get("Location")
	r = do(t, "PATCH", loc, bytes.NewReader(data[:400]), map[string]string{"Content-Range": "0-399", "Content-Type": "application/octet-stream"})
	if r.StatusCode != 202 || r.Header.Get("Range") != "0-399" {
		t.Fatalf("PATCH 1 = %d %v", r.StatusCode, r.Header)
	}
	// Out-of-order chunk.
	if r = do(t, "PATCH", loc, bytes.NewReader(data[500:]), map[string]string{"Content-Range": "500-999"}); r.StatusCode != 416 {
		t.Fatalf("bad range PATCH = %d", r.StatusCode)
	}
	r = do(t, "PATCH", loc, bytes.NewReader(data[400:900]), map[string]string{"Content-Range": "400-899"})
	if r.StatusCode != 202 {
		t.Fatalf("PATCH 2 = %d", r.StatusCode)
	}
	if s := do(t, "GET", loc, nil, nil); s.StatusCode != 204 || s.Header.Get("Range") != "0-899" {
		t.Fatalf("status = %d %v", s.StatusCode, s.Header)
	}
	d := sha(data)
	// Wrong digest is rejected.
	bad := do(t, "PUT", loc+"?digest="+sha([]byte("x")), bytes.NewReader(data[900:]), nil)
	if bad.StatusCode != 400 || !strings.Contains(readAll(bad), "DIGEST_INVALID") {
		t.Fatalf("bad digest PUT = %d", bad.StatusCode)
	}
	// Restart the upload properly.
	r = do(t, "POST", base+"/blobs/uploads/", nil, nil)
	loc = "http://" + e.registry() + r.Header.Get("Location")
	do(t, "PATCH", loc, bytes.NewReader(data[:600]), nil)
	r = do(t, "PUT", loc+"?digest="+d, bytes.NewReader(data[600:]), nil)
	if r.StatusCode != 201 || r.Header.Get("Docker-Content-Digest") != d {
		t.Fatalf("PUT = %d %v", r.StatusCode, r.Header)
	}
	r = do(t, "GET", base+"/blobs/"+d, nil, map[string]string{"Range": "bytes=10-19"})
	if r.StatusCode != 206 || readAll(r) != "0123456789" {
		t.Fatalf("range GET = %d", r.StatusCode)
	}
	if r = do(t, "HEAD", base+"/blobs/"+d, nil, nil); r.StatusCode != 200 || r.ContentLength != 1000 {
		t.Fatalf("HEAD = %d len %d", r.StatusCode, r.ContentLength)
	}
	// Blobs are per OCI repository.
	if r = do(t, "HEAD", "http://"+e.registry()+"/v2/"+testProject+"/images/other/blobs/"+d, nil, nil); r.StatusCode != 404 {
		t.Fatalf("other repo HEAD = %d", r.StatusCode)
	}
	// Cross-repo mount.
	r = do(t, "POST", "http://"+e.registry()+"/v2/"+testProject+"/images/other/blobs/uploads/?mount="+d+"&from="+testProject+"/images/raw", nil, nil)
	if r.StatusCode != 201 {
		t.Fatalf("mount = %d", r.StatusCode)
	}
	if r = do(t, "DELETE", base+"/blobs/"+d, nil, nil); r.StatusCode != 202 {
		t.Fatalf("DELETE = %d", r.StatusCode)
	}
	if r = do(t, "GET", base+"/blobs/"+d, nil, nil); r.StatusCode != 404 {
		t.Fatalf("GET after delete = %d", r.StatusCode)
	}
	// Still served from the mounted repository.
	if r = do(t, "GET", "http://"+e.registry()+"/v2/"+testProject+"/images/other/blobs/"+d, nil, nil); r.StatusCode != 200 || readAll(r) != string(data) {
		t.Fatalf("mounted GET = %d", r.StatusCode)
	}
}

func readAll(r *http.Response) string {
	b, _ := io.ReadAll(r.Body)
	return string(b)
}

func TestRegistryManifestRulesAndTags(t *testing.T) {
	e := start(t)
	e.createRepo(testLoc, "images")
	base := "http://" + e.registry() + "/v2/" + testProject + "/images/app"
	// Manifest referencing unknown blobs.
	m := fmt.Sprintf(`{"schemaVersion":2,"mediaType":%q,"config":{"mediaType":%q,"digest":%q,"size":2},"layers":[]}`,
		types.DockerManifestSchema2, types.DockerConfigJSON, sha([]byte("{}")))
	r := do(t, "PUT", base+"/manifests/v1", strings.NewReader(m), map[string]string{"Content-Type": string(types.DockerManifestSchema2)})
	if r.StatusCode != 400 || !strings.Contains(readAll(r), "MANIFEST_BLOB_UNKNOWN") {
		t.Fatalf("PUT unknown blob = %d", r.StatusCode)
	}
	do(t, "POST", base+"/blobs/uploads/?digest="+sha([]byte("{}")), strings.NewReader("{}"), nil)
	if r = do(t, "PUT", base+"/manifests/v1", strings.NewReader(m), map[string]string{"Content-Type": string(types.DockerManifestSchema2)}); r.StatusCode != 201 {
		t.Fatalf("PUT = %d %s", r.StatusCode, readAll(r))
	}
	dg := sha([]byte(m))
	if r.Header.Get("Docker-Content-Digest") != dg {
		t.Fatalf("digest header %q", r.Header.Get("Docker-Content-Digest"))
	}
	// Digest mismatch.
	if r = do(t, "PUT", base+"/manifests/"+sha([]byte("x")), strings.NewReader(m), map[string]string{"Content-Type": string(types.DockerManifestSchema2)}); r.StatusCode != 400 {
		t.Fatalf("PUT mismatch = %d", r.StatusCode)
	}
	// Content negotiation.
	if r = do(t, "GET", base+"/manifests/v1", nil, map[string]string{"Accept": string(types.OCIManifestSchema1)}); r.StatusCode != 404 {
		t.Fatalf("GET with OCI-only accept = %d", r.StatusCode)
	}
	r = do(t, "HEAD", base+"/manifests/v1", nil, map[string]string{"Accept": string(types.DockerManifestSchema2) + ", " + string(types.OCIManifestSchema1)})
	if r.StatusCode != 200 || r.Header.Get("Content-Type") != string(types.DockerManifestSchema2) || r.Header.Get("Docker-Content-Digest") != dg {
		t.Fatalf("HEAD = %d %v", r.StatusCode, r.Header)
	}
	// Tags pagination.
	for _, tg := range []string{"a", "b", "c", "d"} {
		do(t, "PUT", base+"/manifests/"+tg, strings.NewReader(m), map[string]string{"Content-Type": string(types.DockerManifestSchema2)})
	}
	r = do(t, "GET", base+"/tags/list?n=2", nil, nil)
	var tl struct {
		Name string   `json:"name"`
		Tags []string `json:"tags"`
	}
	_ = json.NewDecoder(r.Body).Decode(&tl)
	if tl.Name != testProject+"/images/app" || strings.Join(tl.Tags, ",") != "a,b" || !strings.Contains(r.Header.Get("Link"), "last=b") {
		t.Fatalf("tags page 1 = %+v link %q", tl, r.Header.Get("Link"))
	}
	r = do(t, "GET", base+"/tags/list?n=10&last=b", nil, nil)
	_ = json.NewDecoder(r.Body).Decode(&tl)
	if strings.Join(tl.Tags, ",") != "c,d,v1" || r.Header.Get("Link") != "" {
		t.Fatalf("tags page 2 = %+v", tl)
	}
	all, err := remote.List(e.ref(testProject+"/images/app").Context(), remote.WithPageSize(2))
	sort.Strings(all)
	if err != nil || strings.Join(all, ",") != "a,b,c,d,v1" {
		t.Fatalf("remote.List = %v %v", all, err)
	}
	// Delete a tag, then the manifest by digest (removes remaining tags).
	if r = do(t, "DELETE", base+"/manifests/a", nil, nil); r.StatusCode != 202 {
		t.Fatalf("DELETE tag = %d", r.StatusCode)
	}
	if r = do(t, "GET", base+"/manifests/a", nil, nil); r.StatusCode != 404 {
		t.Fatalf("GET deleted tag = %d", r.StatusCode)
	}
	if r = do(t, "DELETE", base+"/manifests/"+dg, nil, nil); r.StatusCode != 202 {
		t.Fatalf("DELETE digest = %d", r.StatusCode)
	}
	if r = do(t, "GET", base+"/manifests/v1", nil, nil); r.StatusCode != 404 {
		t.Fatalf("GET after digest delete = %d", r.StatusCode)
	}
	// Unknown repository.
	if r = do(t, "GET", "http://"+e.registry()+"/v2/"+testProject+"/nope/app/tags/list", nil, nil); r.StatusCode != 404 || !strings.Contains(readAll(r), "NAME_UNKNOWN") {
		t.Fatalf("unknown repo = %d", r.StatusCode)
	}
}

func TestRegistryAmbiguousAndHostMode(t *testing.T) {
	e := start(t)
	e.createRepo("us-central1", "dup")
	e.createRepo("europe-west1", "dup")
	r := do(t, "GET", "http://"+e.registry()+"/v2/"+testProject+"/dup/app/tags/list", nil, nil)
	body := readAll(r)
	if r.StatusCode != 400 || !strings.Contains(body, "europe-west1, us-central1") {
		t.Fatalf("ambiguous = %d %s", r.StatusCode, body)
	}
	img, _ := random.Image(128, 1)
	if err := remote.Write(e.ref("europe-west1-docker.pkg.dev/"+testProject+"/dup/app:v1"), img); err != nil {
		t.Fatal(err)
	}
	// Host mode: the Host header names the location.
	req, _ := http.NewRequest("GET", "http://"+e.registry()+"/v2/"+testProject+"/dup/app/tags/list", nil)
	req.Host = "europe-west1-docker.pkg.dev"
	req.Header.Set("Authorization", "Bearer x")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(b), `"v1"`) {
		t.Fatalf("host mode = %d %s", resp.StatusCode, b)
	}
}

func TestRegistryAuth(t *testing.T) {
	e := start(t)
	e.createRepo(testLoc, "images")
	// Ping without credentials is challenged.
	resp, err := http.Get("http://" + e.registry() + "/v2/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 || !strings.Contains(resp.Header.Get("WWW-Authenticate"), `Bearer realm="http://localhost:`+e.registry()[strings.LastIndex(e.registry(), ":")+1:]+`/v2/token"`) {
		t.Fatalf("ping = %d %q", resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
	}
	// Basic oauth2accesstoken works with docker-style clients (audit mode
	// accepts tokens it cannot resolve).
	img, _ := random.Image(128, 1)
	ref := e.ref(testProject + "/images/app:v1")
	auth := remote.WithAuth(&authn.Basic{Username: "oauth2accesstoken", Password: "some-token"})
	if err := remote.Write(ref, img, auth); err != nil {
		t.Fatalf("push with basic auth: %v", err)
	}
	req, _ := http.NewRequest("GET", "http://"+e.registry()+"/v2/token?scope=repository:x:pull", nil)
	req.SetBasicAuth("oauth2accesstoken", "tok-123")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var tok struct {
		Token string `json:"token"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&tok)
	resp.Body.Close()
	if resp.StatusCode != 200 || tok.Token == "" {
		t.Fatalf("token = %d %+v", resp.StatusCode, tok)
	}
}

func TestRegistryEnforceRequiresAuth(t *testing.T) {
	e := start(t, emutest.WithIAMMode(config.IAMEnforce))
	e.createRepo(testLoc, "images")
	resp, err := http.Get("http://" + e.registry() + "/v2/" + testProject + "/images/app/tags/list")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 || !strings.Contains(resp.Header.Get("WWW-Authenticate"), `scope="repository:`+testProject+`/images/app:pull"`) {
		t.Fatalf("anonymous in enforce = %d %q", resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
	}
	resp, err = http.Get("http://" + e.registry() + "/v2/token")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("anonymous token in enforce = %d", resp.StatusCode)
	}
	_, err = remote.Image(e.ref(testProject + "/images/app:v1"))
	if err == nil {
		t.Fatal("anonymous pull succeeded in enforce mode")
	}
}
