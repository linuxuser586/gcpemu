package emutest_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/storage"
	"github.com/golang-jwt/jwt/v5"
	dns "google.golang.org/api/dns/v1"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"

	"github.com/linuxuser586/gcpemu/emutest"
	"github.com/linuxuser586/gcpemu/internal/project"
)

// TestWithSeed: the Seed file is applied before the Instance is ready; its
// project is declared, so strict projects accept it.
func TestWithSeed(t *testing.T) {
	seed := filepath.Join(t.TempDir(), "seed.yaml")
	if err := os.WriteFile(seed, []byte(`projects: [seed-proj]
gcs:
  buckets:
    - name: seed-bucket
      project: seed-proj
`), 0o600); err != nil {
		t.Fatal(err)
	}
	inst := emutest.Start(t, []string{"gcs"}, emutest.WithSeed(seed), emutest.WithStrictProjects())
	sc := inst.Storage(t)
	ctx := context.Background()
	if _, err := sc.Bucket("seed-bucket").Attrs(ctx); err != nil {
		t.Errorf("seeded bucket: %v", err)
	}
	if err := sc.Bucket("seed-bucket-2").Create(ctx, "seed-proj", nil); err != nil {
		t.Errorf("bucket in the seeded project: %v", err)
	}
}

// TestWithStrictProjects: a project nobody declared is refused rather than
// created on first use.
func TestWithStrictProjects(t *testing.T) {
	inst := emutest.Start(t, []string{"gcs"}, emutest.WithStrictProjects())
	err := inst.Storage(t).Bucket("strict-bucket").Create(context.Background(), "undeclared-proj", nil)
	var gerr *googleapi.Error
	if !errors.As(err, &gerr) || gerr.Code != http.StatusNotFound {
		t.Fatalf("bucket in an undeclared project: %v, want 404", err)
	}
}

// TestWithDataDir: a Persistent Instance keeps its state in dir, which a
// later Instance in dir finds, and Close leaves dir in place. It is the one
// test that hands a Test data dir to WithDataDir rather than
// WithTestDataDir, to test that option; its Instances use no runtime, so
// nothing outlives the test.
func TestWithDataDir(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	first, err := emutest.New(ctx, []string{"gcs"}, emutest.WithDataDir(dir))
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Storage(t).Bucket("kept-bucket").Create(ctx, "persist-proj", nil); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "state.db")); err != nil {
		t.Fatalf("state in the data dir after Close: %v", err)
	}
	second := emutest.Start(t, []string{"gcs"}, emutest.WithDataDir(dir))
	if _, err := second.Storage(t).Bucket("kept-bucket").Attrs(ctx); err != nil {
		t.Errorf("bucket after restart: %v", err)
	}
}

// TestWithTestDataDir: Instances started in the same dir in one test,
// through one Option or several, register one end-of-test cleanup of the
// Instance's runtime objects between them, which reports nothing when
// there are none.
func TestWithTestDataDir(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	rec := &cleanupTB{TB: t}
	persistent := emutest.WithTestDataDir(rec, dir)
	for _, opt := range []emutest.Option{persistent, emutest.WithTestDataDir(rec, dir), persistent} {
		if err := emutest.Start(rec, []string{"gcs"}, opt).Close(ctx); err != nil {
			t.Fatal(err)
		}
	}
	// One cleanup per Start, and one for dir.
	if n := len(rec.cleanups); n != 4 {
		t.Errorf("%d cleanups registered, want 4", n)
	}
	rec.runCleanups()
	if len(rec.errors) != 0 {
		t.Errorf("cleanups reported %q", rec.errors)
	}
	if _, err := os.Stat(filepath.Join(dir, "state.db")); err != nil {
		t.Errorf("state in the data dir after the cleanups: %v", err)
	}
}

// TestWithOperationLatency: a DNS change stays pending for the configured
// latency, then completes.
func TestWithOperationLatency(t *testing.T) {
	const latency = 300 * time.Millisecond
	inst := emutest.Start(t, []string{"dns"}, emutest.WithOperationLatency("dns", latency))
	opts, err := inst.HTTPOptions("dns")
	if err != nil {
		t.Fatal(err)
	}
	c, err := dns.NewService(context.Background(), opts...)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.ManagedZones.Create("lat-proj", &dns.ManagedZone{Name: "z", DnsName: "z.test.", Description: "d"}).Do(); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	ch, err := c.Changes.Create("lat-proj", "z", &dns.Change{Additions: []*dns.ResourceRecordSet{
		{Name: "a.z.test.", Type: "A", Ttl: 60, Rrdatas: []string{"192.0.2.1"}},
	}}).Do()
	if err != nil || ch.Status != "pending" {
		t.Fatalf("change = %+v, %v; want pending", ch, err)
	}
	for ch.Status != "done" {
		if time.Since(start) > 10*time.Second {
			t.Fatalf("change still %s after 10s", ch.Status)
		}
		time.Sleep(20 * time.Millisecond)
		if ch, err = c.Changes.Get("lat-proj", "z", ch.Id).Do(); err != nil {
			t.Fatal(err)
		}
	}
	if d := time.Since(start); d < latency {
		t.Errorf("change done after %v, want at least %v", d, latency)
	}
}

