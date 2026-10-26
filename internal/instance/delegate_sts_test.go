package instance

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/project"
)

// tokenExchange is STS's grant_type.
const tokenExchange = "urn:ietf:params:oauth:grant-type:token-exchange"

// oidcPool creates, through in, the workload identity pool "ci-pool" in
// Project ref with an OIDC provider "ci-oidc" that trusts the returned key and
// maps assertion.team to attribute.team. It returns the provider's
// audience, naming the Project by its number.
func oidcPool(t *testing.T, in *Instance, ref string) (*rsa.PrivateKey, string) {
	t.Helper()
	return jwksPool(t, in, ref, "ci-pool", "https://ci.example", "ci-audience")
}

// jwksPool is oidcPool for the pool pool, whose provider is for issuer,
// trusting the returned key by jwksJson, and allows audience.
func jwksPool(t *testing.T, in *Instance, ref, pool, issuer, audience string) (*rsa.PrivateKey, string) {
	t.Helper()
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	jwks, _ := json.Marshal(map[string]any{"keys": []map[string]string{{
		"kty": "RSA", "kid": "k1", "alg": "RS256",
		"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
		"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
	}}})
	base := "/iam/v1/projects/" + ref + "/locations/global/workloadIdentityPools"
	callAdmin(t, in, http.MethodPost, base+"?workloadIdentityPoolId="+pool, map[string]any{}, nil)
	callAdmin(t, in, http.MethodPost, base+"/"+pool+"/providers?workloadIdentityPoolProviderId=ci-oidc", map[string]any{
		"oidc":             map[string]any{"issuerUri": issuer, "jwksJson": string(jwks), "allowedAudiences": []string{audience}},
		"attributeMapping": map[string]string{"google.subject": "assertion.sub", "attribute.team": "assertion.team"},
	}, nil)
	return key, "//iam.googleapis.com/projects/" + project.NumberString(ref) + "/locations/global/workloadIdentityPools/" + pool + "/providers/ci-oidc"
}

// poolOf returns the name of the pool of oidcPool's audience.
func poolOf(audience string) string {
	return strings.TrimPrefix(strings.TrimSuffix(audience, "/providers/ci-oidc"), "//iam.googleapis.com/")
}

// subjectToken is an OIDC token for sub of team, signed by key.
func subjectToken(key *rsa.PrivateKey, sub, team string) string {
	return issuedToken(key, "https://ci.example", "ci-audience", sub, team)
}

// issuedToken is subjectToken from issuer for audience.
func issuedToken(key *rsa.PrivateKey, issuer, audience, sub, team string) string {
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"iss": issuer, "aud": audience, "sub": sub, "team": team,
		"iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix(),
	})
	tok.Header["kid"] = "k1"
	s, _ := tok.SignedString(key)
	return s
}

