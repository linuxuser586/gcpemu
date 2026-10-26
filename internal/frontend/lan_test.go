package frontend_test

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/storage"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	iamv1 "google.golang.org/api/iam/v1"
	"google.golang.org/api/option"

	"github.com/linuxuser586/gcpemu/emutest"
	"github.com/linuxuser586/gcpemu/internal/ca"
	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/frontend"
)

// tokenAuth accepts one bearer token.
type tokenAuth struct{ emu.Authorizer }

func (tokenAuth) Authenticate(_ context.Context, tok string) (emu.Principal, error) {
	if tok != "good" {
		return "", emu.ErrUnknownToken
	}
	return "serviceAccount:ci-runner@p.iam.gserviceaccount.com", nil
}

// stubStorage owns the bucket "ours" and treats XML API requests with a
// signature query as signed.
type stubStorage struct{}

func (stubStorage) ForeignBucket(r *http.Request) bool {
	return !strings.HasPrefix(r.URL.Path, "/ours") && !strings.HasPrefix(r.URL.Path, "/storage/v1/b/ours")
}

func (stubStorage) Signed(r *http.Request) bool { return r.URL.Query().Get("X-Goog-Signature") != "" }

// answer is a stub backend naming itself, the Host it saw and the body,
// and the client address it saw in X-Remote-Addr.
func answer(name string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		w.Header().Set("X-Remote-Addr", r.RemoteAddr)
		io.WriteString(w, name+" "+r.Host+" "+string(b))
	})
}

// lanNames are the hostnames the LAN tests' front end serves.
func lanNames() []string {
	return []string{"iam.googleapis.com", "oauth2.googleapis.com", "storage.googleapis.com", "www.googleapis.com"}
}

// lanStubs are the stub backends of the LAN tests.
func lanStubs() frontend.LAN {
	return frontend.LAN{
		Gateway:     answer("gateway"),
		Registry:    func() http.Handler { return answer("registry") },
		Storage:     stubStorage{},
		ReadThrough: answer("google"),
	}
}

// TestLAN covers #462: on a --gfe-bind address the Google front end serves
// the Instance's real hostnames over TLS, but only to callers with
// credentials; anonymous reads of buckets the Instance lacks go to real
// GCS, and nothing but the Google APIs is reachable (ADR 0021).
func TestLAN(t *testing.T) {
	authority, err := ca.Load(t.TempDir(), "lan")
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	cfg.GFEBind = []string{"127.0.0.1"}
	cfg.Ports["gfe-lan"] = 0
	env := &emu.Env{Config: &cfg, CA: authority, Auth: tokenAuth{}, Endpoints: emu.NewEndpoints(), Log: slog.New(slog.DiscardHandler)}
	fe := frontend.New(env, lanNames)
	fe.StartLAN(lanStubs())
	t.Cleanup(func() { fe.Close() })
	addr := env.Endpoints.Get("gfe-lan")
	if !strings.HasPrefix(addr, "127.0.0.1:") {
		t.Fatalf("gfe-lan endpoint = %q", addr)
	}
	checkLAN(t, &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		DialContext:       dialFrontend(addr),
		TLSClientConfig:   &tls.Config{RootCAs: authority.Pool()},
		ForceAttemptHTTP2: true,
	}})
}

