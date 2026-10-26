package instance

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"cloud.google.com/go/storage"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	crmv1 "google.golang.org/api/cloudresourcemanager/v1"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"

	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/project"
)

// Requests that name no Project are routed across the link too (ADR
// 0027): a bucket by who owns it, a service account by the Project in its
// email, and projects/- and aggregated lists by asking every Instance.

const runner = "runner@" + shared + ".iam.gserviceaccount.com"

// bearerGet gets path from in's API gateway with the access token tok.
func bearerGet(t *testing.T, in *Instance, path, tok string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, "http://"+in.Env.Endpoints.Get("gateway")+path, nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// keyFileThrough creates a key for email through in's API gateway, with
// the request's headers set from header, and returns the key's ID and the
// decoded key file.
func keyFileThrough(t *testing.T, in *Instance, email string, header http.Header) (string, []byte) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, "http://"+in.Env.Endpoints.Get("gateway")+"/iam/v1/projects/-/serviceAccounts/"+email+"/keys", strings.NewReader("{}"))
	for k, v := range header {
		req.Header[k] = v
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var key struct{ Name, PrivateKeyData string }
	if b, _ := io.ReadAll(resp.Body); resp.StatusCode != http.StatusOK || json.Unmarshal(b, &key) != nil {
		t.Fatalf("creating a key for %s: %d %s", email, resp.StatusCode, b)
	}
	file, err := base64.StdEncoding.DecodeString(key.PrivateKeyData)
	if err != nil {
		t.Fatal(err)
	}
	return key.Name[strings.LastIndex(key.Name, "/")+1:], file
}

// keyFileURLs are the URLs of a key file that name an Instance.
type keyFileURLs struct {
	TokenURI                string `json:"token_uri"`
	AuthProviderX509CertURL string `json:"auth_provider_x509_cert_url"`
	ClientX509CertURL       string `json:"client_x509_cert_url"`
}

// urlsOf returns the URLs of the key file.
func urlsOf(t *testing.T, file []byte) keyFileURLs {
	t.Helper()
	var u keyFileURLs
	if err := json.Unmarshal(file, &u); err != nil {
		t.Fatalf("key file %s: %v", file, err)
	}
	return u
}

// urlsUnder returns the key file URLs of a key for email under base.
func urlsUnder(base, email string) keyFileURLs {
	return keyFileURLs{
		TokenURI:                base + "/oauth2/token",
		AuthProviderX509CertURL: base + "/oauth2/v1/certs",
		ClientX509CertURL:       base + "/service_accounts/v1/metadata/x509/" + email,
	}
}

// A key for a service account in the Delegated Project, created through
// the laptop under projects/-, names the laptop's token and certificate
// URLs (ADR 0027), in the scheme the caller used, and the laptop serves
// its certificate from the office. A key created on the office names the
// office's URLs.
func TestDelegatedKeyFileNamesFrontInstance(t *testing.T) {
	laptop, office := delegated(t, nil, nil)
	callAdmin(t, laptop, http.MethodPost, "/iam/v1/projects/"+shared+"/serviceAccounts", map[string]string{"accountId": "runner"}, nil)
	keyID, file := keyFileThrough(t, laptop, runner, nil)
	gw := laptop.Env.Endpoints.Get("gateway")
	urls := urlsOf(t, file)
	if want := urlsUnder("http://"+gw, runner); urls != want {
		t.Errorf("key file through the laptop names %+v, want %+v", urls, want)
	}
	for proto, scheme := range map[string]string{"https": "https", "http": "http", "https, http": "http", "ftp": "http"} {
		_, f := keyFileThrough(t, laptop, runner, http.Header{"X-Forwarded-Proto": {proto}})
		if got, want := urlsOf(t, f), urlsUnder(scheme+"://"+gw, runner); got != want {
			t.Errorf("key file through the laptop with X-Forwarded-Proto %q names %+v, want %+v", proto, got, want)
		}
	}

	certs, _ := url.Parse(urls.ClientX509CertURL)
	if code, body := restGet(t, laptop, http.MethodGet, certs.EscapedPath()); code != http.StatusOK || !strings.Contains(body, `"`+keyID+`"`) {
		t.Errorf("the key's certificates through the laptop = %d %s, want key %s", code, body, keyID)
	}
	_, direct := keyFileThrough(t, office, runner, nil)
	if got, want := urlsOf(t, direct), urlsUnder("http://"+office.Env.Endpoints.Get("gateway"), runner); got != want {
		t.Errorf("key file created on the office names %+v, want %+v", got, want)
	}
}

