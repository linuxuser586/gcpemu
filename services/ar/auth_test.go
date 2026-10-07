package ar_test

import (
	"encoding/base64"
	"errors"
	"net/http"
	"testing"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
	iamv1 "google.golang.org/api/iam/v1"
	"google.golang.org/api/option"

	"github.com/linuxuser586/gcpemu/emutest"
	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/emu"
)

// FR-AR-003 in enforce mode with credentials issued by the iam service.
func TestRegistryEnforceWithCredentials(t *testing.T) {
	e := start(t, emutest.WithIAMMode(config.IAMEnforce))
	e.createRepo(testLoc, "images")
	svc, ok := e.inst.Env.Lookup("iam")
	if !ok {
		t.Skip("iam service not running")
	}
	keys, ok := svc.(emu.ServiceAccountKeys)
	if !ok {
		t.Skip("iam does not provide ServiceAccountKeys")
	}
	// The default principal holds every permission.
	tok, _, err := keys.AccessToken(e.ctx, emu.Principal(e.inst.Config.DefaultPrincipal))
	if err != nil {
		t.Fatal(err)
	}
	img, _ := random.Image(256, 1)
	ref := e.ref(testProject + "/images/app:v1")
	owner := remote.WithAuth(&authn.Basic{Username: "oauth2accesstoken", Password: tok})
	if err := remote.Write(ref, img, owner); err != nil {
		t.Fatalf("push as owner: %v", err)
	}
	if _, err := remote.Image(ref, owner); err != nil {
		t.Fatalf("pull as owner: %v", err)
	}
	// Bad credentials are rejected at the token endpoint.
	bad := remote.WithAuth(&authn.Basic{Username: "oauth2accesstoken", Password: "bogus"})
	if _, err := remote.Image(ref, bad); !isStatus(err, http.StatusUnauthorized) {
		t.Fatalf("pull with bad token = %v", err)
	}

	// A service account key (_json_key) authenticates as the account, which
	// has no role on the repository: DENIED.
	iamSvc, err := iamv1.NewService(e.ctx, option.WithEndpoint(e.inst.GatewayURL()+"/iam/"), option.WithoutAuthentication(),
		option.WithHTTPClient(&http.Client{Transport: bearer{tok}}))
	if err != nil {
		t.Fatal(err)
	}
	sa, err := iamSvc.Projects.ServiceAccounts.Create("projects/"+testProject, &iamv1.CreateServiceAccountRequest{AccountId: "puller"}).Do()
	if err != nil {
		t.Fatalf("create SA: %v", err)
	}
	key, err := iamSvc.Projects.ServiceAccounts.Keys.Create(sa.Name, &iamv1.CreateServiceAccountKeyRequest{}).Do()
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	keyJSON, err := base64.StdEncoding.DecodeString(key.PrivateKeyData)
	if err != nil {
		t.Fatal(err)
	}
	sakey := remote.WithAuth(&authn.Basic{Username: "_json_key", Password: string(keyJSON)})
	if _, err := remote.Image(ref, sakey); !isStatus(err, http.StatusForbidden) {
		t.Fatalf("pull as unprivileged SA = %v", err)
	}
}

type bearer struct{ tok string }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.tok)
	return http.DefaultTransport.RoundTrip(r)
}

func isStatus(err error, code int) bool {
	var te *transport.Error
	return errors.As(err, &te) && te.StatusCode == code
}
