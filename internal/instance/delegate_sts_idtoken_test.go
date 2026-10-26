package instance

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/project"
)

// googlePool creates, through in, the workload identity pool "sa-pool" in
// Project ref with an OIDC provider "google" whose issuer is the
// Instance's own, with no jwksJson, that allows the audience
// https://app.example and maps assertion.sub. It returns the provider's
// audience.
func googlePool(t *testing.T, in *Instance, ref string) string {
	t.Helper()
	base := "/iam/v1/projects/" + ref + "/locations/global/workloadIdentityPools"
	callAdmin(t, in, http.MethodPost, base+"?workloadIdentityPoolId=sa-pool", map[string]any{}, nil)
	callAdmin(t, in, http.MethodPost, base+"/sa-pool/providers?workloadIdentityPoolProviderId=google", map[string]any{
		"oidc":             map[string]any{"issuerUri": "https://accounts.google.com", "allowedAudiences": []string{"https://app.example"}},
		"attributeMapping": map[string]string{"google.subject": "assertion.sub"},
	}, nil)
	return "//iam.googleapis.com/projects/" + project.NumberString(ref) + "/locations/global/workloadIdentityPools/sa-pool/providers/google"
}

// idTokenFor gets an ID token for email, with or without the email claim,
// from generateIdToken through in's API gateway.
func idTokenFor(t *testing.T, in *Instance, email string, includeEmail bool) string {
	t.Helper()
	var reply struct{ Token string }
	callAdmin(t, in, http.MethodPost, "/iamcredentials/v1/projects/-/serviceAccounts/"+email+":generateIdToken",
		map[string]any{"audience": "https://app.example", "includeEmail": includeEmail}, &reply)
	if strings.Count(reply.Token, ".") != 2 {
		t.Fatalf("generateIdToken for %s through %s = %q", email, in.Config.Instance, reply.Token)
	}
	return reply.Token
}

// An ID token the office signs for a service account in the Delegated
// Project, got through the laptop with or without the email claim,
// exchanges at the laptop's STS for a provider in the laptop's own pool
// that trusts the Instance's own issuer, as the laptop's own ID tokens do
// (#702).
func TestDelegatedIDTokenExchangesAtFrontInstance(t *testing.T) {
	laptop, _ := delegated(t, nil, nil)
	audience := googlePool(t, laptop, "laptop-own")
	callAdmin(t, laptop, http.MethodPost, "/iam/v1/projects/"+shared+"/serviceAccounts", map[string]string{"accountId": "runner"}, nil)
	callAdmin(t, laptop, http.MethodPost, "/iam/v1/projects/laptop-own/serviceAccounts", map[string]string{"accountId": "local-runner"}, nil)
	for _, email := range []string{runner, "local-runner@laptop-own.iam.gserviceaccount.com"} {
		for _, includeEmail := range []bool{false, true} {
			code, body := exchange(t, laptop, audience, idTokenFor(t, laptop, email, includeEmail), false)
			if code != http.StatusOK || !strings.Contains(body, "access_token") {
				t.Errorf("exchanging the ID token of %s (email claim %v) at the laptop = %d %s", email, includeEmail, code, body)
			}
		}
	}
}

// rejected asserts that exchanging the ID token tok at in's STS for
// audience fails with 400 invalid_grant.
func rejected(t *testing.T, in *Instance, audience, tok, what string) {
	t.Helper()
	if code, body := exchange(t, in, audience, tok, false); code != http.StatusBadRequest || !strings.Contains(body, "invalid_grant") {
		t.Errorf("exchanging %s at the %s = %d %s, want 400 invalid_grant", what, in.Config.Instance, code, body)
	}
}

// The laptop exchanges an ID token the office signed only for a service
// account, not disabled, of a Project delegated to the office: not one of
// a Project the office shares but the laptop does not delegate, not one
// claiming the laptop's own service account, and not one of a disabled
// account (#702).
func TestLinkedIDTokenScopedToDelegation(t *testing.T) {
	laptop, office := delegated(t, nil, nil)
	audience := googlePool(t, laptop, "laptop-own")

	callAdmin(t, office, http.MethodPost, "/_emu/v1/links/shares", map[string]string{"project": "office-extra"}, nil)
	callAdmin(t, office, http.MethodPost, "/iam/v1/projects/office-extra/serviceAccounts", map[string]string{"accountId": "extra-runner"}, nil)
	rejected(t, laptop, audience, idTokenFor(t, office, "extra-runner@office-extra.iam.gserviceaccount.com", true), "the ID token of a Project shared but not delegated")

	callAdmin(t, laptop, http.MethodPost, "/iam/v1/projects/laptop-own/serviceAccounts", map[string]string{"accountId": "local-runner"}, nil)
	svc, _ := office.Env.Lookup("iam")
	forged, err := svc.(emu.ServiceAccountKeys).IDToken(t.Context(), "local-runner@laptop-own.iam.gserviceaccount.com", "https://app.example")
	if err != nil {
		t.Fatal(err)
	}
	rejected(t, laptop, audience, forged, "an ID token the office signed for the laptop's own service account")

	callAdmin(t, laptop, http.MethodPost, "/iam/v1/projects/"+shared+"/serviceAccounts", map[string]string{"accountId": "runner"}, nil)
	tok := idTokenFor(t, laptop, runner, true)
	callAdmin(t, laptop, http.MethodPost, "/iam/v1/projects/"+shared+"/serviceAccounts/"+runner+":disable", map[string]any{}, nil)
	rejected(t, laptop, audience, tok, "the ID token of a disabled service account")
}

