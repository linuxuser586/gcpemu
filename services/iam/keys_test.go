package iam_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	crmv1 "google.golang.org/api/cloudresourcemanager/v1"
	iamv1 "google.golang.org/api/iam/v1"
	"google.golang.org/api/option"
)

// createKeyFile creates a JSON key for sa and returns the decoded file.
func createKeyFile(t *testing.T, c *iamv1.Service, sa *iamv1.ServiceAccount) (*iamv1.ServiceAccountKey, []byte) {
	t.Helper()
	key, err := c.Projects.ServiceAccounts.Keys.Create("projects/-/serviceAccounts/"+sa.Email, &iamv1.CreateServiceAccountKeyRequest{}).Do()
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	kf, err := base64.StdEncoding.DecodeString(key.PrivateKeyData)
	if err != nil {
		t.Fatal(err)
	}
	return key, kf
}

func crmClient(t *testing.T, inst *testInstance, opts ...option.ClientOption) *crmv1.Service {
	t.Helper()
	if len(opts) == 0 {
		opts = []option.ClientOption{option.WithoutAuthentication()}
	}
	opts = append(opts, option.WithEndpoint(inst.GatewayURL()+"/cloudresourcemanager/"))
	c, err := crmv1.NewService(context.Background(), opts...)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestKeys(t *testing.T) {
	inst := startIAM(t)
	c := iamClient(t, inst)
	sa := createSA(t, c, "key-holder")
	key, kf := createKeyFile(t, c, sa)
	if key.KeyType != "USER_MANAGED" || key.KeyAlgorithm != "KEY_ALG_RSA_2048" || key.PrivateKeyType != "TYPE_GOOGLE_CREDENTIALS_FILE" {
		t.Fatalf("unexpected key %+v", key)
	}
	var f map[string]string
	if err := json.Unmarshal(kf, &f); err != nil {
		t.Fatal(err)
	}
	if f["type"] != "service_account" || f["client_email"] != sa.Email || f["token_uri"] != inst.GatewayURL()+"/oauth2/token" {
		t.Fatalf("unexpected key file %v", f)
	}
	keyID := key.Name[strings.LastIndex(key.Name, "/")+1:]
	if f["private_key_id"] != keyID {
		t.Fatalf("private_key_id %s != %s", f["private_key_id"], keyID)
	}

	got, err := c.Projects.ServiceAccounts.Keys.Get(key.Name).PublicKeyType("TYPE_X509_PEM_FILE").Do()
	if err != nil {
		t.Fatal(err)
	}
	pemData, _ := base64.StdEncoding.DecodeString(got.PublicKeyData)
	if blk, _ := pem.Decode(pemData); blk == nil || blk.Type != "CERTIFICATE" {
		t.Fatalf("publicKeyData is not a PEM certificate: %q", pemData)
	}

	// Upload a user-provided certificate.
	priv, _ := rsa.GenerateKey(rand.Reader, 2048)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "x"}, NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour)}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	up, err := c.Projects.ServiceAccounts.Keys.Upload("projects/-/serviceAccounts/"+sa.Email, &iamv1.UploadServiceAccountKeyRequest{
		PublicKeyData: base64.StdEncoding.EncodeToString(certPEM),
	}).Do()
	if err != nil || up.KeyOrigin != "USER_PROVIDED" {
		t.Fatalf("upload: %v %+v", err, up)
	}

	list, err := c.Projects.ServiceAccounts.Keys.List("projects/-/serviceAccounts/" + sa.Email).KeyTypes("USER_MANAGED").Do()
	if err != nil || len(list.Keys) != 2 {
		t.Fatalf("list: %v %+v", err, list)
	}
	if _, err := c.Projects.ServiceAccounts.Keys.Delete(up.Name).Do(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Projects.ServiceAccounts.Keys.Get(up.Name).Do(); httpCode(err) != 404 {
		t.Fatalf("get deleted key: want 404, got %v", err)
	}
	if _, err := c.Projects.ServiceAccounts.Keys.Create("projects/-/serviceAccounts/"+sa.Email, &iamv1.CreateServiceAccountKeyRequest{PrivateKeyType: "TYPE_PKCS12_FILE"}).Do(); err == nil {
		t.Fatal("PKCS12 key creation should fail")
	}
}