// checkLAN runs the requests a --gfe-bind listener admits and refuses
// through hc, whose transport reaches one.
func checkLAN(t *testing.T, hc *http.Client) {
	t.Helper()
	jwtGrant := url.Values{"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"}, "assertion": {"a.b.c"}}.Encode()
	for _, tc := range []struct {
		name, method, url, bearer, ctype, body string
		host                                   string // Host header, if not the URL's
		code                                   int
		want                                   string // body prefix
	}{
		{name: "anonymous", url: "https://storage.googleapis.com/storage/v1/b/ours/o/x", code: 401},
		{name: "unknown token", url: "https://storage.googleapis.com/storage/v1/b/ours/o/x", bearer: "bad", code: 401},
		{name: "token", url: "https://storage.googleapis.com/storage/v1/b/ours/o/x", bearer: "good", code: 200, want: "gateway storage.googleapis.com"},
		{name: "token, other API", url: "https://iam.googleapis.com/v1/projects/p/serviceAccounts", bearer: "good", code: 200, want: "gateway iam.googleapis.com"},
		{name: "key grant", method: "POST", url: "https://oauth2.googleapis.com/token", ctype: "application/x-www-form-urlencoded", body: jwtGrant, code: 200, want: "gateway oauth2.googleapis.com " + jwtGrant},
		{name: "key grant as JSON", method: "POST", url: "https://oauth2.googleapis.com/token", ctype: "application/json", body: `{"grant_type":"urn:ietf:params:oauth:grant-type:jwt-bearer","assertion":"a.b.c"}`, code: 200, want: "gateway"},
		{name: "refresh grant", method: "POST", url: "https://oauth2.googleapis.com/token", ctype: "application/x-www-form-urlencoded", body: "grant_type=refresh_token&refresh_token=serviceAccount:admin@p.iam.gserviceaccount.com", code: 400},
		{name: "refresh grant in the query", method: "POST", url: "https://oauth2.googleapis.com/token?grant_type=refresh_token&refresh_token=user:admin@x", ctype: "text/plain", body: jwtGrant, code: 400},
		{name: "key grant with a query", method: "POST", url: "https://oauth2.googleapis.com/token?grant_type=refresh_token", ctype: "application/x-www-form-urlencoded", body: jwtGrant, code: 400},
		{name: "token exchange", method: "POST", url: "https://oauth2.googleapis.com/token", ctype: "application/x-www-form-urlencoded", body: "grant_type=urn:ietf:params:oauth:grant-type:token-exchange", code: 400},
		{name: "other oauth2 path", url: "https://oauth2.googleapis.com/tokeninfo?access_token=x", code: 401},
		{name: "foreign bucket", url: "https://storage.googleapis.com/proxy-golang-org-prod/abc?X-Goog-Signature=s", code: 200, want: "google"},
		{name: "foreign bucket, JSON API", url: "https://storage.googleapis.com/storage/v1/b/theirs/o/x?alt=media", code: 200, want: "google"},
		{name: "foreign bucket, token in the query", url: "https://storage.googleapis.com/theirs/x?access_token=good", code: 401},
		{name: "foreign bucket, API key", url: "https://storage.googleapis.com/theirs/x?key=k", code: 401},
		{name: "foreign bucket with a token", url: "https://storage.googleapis.com/theirs/x", bearer: "good", code: 200, want: "gateway"},
		{name: "foreign bucket write", method: "PUT", url: "https://storage.googleapis.com/theirs/x", body: "data", code: 401},
		{name: "signed URL", url: "https://storage.googleapis.com/ours/x?X-Goog-Signature=s", code: 200, want: "gateway"},
		{name: "registry", url: "https://us-central1-docker.pkg.dev/v2/", code: 200, want: "registry us-central1-docker.pkg.dev"},
		{name: "admin API", url: "https://www.googleapis.com/_emu/v1/info", bearer: "good", code: 404},
		{name: "console", url: "https://www.googleapis.com/console/", bearer: "good", code: 404},
		{name: "unserved Host", url: "https://storage.googleapis.com/storage/v1/b", host: "evil.example", bearer: "good", code: 421},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := tc.method
			if m == "" {
				m = "GET"
			}
			req, _ := http.NewRequest(m, tc.url, strings.NewReader(tc.body))
			if tc.bearer != "" {
				req.Header.Set("Authorization", "Bearer "+tc.bearer)
			}
			if tc.ctype != "" {
				req.Header.Set("Content-Type", tc.ctype)
			}
			if tc.host != "" {
				req.Host = tc.host
			}
			resp, err := hc.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != tc.code || !strings.HasPrefix(string(b), tc.want) {
				t.Fatalf("%d %q, want %d %q", resp.StatusCode, b, tc.code, tc.want)
			}
			if tc.code == 401 && resp.Header.Get("WWW-Authenticate") == "" {
				t.Error("401 without WWW-Authenticate")
			}
		})
	}

	// A name the Instance doesn't serve gets no certificate.
	if _, err := hc.Get("https://example.com/"); err == nil {
		t.Error("example.com: want a TLS failure")
	}
}