// With the office down, exchanging its ID token at the laptop fails with
// 503 temporarily_unavailable naming the link, while the laptop's own ID
// tokens, and subject tokens of providers with jwksJson or a foreign
// issuer, for which no Linked Instance is asked, still exchange (#702).
func TestLinkedIDTokenWhileLinkedInstanceIsDown(t *testing.T) {
	laptop, office := delegated(t, nil, nil)
	audience := googlePool(t, laptop, "laptop-own")
	callAdmin(t, laptop, http.MethodPost, "/iam/v1/projects/"+shared+"/serviceAccounts", map[string]string{"accountId": "runner"}, nil)
	callAdmin(t, laptop, http.MethodPost, "/iam/v1/projects/laptop-own/serviceAccounts", map[string]string{"accountId": "local-runner"}, nil)
	delegatedTok := idTokenFor(t, laptop, runner, true)
	ownTok := idTokenFor(t, laptop, "local-runner@laptop-own.iam.gserviceaccount.com", true)
	ciKey, ciAudience := oidcPool(t, laptop, "laptop-own")
	jwksKey, jwksAudience := googleJWKSPool(t, laptop)
	if err := office.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}

	code, body := exchange(t, laptop, audience, delegatedTok, false)
	var reply struct {
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	_ = json.Unmarshal([]byte(body), &reply)
	if code != http.StatusServiceUnavailable || reply.Error != "temporarily_unavailable" || !strings.Contains(reply.ErrorDescription, `"office"`) {
		t.Errorf("exchanging the office's ID token with the office down = %d %s, want 503 temporarily_unavailable naming the office", code, body)
	}
	for what, ex := range map[string][2]string{
		"the laptop's own ID token":                         {audience, ownTok},
		"a token of a provider with a foreign issuer":       {ciAudience, subjectToken(ciKey, "build-1", "red")},
		"a token of a Google-issuer provider with jwksJson": {jwksAudience, googleToken(jwksKey)},
	} {
		if code, body := exchange(t, laptop, ex[0], ex[1], false); code != http.StatusOK {
			t.Errorf("exchanging %s with the office down = %d %s", what, code, body)
		}
	}
}

// googleJWKSPool creates, through in, the pool "jwks-pool" in laptop-own
// with a provider whose issuer is the Instance's own but which trusts the
// returned key by jwksJson. It returns the key and the provider's
// audience.
func googleJWKSPool(t *testing.T, in *Instance) (*rsa.PrivateKey, string) {
	t.Helper()
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	jwks, _ := json.Marshal(map[string]any{"keys": []map[string]string{{
		"kty": "RSA", "kid": "k1", "alg": "RS256",
		"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
		"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
	}}})
	base := "/iam/v1/projects/laptop-own/locations/global/workloadIdentityPools"
	callAdmin(t, in, http.MethodPost, base+"?workloadIdentityPoolId=jwks-pool", map[string]any{}, nil)
	callAdmin(t, in, http.MethodPost, base+"/jwks-pool/providers?workloadIdentityPoolProviderId=google", map[string]any{
		"oidc": map[string]any{"issuerUri": "https://accounts.google.com", "jwksJson": string(jwks), "allowedAudiences": []string{"https://app.example"}},
	}, nil)
	return key, "//iam.googleapis.com/projects/" + project.NumberString("laptop-own") + "/locations/global/workloadIdentityPools/jwks-pool/providers/google"
}

// googleToken is an ID token with the Instance's own issuer, signed by key.
func googleToken(key *rsa.PrivateKey) string {
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"iss": "https://accounts.google.com", "aud": "https://app.example", "sub": "build-1",
		"iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix(),
	})
	tok.Header["kid"] = "k1"
	s, _ := tok.SignedString(key)
	return s
}

// An ID token the office signed is never an access token: presented as a
// bearer credential through the laptop under IAM enforcement it gets 401
// (#702).
func TestLinkedIDTokenIsNotABearerCredential(t *testing.T) {
	laptop, _ := delegated(t, func(c *config.Config) { c.IAMMode = config.IAMEnforce }, nil)
	callAdmin(t, laptop, http.MethodPost, "/iam/v1/projects/"+shared+"/serviceAccounts", map[string]string{"accountId": "runner"}, nil)
	tok := idTokenFor(t, laptop, runner, true)
	for _, path := range []string{"/pubsub/v1/projects/laptop-own/topics", "/pubsub/v1/projects/" + shared + "/topics"} {
		if code, body := bearerGet(t, laptop, path, tok); code != http.StatusUnauthorized {
			t.Errorf("GET %s with the office's ID token as a bearer credential = %d %s, want 401", path, code, body)
		}
	}
}