// TestKeyFileTokenLoop proves FR-CORE-052: a JSON key created by the
// emulator yields a token (JWT-bearer grant) that identifies the service
// account, and enforce mode denies it until a role is bound.
func TestKeyFileTokenLoop(t *testing.T) {
	inst := startIAM(t, enforce)
	ctx := context.Background()
	admin := iamClient(t, inst) // default principal: implicit owner
	sa := createSA(t, admin, "deployer")
	_, kf := createKeyFile(t, admin, sa)

	creds, err := google.CredentialsFromJSON(ctx, kf, "https://www.googleapis.com/auth/cloud-platform")
	if err != nil {
		t.Fatal(err)
	}
	tok, err := creds.TokenSource.Token()
	if err != nil {
		t.Fatalf("token exchange: %v", err)
	}
	if !strings.HasPrefix(tok.AccessToken, "ya29.") || time.Until(tok.Expiry) < 50*time.Minute {
		t.Fatalf("unexpected token %+v", tok)
	}

	// tokeninfo identifies the account.
	resp, err := http.Get(inst.GatewayURL() + "/oauth2/tokeninfo?access_token=" + url.QueryEscape(tok.AccessToken))
	if err != nil {
		t.Fatal(err)
	}
	var info map[string]string
	_ = json.NewDecoder(resp.Body).Decode(&info)
	resp.Body.Close()
	if info["email"] != sa.Email {
		t.Fatalf("tokeninfo: %v", info)
	}

	asSA := crmClient(t, inst, option.WithTokenSource(oauth2.StaticTokenSource(tok)))
	if _, err := asSA.Projects.GetIamPolicy(testProject, &crmv1.GetIamPolicyRequest{}).Do(); httpCode(err) != 403 {
		t.Fatalf("enforce: want 403 before binding, got %v", err)
	}

	// Grant roles/viewer as the default principal.
	adminCRM := crmClient(t, inst)
	pol, err := adminCRM.Projects.GetIamPolicy(testProject, &crmv1.GetIamPolicyRequest{}).Do()
	if err != nil {
		t.Fatal(err)
	}
	pol.Bindings = append(pol.Bindings, &crmv1.Binding{Role: "roles/viewer", Members: []string{"serviceAccount:" + sa.Email}})
	if _, err := adminCRM.Projects.SetIamPolicy(testProject, &crmv1.SetIamPolicyRequest{Policy: pol}).Do(); err != nil {
		t.Fatal(err)
	}
	if _, err := asSA.Projects.GetIamPolicy(testProject, &crmv1.GetIamPolicyRequest{}).Do(); err != nil {
		t.Fatalf("after binding: %v", err)
	}
	// Viewer cannot set policy.
	if _, err := asSA.Projects.SetIamPolicy(testProject, &crmv1.SetIamPolicyRequest{Policy: &crmv1.Policy{}}).Do(); httpCode(err) != 403 {
		t.Fatalf("viewer setIamPolicy: want 403, got %v", err)
	}

	// Self-signed JWTs (no token exchange) authenticate too.
	jwtTS, err := google.JWTAccessTokenSourceWithScope(kf, "https://www.googleapis.com/auth/cloud-platform")
	if err != nil {
		t.Fatal(err)
	}
	asJWT := crmClient(t, inst, option.WithTokenSource(jwtTS))
	if _, err := asJWT.Projects.GetIamPolicy(testProject, &crmv1.GetIamPolicyRequest{}).Do(); err != nil {
		t.Fatalf("self-signed JWT: %v", err)
	}

	// Unknown tokens are rejected in enforce mode (FR-CORE-051).
	bogus := crmClient(t, inst, option.WithTokenSource(oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "nope"})))
	if _, err := bogus.Projects.Get(testProject).Do(); httpCode(err) != 401 {
		t.Fatalf("unknown token: want 401, got %v", err)
	}

	// Disabling the account invalidates its tokens.
	if _, err := admin.Projects.ServiceAccounts.Disable("projects/-/serviceAccounts/"+sa.Email, &iamv1.DisableServiceAccountRequest{}).Do(); err != nil {
		t.Fatal(err)
	}
	if _, err := asSA.Projects.GetIamPolicy(testProject, &crmv1.GetIamPolicyRequest{}).Do(); httpCode(err) != 401 {
		t.Fatalf("disabled account: want 401, got %v", err)
	}
}

// TestRefreshTokenGrant covers user ADC (authorized_user credentials).
func TestRefreshTokenGrant(t *testing.T) {
	inst := startIAM(t)
	adc, _ := json.Marshal(map[string]string{
		"type":          "authorized_user",
		"client_id":     "client.apps.googleusercontent.com",
		"client_secret": "secret",
		"refresh_token": "alice@example.com",
		"token_uri":     inst.GatewayURL() + "/oauth2/token",
	})
	creds, err := google.CredentialsFromJSON(context.Background(), adc, "https://www.googleapis.com/auth/cloud-platform")
	if err != nil {
		t.Fatal(err)
	}
	tok, err := creds.TokenSource.Token()
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Get(inst.GatewayURL() + "/oauth2/tokeninfo?access_token=" + url.QueryEscape(tok.AccessToken))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var info map[string]string
	_ = json.NewDecoder(resp.Body).Decode(&info)
	if info["email"] != "alice@example.com" || info["aud"] != "client.apps.googleusercontent.com" {
		t.Fatalf("tokeninfo: %v", info)
	}
}