// exchange exchanges subject for an access token for a Federated
// principal at in's STS, as a form or as JSON, and returns the status and
// body.
func exchange(t *testing.T, in *Instance, audience, subject string, asJSON bool) (int, string) {
	t.Helper()
	fields := map[string]string{
		"grant_type": tokenExchange, "audience": audience, "subject_token": subject,
		"subject_token_type": "urn:ietf:params:oauth:token-type:jwt", "requested_token_type": "urn:ietf:params:oauth:token-type:access_token",
		"scope": "https://www.googleapis.com/auth/cloud-platform",
	}
	var body, contentType string
	if asJSON {
		b, _ := json.Marshal(fields)
		body, contentType = string(b), "application/json"
	} else {
		form := url.Values{}
		for k, v := range fields {
			form.Set(k, v)
		}
		body, contentType = form.Encode(), "application/x-www-form-urlencoded"
	}
	req, _ := http.NewRequest(http.MethodPost, "http://"+in.Env.Endpoints.Get("gateway")+"/sts/v1/token", strings.NewReader(body))
	req.Header.Set("Content-Type", contentType)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// federatedToken exchanges a subject token for sub of team through in and
// returns the access token for its Federated principal.
func federatedToken(t *testing.T, in *Instance, key *rsa.PrivateKey, audience, sub, team string) string {
	t.Helper()
	code, body := exchange(t, in, audience, subjectToken(key, sub, team), false)
	var reply struct {
		AccessToken string `json:"access_token"`
	}
	if code != http.StatusOK || json.Unmarshal([]byte(body), &reply) != nil || reply.AccessToken == "" {
		t.Fatalf("STS exchange through %s for %s = %d %s", in.Config.Instance, audience, code, body)
	}
	return reply.AccessToken
}

// An STS exchange through the laptop for a provider in the Delegated
// Project, as a form or JSON, is answered by the office, whose token the
// office knows. One naming the Project by ID is forwarded too, and the
// office refuses it, as STS wants the Project number. One for a provider
// in the laptop's own Project is answered by the laptop, as before (#711).
func TestDelegatedSTSExchange(t *testing.T) {
	laptop, office := delegated(t, nil, nil)
	key, audience := oidcPool(t, laptop, shared)
	for _, asJSON := range []bool{false, true} {
		code, body := exchange(t, laptop, audience, subjectToken(key, "build-1", "red"), asJSON)
		var reply struct {
			AccessToken string `json:"access_token"`
		}
		if code != http.StatusOK || json.Unmarshal([]byte(body), &reply) != nil {
			t.Errorf("exchange (JSON %v) through the laptop = %d %s", asJSON, code, body)
			continue
		}
		if p, ok := office.Env.Auth.Authenticate(t.Context(), reply.AccessToken); !ok || !strings.HasSuffix(string(p), "/workloadIdentityPools/ci-pool/subject/build-1") {
			t.Errorf("the office knows the token (JSON %v) as %q %v, want the pool's subject build-1", asJSON, p, ok)
		}
	}

	byID := strings.Replace(audience, "/projects/"+project.NumberString(shared)+"/", "/projects/"+shared+"/", 1)
	if code, body := exchange(t, laptop, byID, subjectToken(key, "build-1", "red"), false); code != http.StatusBadRequest || !strings.Contains(body, "invalid_grant") {
		t.Errorf("exchange naming the Project by ID through the laptop = %d %s, want 400 invalid_grant", code, body)
	}
	forwarded := 0
	for _, e := range laptop.Log.Entries("") {
		if e.Delegated == "office" && e.Method == "POST /sts/v1/token" {
			forwarded++
		}
	}
	if forwarded != 3 {
		t.Errorf("the laptop forwarded %d exchanges to the office, want 3, the one naming the Project by ID included", forwarded)
	}

	ownKey, ownAudience := oidcPool(t, laptop, "laptop-own")
	tok := federatedToken(t, laptop, ownKey, ownAudience, "build-2", "red")
	if _, ok := laptop.Env.Auth.Authenticate(t.Context(), tok); !ok {
		t.Error("the laptop does not know the token it exchanged for its own Project's provider")
	}
}

// addBinding grants member role on the resource at path, an API gateway
// path the :getIamPolicy and :setIamPolicy verbs follow, through in.
func addBinding(t *testing.T, in *Instance, path, role, member string) {
	t.Helper()
	var pol struct {
		Bindings []map[string]any `json:"bindings"`
		Etag     string           `json:"etag,omitempty"`
	}
	callAdmin(t, in, http.MethodPost, path+":getIamPolicy", map[string]any{}, &pol)
	pol.Bindings = append(pol.Bindings, map[string]any{"role": role, "members": []string{member}})
	callAdmin(t, in, http.MethodPost, path+":setIamPolicy", map[string]any{"policy": pol}, nil)
}

// bearerPost posts body as JSON to path on in's API gateway with the
// access token tok.
func bearerPost(t *testing.T, in *Instance, path, tok string, body any) (int, string) {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, "http://"+in.Env.Endpoints.Get("gateway")+path, strings.NewReader(string(b)))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(rb)
}

