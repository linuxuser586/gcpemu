package instance

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"cloud.google.com/go/storage"
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

// A key for a service account in the Delegated Project, created through
// the laptop under projects/-, gets a token from the laptop's token
// endpoint, and the laptop accepts that token for the Delegated Project's
// resources, asserting the service account to the office.
func TestDelegatedServiceAccountToken(t *testing.T) {
	laptop, office := delegated(t, func(c *config.Config) { c.IAMMode = config.IAMEnforce }, nil)
	callAdmin(t, laptop, http.MethodPost, "/iam/v1/projects/"+shared+"/serviceAccounts", map[string]string{"accountId": "runner"}, nil)
	var key struct{ PrivateKeyData string }
	callAdmin(t, laptop, http.MethodPost, "/iam/v1/projects/-/serviceAccounts/"+runner+"/keys", map[string]string{}, &key)
	file, err := base64.StdEncoding.DecodeString(key.PrivateKeyData)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := google.JWTConfigFromJSON(file, "https://www.googleapis.com/auth/cloud-platform")
	if err != nil {
		t.Fatal(err)
	}
	cfg.TokenURL = "http://" + laptop.Env.Endpoints.Get("gateway") + "/oauth2/token"
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