// A key for a service account in the Delegated Project, created through
// the laptop under projects/-, gets a token from the laptop's token
// endpoint unmodified, and the laptop accepts that token for the Delegated
// Project's resources, asserting the service account to the office.
func TestDelegatedServiceAccountToken(t *testing.T) {
	laptop, office := delegated(t, func(c *config.Config) { c.IAMMode = config.IAMEnforce }, nil)
	callAdmin(t, laptop, http.MethodPost, "/iam/v1/projects/"+shared+"/serviceAccounts", map[string]string{"accountId": "runner"}, nil)
	_, file := keyFileThrough(t, laptop, runner, nil)
	cfg, err := google.JWTConfigFromJSON(file, "https://www.googleapis.com/auth/cloud-platform")
	if err != nil {
		t.Fatal(err)
	}
	tok, err := cfg.TokenSource(context.Background()).Token()
	if err != nil {
		t.Fatalf("token from the laptop's token endpoint: %v", err)
	}

	if code, body := bearerGet(t, laptop, "/pubsub/v1/projects/"+shared+"/topics", tok.AccessToken); code != http.StatusOK {
		t.Fatalf("listing topics through the laptop with the token = %d %s", code, body)
	}
	asserted := false
	for _, e := range office.Log.Entries("pubsub") {
		asserted = asserted || e.Principal == "serviceAccount:"+runner
	}
	if !asserted {
		t.Error("the office did not serve the request as the service account")
	}
	if code, body := bearerGet(t, laptop, "/pubsub/v1/projects/"+shared+"/topics", "ya29.gcpemu.not-a-token"); code != http.StatusUnauthorized {
		t.Errorf("listing topics through the laptop with a token no Instance issued = %d %s, want 401", code, body)
	}
}

// A key created through the laptop for a service account in the Delegated
// Project, exchanged at the laptop's token endpoint once the Project is no
// longer delegated, is refused, 400 invalid_grant, saying the Project is
// neither on the laptop nor delegated by it (#695).
func TestUndelegatedServiceAccountToken(t *testing.T) {
	laptop, _ := delegated(t, nil, nil)
	callAdmin(t, laptop, http.MethodPost, "/iam/v1/projects/"+shared+"/serviceAccounts", map[string]string{"accountId": "runner"}, nil)
	_, file := keyFileThrough(t, laptop, runner, nil)
	cfg, err := google.JWTConfigFromJSON(file, "https://www.googleapis.com/auth/cloud-platform")
	if err != nil {
		t.Fatal(err)
	}
	callAdmin(t, laptop, http.MethodDelete, "/_emu/v1/links/delegations/"+shared, nil, nil)

	_, err = cfg.TokenSource(context.Background()).Token()
	var rerr *oauth2.RetrieveError
	var body struct {
		Error            string
		ErrorDescription string `json:"error_description"`
	}
	if !errors.As(err, &rerr) || rerr.Response.StatusCode != http.StatusBadRequest || json.Unmarshal(rerr.Body, &body) != nil ||
		body.Error != "invalid_grant" || !strings.Contains(body.ErrorDescription, `Project "`+shared+`" is not on this Instance and is not delegated by it`) {
		t.Errorf("token from the laptop after undelegating: %v, want 400 invalid_grant saying %s is neither on the laptop nor delegated by it", err, shared)
	}
}