// TestWithOffline: STS refuses to fetch a workload identity provider's
// issuer keys rather than reaching the internet.
func TestWithOffline(t *testing.T) {
	inst := emutest.Start(t, []string{"iam"}, emutest.WithOffline())
	gw := inst.GatewayURL()
	const proj, issuer = "offline-proj", "https://issuer.example.test"
	pools := gw + "/iam/v1/projects/" + proj + "/locations/global/workloadIdentityPools"
	ok(t, "POST", pools+"?workloadIdentityPoolId=pool", map[string]any{}, nil)
	ok(t, "POST", pools+"/pool/providers?workloadIdentityPoolProviderId=oidc",
		map[string]any{"oidc": map[string]any{"issuerUri": issuer}, "attributeMapping": map[string]string{"google.subject": "assertion.sub"}}, nil)
	provider := "//iam.googleapis.com/projects/" + project.NumberString(proj) + "/locations/global/workloadIdentityPools/pool/providers/oidc"
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{"iss": issuer, "aud": "https:" + provider,
		"sub": "u", "iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix()})
	tok.Header["kid"] = "k"
	subject, err := tok.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	code, b := send(t, "POST", gw+"/sts/v1/token", map[string]any{
		"grant_type": "urn:ietf:params:oauth:grant-type:token-exchange", "audience": provider,
		"subject_token": subject, "subject_token_type": "urn:ietf:params:oauth:token-type:jwt",
		"requested_token_type": "urn:ietf:params:oauth:token-type:access_token", "scope": "https://www.googleapis.com/auth/cloud-platform",
	}, nil)
	if code/100 == 2 || !strings.Contains(string(b), "offline mode") {
		t.Errorf("STS with an issuer to fetch keys from: %d %s", code, b)
	}
}

// TestNewClose: an Instance from New serves until Close, which removes
// its temporary data dir.
func TestNewClose(t *testing.T) {
	ctx := context.Background()
	inst, err := emutest.New(ctx, []string{"gcs"})
	if err != nil {
		t.Fatal(err)
	}
	dir := inst.Internal().Dir
	sc, err := storage.NewClient(ctx, append(httpOptions(t, inst, "storage"), storage.WithJSONReads())...)
	if err != nil {
		t.Fatal(err)
	}
	defer sc.Close()
	if err := sc.Bucket("new-bucket").Create(ctx, "new-proj", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("data dir while running: %v", err)
	}
	if err := inst.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("data dir %s after Close: %v, want it gone", dir, err)
	}
	if err := inst.Close(ctx); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

// httpOptions is inst.HTTPOptions(api), failing the test on error.
func httpOptions(t *testing.T, inst *emutest.Instance, api string) []option.ClientOption {
	t.Helper()
	opts, err := inst.HTTPOptions(api)
	if err != nil {
		t.Fatal(err)
	}
	return opts
}

// TestWithExternal: the Instance forwards the Delegated API to the
// External emulator.
func TestWithExternal(t *testing.T) {
	ext := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "external "+r.URL.Path)
	}))
	defer ext.Close()
	inst := emutest.Start(t, []string{"iam"}, emutest.WithExternal("firestore", ext.Listener.Addr().String()))
	resp, err := http.Get(inst.GatewayURL() + "/firestore/v1/projects/p/databases/(default)/documents/c/d")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if want := "external /v1/projects/p/databases/(default)/documents/c/d"; string(b) != want {
		t.Errorf("got %d %q, want %q", resp.StatusCode, b, want)
	}
}

// cleanupTB records the cleanups and errors of what it is passed to, and
// sends everything else to the test.
type cleanupTB struct {
	testing.TB
	cleanups []func()
	errors   []string
}

func (c *cleanupTB) Cleanup(f func()) { c.cleanups = append(c.cleanups, f) }
func (c *cleanupTB) Errorf(format string, a ...any) {
	c.errors = append(c.errors, fmt.Sprintf(format, a...))
}

func (c *cleanupTB) runCleanups() {
	for i := len(c.cleanups) - 1; i >= 0; i-- {
		c.cleanups[i]()
	}
}