// An access token the office issued for a Federated principal of a pool
// in the Delegated Project works through the laptop under IAM enforcement: it impersonates
// a service account that grants it roles/iam.workloadIdentityUser, the
// office's IAM checks it on the Delegated Project's resources, principalSet
// bindings included, and on the laptop's own resources a principal://
// binding for it matches while a principalSet one for its pool does not
// (#711).
func TestDelegatedFederatedPrincipal(t *testing.T) {
	enforce := func(c *config.Config) { c.IAMMode = config.IAMEnforce }
	laptop, _ := delegated(t, enforce, enforce)
	key, audience := oidcPool(t, laptop, shared)
	pool := poolOf(audience)
	member := "principal://iam.googleapis.com/" + pool + "/subject/build-1"
	red := federatedToken(t, laptop, key, audience, "build-1", "red")
	blue := federatedToken(t, laptop, key, audience, "build-2", "blue")

	callAdmin(t, laptop, http.MethodPost, "/iam/v1/projects/"+shared+"/serviceAccounts", map[string]string{"accountId": "runner"}, nil)
	addBinding(t, laptop, "/iam/v1/projects/"+shared+"/serviceAccounts/"+runner, "roles/iam.workloadIdentityUser", member)
	generate := "/iamcredentials/v1/projects/-/serviceAccounts/" + runner + ":generateAccessToken"
	scope := map[string]any{"scope": []string{"https://www.googleapis.com/auth/cloud-platform"}}
	if code, body := bearerPost(t, laptop, generate, red, scope); code != http.StatusOK {
		t.Errorf("generateAccessToken through the laptop as %s = %d %s", member, code, body)
	}
	if code, body := bearerPost(t, laptop, generate, blue, scope); code != http.StatusForbidden {
		t.Errorf("generateAccessToken through the laptop as another subject = %d %s, want 403", code, body)
	}

	addBinding(t, laptop, "/cloudresourcemanager/v1/projects/"+shared, "roles/pubsub.viewer", "principalSet://iam.googleapis.com/"+pool+"/attribute.team/red")
	topics := "/pubsub/v1/projects/" + shared + "/topics"
	if code, body := bearerGet(t, laptop, topics, red); code != http.StatusOK {
		t.Errorf("listing the Delegated Project's topics through the laptop as team red = %d %s", code, body)
	}
	if code, body := bearerGet(t, laptop, topics, blue); code != http.StatusForbidden {
		t.Errorf("listing the Delegated Project's topics through the laptop as team blue = %d %s, want 403", code, body)
	}

	addBinding(t, laptop, "/cloudresourcemanager/v1/projects/laptop-own", "roles/pubsub.viewer", member)
	addBinding(t, laptop, "/cloudresourcemanager/v1/projects/laptop-own", "roles/pubsub.editor", "principalSet://iam.googleapis.com/"+pool+"/*")
	if code, body := bearerGet(t, laptop, "/pubsub/v1/projects/laptop-own/topics", red); code != http.StatusOK {
		t.Errorf("listing the laptop's own topics with a principal:// binding = %d %s", code, body)
	}
	if code, body := bearerGet(t, laptop, "/pubsub/v1/projects/laptop-own/topics", blue); code != http.StatusForbidden {
		t.Errorf("listing the laptop's own topics with only a principalSet binding = %d %s, want 403", code, body)
	}
}

// The laptop asserts a Federated principal the office vouches for only
// when its pool's Project is delegated to the office, and the office
// vouches for one only when its pool's Project is shared with the laptop:
// otherwise its token gets 401 through the laptop under IAM enforcement,
// even on a resource that grants the principal (#711).
func TestFederatedPrincipalScopedToDelegation(t *testing.T) {
	laptop, office := delegated(t, func(c *config.Config) { c.IAMMode = config.IAMEnforce }, nil)
	ownTopics := "/pubsub/v1/projects/laptop-own/topics"
	grant := func(aud, sub string) {
		pool := poolOf(aud)
		addBinding(t, laptop, "/cloudresourcemanager/v1/projects/laptop-own", "roles/pubsub.viewer", "principal://iam.googleapis.com/"+pool+"/subject/"+sub)
	}

	// office-extra is shared with the laptop but not delegated.
	callAdmin(t, office, http.MethodPost, "/_emu/v1/links/shares", map[string]string{"project": "office-extra"}, nil)
	extraKey, extraAudience := oidcPool(t, office, "office-extra")
	grant(extraAudience, "build-1")
	if code, body := bearerGet(t, laptop, ownTopics, federatedToken(t, office, extraKey, extraAudience, "build-1", "red")); code != http.StatusUnauthorized {
		t.Errorf("a token for a pool in a Project not delegated to the office, through the laptop = %d %s, want 401", code, body)
	}

	// team-shared is delegated, but the office no longer shares it.
	key, audience := oidcPool(t, laptop, shared)
	grant(audience, "build-2")
	tok := federatedToken(t, office, key, audience, "build-2", "red")
	callAdmin(t, office, http.MethodDelete, "/_emu/v1/links/shares/"+shared, nil, nil)
	if code, body := bearerGet(t, laptop, ownTopics, tok); code != http.StatusUnauthorized {
		t.Errorf("a token for a pool in a Project the office no longer shares, through the laptop = %d %s, want 401", code, body)
	}
}

// With the office down, an STS exchange through the laptop for a provider
// in the Delegated Project fails as STS does, with 503 and an OAuth error
// naming the link (#711).
func TestDelegatedSTSExchangeWhileLinkedInstanceIsDown(t *testing.T) {
	laptop, office := delegated(t, nil, nil)
	key, audience := oidcPool(t, laptop, shared)
	if err := office.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	code, body := exchange(t, laptop, audience, subjectToken(key, "build-1", "red"), false)
	var reply struct {
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	_ = json.Unmarshal([]byte(body), &reply)
	if code != http.StatusServiceUnavailable || reply.Error != "temporarily_unavailable" || !strings.Contains(reply.ErrorDescription, `"office"`) {
		t.Errorf("exchange with the office down = %d %s, want 503 temporarily_unavailable naming the office", code, body)
	}
}
