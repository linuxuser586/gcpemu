package iam_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/oauth2/google"
	iamv1 "google.golang.org/api/iam/v1"

	"github.com/linuxuser586/gcpemu/internal/project"
)

// TestWorkloadIdentityFederation covers FR-IAM-009: pools, OIDC providers
// with attribute mapping/condition, STS exchange and SA impersonation via
// an external_account credential file.
func TestWorkloadIdentityFederation(t *testing.T) {
	inst := startIAM(t, enforce)
	c := iamClient(t, inst)
	num := project.NumberString(testProject)
	issuerKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	jwks, _ := json.Marshal(map[string]any{"keys": []map[string]string{{
		"kty": "RSA", "kid": "k1", "alg": "RS256",
		"n": base64.RawURLEncoding.EncodeToString(issuerKey.N.Bytes()),
		"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(issuerKey.E)).Bytes()),
	}}})

	op, err := c.Projects.Locations.WorkloadIdentityPools.Create("projects/"+testProject+"/locations/global",
		&iamv1.WorkloadIdentityPool{DisplayName: "GitHub"}).WorkloadIdentityPoolId("gh-pool").Do()
	if err != nil || !op.Done {
		t.Fatalf("create pool: %v %+v", err, op)
	}
	poolName := "projects/" + num + "/locations/global/workloadIdentityPools/gh-pool"
	pool, err := c.Projects.Locations.WorkloadIdentityPools.Get(poolName).Do()
	if err != nil || pool.State != "ACTIVE" {
		t.Fatalf("get pool: %v %+v", err, pool)
	}
	_, err = c.Projects.Locations.WorkloadIdentityPools.Providers.Create(poolName, &iamv1.WorkloadIdentityPoolProvider{
		Oidc:               &iamv1.Oidc{IssuerUri: "https://token.actions.example", JwksJson: string(jwks)},
		AttributeMapping:   map[string]string{"google.subject": "assertion.sub", "attribute.repository": "assertion.repository"},
		AttributeCondition: "assertion.repository_owner == 'acme'",
	}).WorkloadIdentityPoolProviderId("gh-provider").Do()
	if err != nil {
		t.Fatalf("create provider: %v", err)
	}
	provName := poolName + "/providers/gh-provider"

	sa := createSA(t, c, "ci-deployer")
	grant(t, c, sa, "roles/iam.workloadIdentityUser",
		"principalSet://iam.googleapis.com/"+poolName+"/attribute.repository/acme/app")

	dir := t.TempDir()
	writeSubject := func(owner, repo string) string {
		tok := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
			"iss": "https://token.actions.example", "aud": "https://iam.googleapis.com/" + provName,
			"sub": "repo:" + repo + ":ref:refs/heads/main", "repository": repo, "repository_owner": owner,
			"iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix(),
		})
		tok.Header["kid"] = "k1"
		s, _ := tok.SignedString(issuerKey)
		p := filepath.Join(dir, owner+"-token")
		_ = os.WriteFile(p, []byte(s), 0o600)
		return p
	}
	credFile := func(subjectPath string) []byte {
		b, _ := json.Marshal(map[string]any{
			"type":                              "external_account",
			"audience":                          "//iam.googleapis.com/" + provName,
			"subject_token_type":                "urn:ietf:params:oauth:token-type:jwt",
			"token_url":                         inst.GatewayURL() + "/sts/v1/token",
			"service_account_impersonation_url": inst.GatewayURL() + "/iamcredentials/v1/projects/-/serviceAccounts/" + sa.Email + ":generateAccessToken",
			"credential_source":                 map[string]string{"file": subjectPath},
		})
		return b
	}
	ctx := context.Background()
	creds, err := google.CredentialsFromJSON(ctx, credFile(writeSubject("acme", "acme/app")), "https://www.googleapis.com/auth/cloud-platform")
	if err != nil {
		t.Fatal(err)
	}
	tok, err := creds.TokenSource.Token()
	if err != nil {
		t.Fatalf("federated token: %v", err)
	}
	info := fetchJSON[map[string]string](t, inst.GatewayURL()+"/oauth2/tokeninfo?access_token="+tok.AccessToken)
	if info["email"] != sa.Email {
		t.Fatalf("impersonated token: %v", info)
	}

	// The attribute condition rejects other owners.
	bad, _ := google.CredentialsFromJSON(ctx, credFile(writeSubject("evil", "acme/app")), "https://www.googleapis.com/auth/cloud-platform")
	if _, err := bad.TokenSource.Token(); err == nil {
		t.Fatal("expected attribute condition rejection")
	}
}