// TestLANInstance is #462's acceptance on one machine: through an
// Instance's --gfe-bind listener, a client with an unmodified key the
// Instance issued writes and reads an object, and anonymous requests, the
// registry's included, are refused.
func TestLANInstance(t *testing.T) {
	inst := emutest.Start(t, []string{"iam", "gcs", "ar"}, emutest.WithConfig(func(c *config.Config) {
		c.GFEBind = []string{"127.0.0.1"}
		c.Ports["gfe-lan"] = 0
	}))
	ctx := context.Background()
	tlsCfg := &tls.Config{RootCAs: inst.Internal().Env.CA.Pool()}
	client := func(addr string) *http.Client {
		return &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{
			DialContext: dialFrontend(addr), TLSClientConfig: tlsCfg, ForceAttemptHTTP2: true,
		}}
	}
	local := client(inst.Endpoint(frontend.Endpoint))
	lan := client(inst.Endpoint(frontend.LANEndpoint))

	// On this machine, through the loopback front end: an account, a
	// bucket and a key for the account.
	ic, err := iamv1.NewService(ctx, option.WithHTTPClient(local))
	if err != nil {
		t.Fatal(err)
	}
	sa, err := ic.Projects.ServiceAccounts.Create("projects/lan-project", &iamv1.CreateServiceAccountRequest{AccountId: "ci-runner"}).Do()
	if err != nil {
		t.Fatal(err)
	}
	key, err := ic.Projects.ServiceAccounts.Keys.Create("projects/-/serviceAccounts/"+sa.Email, &iamv1.CreateServiceAccountKeyRequest{}).Do()
	if err != nil {
		t.Fatal(err)
	}
	kf, _ := base64.StdEncoding.DecodeString(key.PrivateKeyData)
	sc, err := storage.NewClient(ctx, option.WithHTTPClient(local))
	if err != nil {
		t.Fatal(err)
	}
	defer sc.Close()
	if err := sc.Bucket("artifacts").Create(ctx, "lan-project", nil); err != nil {
		t.Fatal(err)
	}

	// From the LAN, with the key as it is.
	creds, err := google.CredentialsFromJSON(context.WithValue(ctx, oauth2.HTTPClient, lan), kf, storage.ScopeFullControl)
	if err != nil {
		t.Fatal(err)
	}
	authed := &http.Client{Timeout: 30 * time.Second, Transport: &oauth2.Transport{Source: creds.TokenSource, Base: lan.Transport}}
	lc, err := storage.NewClient(ctx, option.WithHTTPClient(authed))
	if err != nil {
		t.Fatal(err)
	}
	defer lc.Close()
	w := lc.Bucket("artifacts").Object("a.txt").NewWriter(ctx)
	io.WriteString(w, "from the LAN")
	if err := w.Close(); err != nil {
		t.Fatalf("write: %v", err)
	}
	r, err := lc.Bucket("artifacts").Object("a.txt").NewReader(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	got, _ := io.ReadAll(r)
	r.Close()
	if string(got) != "from the LAN" {
		t.Fatalf("read %q", got)
	}

	// containerd's pull with _json_key: an image pushed and pulled from
	// us-central1-docker.pkg.dev with the key alone.
	resp, err := local.Post("https://artifactregistry.googleapis.com/v1/projects/lan-project/locations/us-central1/repositories?repositoryId=images",
		"application/json", strings.NewReader(`{"format":"DOCKER"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create repository: %d", resp.StatusCode)
	}
	ref, err := name.ParseReference("us-central1-docker.pkg.dev/lan-project/images/app:v1")
	if err != nil {
		t.Fatal(err)
	}
	img, _ := random.Image(256, 1)
	jsonKey := remote.WithAuth(&authn.Basic{Username: "_json_key", Password: string(kf)})
	if err := remote.Write(ref, img, jsonKey, remote.WithTransport(lan.Transport)); err != nil {
		t.Fatalf("push with _json_key: %v", err)
	}
	if _, err := remote.Image(ref, jsonKey, remote.WithTransport(lan.Transport)); err != nil {
		t.Fatalf("pull with _json_key: %v", err)
	}

	for _, u := range []string{
		"https://storage.googleapis.com/storage/v1/b/artifacts/o/a.txt?alt=media",
		"https://storage.googleapis.com/artifacts/a.txt",
		"https://us-central1-docker.pkg.dev/v2/",
	} {
		resp, err := lan.Get(u)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("anonymous %s: %d, want 401", u, resp.StatusCode)
		}
	}
}
