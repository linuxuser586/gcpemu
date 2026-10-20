package ar_test

import (
	"crypto/tls"
	"encoding/base64"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	iamv1 "google.golang.org/api/iam/v1"
	"google.golang.org/api/option"

	"github.com/linuxuser586/gcpemu/emutest"
	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/emu"
)

// --ar-bind serves the registry over TLS from the instance CA and only to
// callers whose credentials authenticate, even in audit mode, while the
// loopback registry keeps serving anonymous callers.
func TestRegistryLANRequiresAuth(t *testing.T) {
	e := start(t, emutest.WithConfig(func(c *config.Config) { c.ARBind = []string{"127.0.0.1"} }))
	e.createRepo(testLoc, "images")
	lan := e.inst.Endpoint("ar-lan")
	if lan == "" || lan == e.registry() {
		t.Fatalf("ar-lan endpoint = %q (registry %s)", lan, e.registry())
	}
	tr := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: e.inst.Internal().Env.CA.Pool()}}
	hc := &http.Client{Transport: tr}
	ref, err := name.ParseReference(lan + "/" + testProject + "/images/app:v1")
	if err != nil {
		t.Fatal(err)
	}

	// Anonymous: challenged for Basic credentials, on every route.
	for _, p := range []string{"/v2/", "/v2/_catalog", "/v2/" + testProject + "/images/app/manifests/v1"} {
		resp, err := hc.Get("https://" + lan + p)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized || !strings.HasPrefix(resp.Header.Get("WWW-Authenticate"), "Basic ") {
			t.Errorf("anonymous GET %s = %d %q", p, resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
		}
	}
	// The token endpoint hands out no anonymous token.
	resp, err := hc.Get("https://" + lan + "/v2/token")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("anonymous token = %d", resp.StatusCode)
	}
	// A bearer the emulator never issued is refused too.
	req, _ := http.NewRequest(http.MethodGet, "https://"+lan+"/v2/", nil)
	req.Header.Set("Authorization", "Bearer gcpemu-anonymous")
	if resp, err = hc.Do(req); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("anonymous bearer = %d", resp.StatusCode)
	}

	img, _ := random.Image(256, 1)
	if err := remote.Write(ref, img, remote.WithTransport(tr)); !isStatus(err, http.StatusUnauthorized) {
		t.Fatalf("anonymous push = %v", err)
	}
	bad := remote.WithAuth(&authn.Basic{Username: "oauth2accesstoken", Password: "bogus"})
	if _, err := remote.Image(ref, bad, remote.WithTransport(tr)); !isStatus(err, http.StatusUnauthorized) {
		t.Fatalf("pull with bad token = %v", err)
	}

	// An access token from the emulator's IAM pushes and pulls.
	svc, ok := e.inst.Internal().Env.Lookup("iam")
	if !ok {
		t.Fatal("iam service not running")
	}
	keys := svc.(emu.ServiceAccountKeys)
	tok, _, err := keys.AccessToken(e.ctx, emu.Principal(e.inst.Internal().Config.DefaultPrincipal))
	if err != nil {
		t.Fatal(err)
	}
	owner := remote.WithAuth(&authn.Basic{Username: "oauth2accesstoken", Password: tok})
	if err := remote.Write(ref, img, owner, remote.WithTransport(tr)); err != nil {
		t.Fatalf("push with token: %v", err)
	}
	if _, err := remote.Image(ref, owner, remote.WithTransport(tr)); err != nil {
		t.Fatalf("pull with token: %v", err)
	}

	// So does a service account key, BuildKit's long-lived credential.
	iamSvc, err := iamv1.NewService(e.ctx, option.WithEndpoint(e.inst.GatewayURL()+"/iam/"), option.WithoutAuthentication(),
		option.WithHTTPClient(&http.Client{Transport: bearer{tok}}))
	if err != nil {
		t.Fatal(err)
	}
	sa, err := iamSvc.Projects.ServiceAccounts.Create("projects/"+testProject, &iamv1.CreateServiceAccountRequest{AccountId: "builder"}).Do()
	if err != nil {
		t.Fatalf("create SA: %v", err)
	}
	key, err := iamSvc.Projects.ServiceAccounts.Keys.Create(sa.Name, &iamv1.CreateServiceAccountKeyRequest{}).Do()
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	keyJSON, _ := base64.StdEncoding.DecodeString(key.PrivateKeyData)
	sakey := remote.WithAuth(&authn.Basic{Username: "_json_key", Password: string(keyJSON)})
	if _, err := remote.Image(ref, sakey, remote.WithTransport(tr)); err != nil {
		t.Fatalf("pull with SA key: %v", err)
	}
	// Basic credentials also trade for a token at the token endpoint.
	req, _ = http.NewRequest(http.MethodGet, "https://"+lan+"/v2/token?"+url.Values{"scope": {"repository:" + testProject + "/images/app:pull"}}.Encode(), nil)
	req.SetBasicAuth("oauth2accesstoken", tok)
	if resp, err = hc.Do(req); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("token with credentials = %d", resp.StatusCode)
	}

	// Loopback is unchanged: anonymous pulls work in audit mode.
	if _, err := remote.Image(e.ref(testProject + "/images/app:v1")); err != nil {
		t.Fatalf("anonymous pull on loopback: %v", err)
	}
}