// storageClient is a Go SDK Cloud Storage client of in's API gateway.
func storageClient(t *testing.T, in *Instance) *storage.Client {
	t.Helper()
	c, err := storage.NewClient(context.Background(), option.WithEndpoint("http://"+in.Env.Endpoints.Get("gateway")+"/storage/v1/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// waitBucket waits for c to see bucket, which a Linked Instance may tell
// the laptop of only after a moment, and returns it.
func waitBucket(t *testing.T, c *storage.Client, bucket string) *storage.BucketAttrs {
	t.Helper()
	for i := 0; ; i++ {
		attrs, err := c.Bucket(bucket).Attrs(context.Background())
		if err == nil {
			return attrs
		}
		if i == 100 {
			t.Fatalf("bucket %s: %v", bucket, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// A Storage client of the laptop reads and writes objects, a resumable
// upload included, in buckets the Delegated Project owns: one the office
// created itself, which the laptop learns of from the office, and one
// created through the laptop. The objects are on the office only.
func TestDelegatedBucketObjects(t *testing.T) {
	laptop, office := delegated(t, nil, nil)
	ctx := context.Background()
	lc, oc := storageClient(t, laptop), storageClient(t, office)
	if err := oc.Bucket("office-made").Create(ctx, shared, nil); err != nil {
		t.Fatal(err)
	}
	if err := lc.Bucket("laptop-made").Create(ctx, shared, nil); err != nil {
		t.Fatalf("creating a bucket in the Delegated Project through the laptop: %v", err)
	}
	big := bytes.Repeat([]byte("0123456789abcdef"), 64<<10) // 1 MiB, past the chunk size
	for _, bucket := range []string{"office-made", "laptop-made"} {
		attrs := waitBucket(t, lc, bucket)
		if attrs.ProjectNumber != uint64(project.Number(shared)) {
			t.Errorf("bucket %s through the laptop is in Project number %d", bucket, attrs.ProjectNumber)
		}
		w := lc.Bucket(bucket).Object("small").NewWriter(ctx)
		w.ChunkSize = 0
		_, _ = w.Write([]byte("hello"))
		if err := w.Close(); err != nil {
			t.Fatalf("writing to %s through the laptop: %v", bucket, err)
		}
		w = lc.Bucket(bucket).Object("big").NewWriter(ctx)
		w.ChunkSize = 256 << 10
		_, _ = w.Write(big)
		if err := w.Close(); err != nil {
			t.Fatalf("resumable upload to %s through the laptop: %v", bucket, err)
		}
		for name, want := range map[string][]byte{"small": []byte("hello"), "big": big} {
			for who, c := range map[string]*storage.Client{"laptop": lc, "office": oc} {
				r, err := c.Bucket(bucket).Object(name).NewReader(ctx)
				if err != nil {
					t.Fatalf("reading %s/%s through the %s: %v", bucket, name, who, err)
				}
				got, _ := io.ReadAll(r)
				_ = r.Close()
				if !bytes.Equal(got, want) {
					t.Errorf("%s/%s through the %s has %d bytes, want %d", bucket, name, who, len(got), len(want))
				}
			}
		}
	}
	if slices.Contains(laptop.Env.Projects(), shared) {
		t.Error("the laptop created the Delegated Project locally")
	}
}

// Bucket names are global across a link: the laptop refuses, 409, a local
// bucket whose name the office holds, and a bucket in the Delegated
// Project whose name it holds itself.
func TestDelegatedBucketNamesAreGlobal(t *testing.T) {
	laptop, office := delegated(t, nil, nil)
	ctx := context.Background()
	lc := storageClient(t, laptop)
	if err := storageClient(t, office).Bucket("taken-on-office").Create(ctx, shared, nil); err != nil {
		t.Fatal(err)
	}
	if err := lc.Bucket("taken-on-laptop").Create(ctx, "laptop-own", nil); err != nil {
		t.Fatal(err)
	}
	waitBucket(t, lc, "taken-on-office")
	conflict := func(bucket, in string) {
		t.Helper()
		var gerr *googleapi.Error
		if err := lc.Bucket(bucket).Create(ctx, in, nil); !errors.As(err, &gerr) || gerr.Code != http.StatusConflict {
			t.Errorf("creating %s in %s through the laptop = %v, want 409", bucket, in, err)
		}
	}
	conflict("taken-on-office", "laptop-own")
	conflict("taken-on-laptop", shared)
}

// Listing Projects through the laptop, which names none, lists the
// laptop's own and the office's Delegated Project, but not a Project the
// office does not share, paging through them as one list.
func TestDelegatedProjectsListed(t *testing.T) {
	laptop, office := delegated(t, nil, nil)
	ctx := context.Background()
	if _, err := pubsubClient(t, office).TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{Name: "projects/" + shared + "/topics/topic-a"}); err != nil {
		t.Fatal(err)
	}
	if err := storageClient(t, office).Bucket("office-private-bucket").Create(ctx, "office-private", nil); err != nil {
		t.Fatal(err)
	}
	if err := storageClient(t, laptop).Bucket("laptop-own-bucket").Create(ctx, "laptop-own", nil); err != nil {
		t.Fatal(err)
	}
	crm, err := crmv1.NewService(ctx, option.WithEndpoint("http://"+laptop.Env.Endpoints.Get("gateway")+"/cloudresourcemanager/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	pages := 0
	err = crm.Projects.List().Pages(ctx, func(r *crmv1.ListProjectsResponse) error {
		pages++
		for _, p := range r.Projects {
			got = append(got, p.ProjectId)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("listing Projects through the laptop: %v", err)
	}
	if !slices.Contains(got, "laptop-own") || !slices.Contains(got, shared) || slices.Contains(got, "office-private") {
		t.Errorf("Projects through the laptop = %v, want laptop-own and %s, not office-private", got, shared)
	}
	if pages < 2 {
		t.Errorf("Projects through the laptop came in %d page, want one per Instance", pages)
	}
	if code, body := restGet(t, laptop, http.MethodGet, "/cloudresourcemanager/v3/projects:search"); code != http.StatusOK || !strings.Contains(body, `"laptop-own"`) {
		t.Errorf("searching Projects through the laptop = %d %s", code, body)
	}
}

// Delegating a Project fetches the office's buckets before it returns, so
// a bucket the office already has is routed at once.
func TestDelegatingFetchesBuckets(t *testing.T) {
	laptop, office := startWithProjects(t, "laptop", nil), startWithProjects(t, "office", nil)
	pairInstances(t, laptop, office)
	callAdmin(t, office, http.MethodPost, "/_emu/v1/links/shares", map[string]string{"project": shared}, nil)
	if err := storageClient(t, office).Bucket("office-made").Create(context.Background(), shared, nil); err != nil {
		t.Fatal(err)
	}
	callAdmin(t, laptop, http.MethodPost, "/_emu/v1/links/delegations", map[string]string{"project": shared, "link": "office"}, nil)
	if _, err := storageClient(t, laptop).Bucket("office-made").Attrs(context.Background()); err != nil {
		t.Errorf("the office's bucket through the laptop right after delegating: %v", err)
	}
}
