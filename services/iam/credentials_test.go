package iam_test

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/http"
	"net/url"
	"testing"

	credentials "cloud.google.com/go/iam/credentials/apiv1"
	"cloud.google.com/go/iam/credentials/apiv1/credentialspb"
	"github.com/golang-jwt/jwt/v5"
	iamv1 "google.golang.org/api/iam/v1"
	iamcredentials "google.golang.org/api/iamcredentials/v1"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
)

func credsClient(t *testing.T, inst *testInstance) *credentials.IamCredentialsClient {
	t.Helper()
	c, err := credentials.NewIamCredentialsClient(context.Background(),
		option.WithEndpoint(inst.Endpoint("gateway")),
		option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func bearer(ctx context.Context, tok string) context.Context {
	return metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+tok)
}

func saName(sa *iamv1.ServiceAccount) string { return "projects/-/serviceAccounts/" + sa.Email }

// TestImpersonation covers FR-IAM-007 over gRPC: token creator checks in
// enforce mode, delegation chains, ID tokens verifiable with the JWKS and
// signBlob verifiable with the account's published certificates.
func TestImpersonation(t *testing.T) {
	inst := startIAM(t, enforce)
	ctx := context.Background()
	admin := iamClient(t, inst)
	caller := createSA(t, admin, "caller-sa")
	middle := createSA(t, admin, "middle-sa")
	target := createSA(t, admin, "target-sa")
	cc := credsClient(t, inst)

	// The default principal may impersonate anything.
	ct, err := cc.GenerateAccessToken(ctx, &credentialspb.GenerateAccessTokenRequest{
		Name: saName(caller), Scope: []string{"https://www.googleapis.com/auth/cloud-platform"}, Lifetime: durationpb.New(600e9),
	})
	if err != nil {
		t.Fatal(err)
	}
	callerCtx := bearer(ctx, ct.AccessToken)

	req := &credentialspb.GenerateAccessTokenRequest{Name: saName(target), Scope: []string{"https://www.googleapis.com/auth/cloud-platform"}}
	if _, err := cc.GenerateAccessToken(callerCtx, req); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("without token creator: want PermissionDenied, got %v", err)
	}
	grant(t, admin, target, "roles/iam.serviceAccountTokenCreator", "serviceAccount:"+caller.Email)
	if _, err := cc.GenerateAccessToken(callerCtx, req); err != nil {
		t.Fatalf("with token creator: %v", err)
	}

	// Delegation: caller → middle → target needs caller on middle too.
	other := createSA(t, admin, "other-target")
	grant(t, admin, other, "roles/iam.serviceAccountTokenCreator", "serviceAccount:"+middle.Email)
	dreq := &credentialspb.GenerateAccessTokenRequest{Name: saName(other), Delegates: []string{saName(middle)}, Scope: []string{"x"}}
	if _, err := cc.GenerateAccessToken(callerCtx, dreq); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("chain without delegation: want PermissionDenied, got %v", err)
	}
	grant(t, admin, middle, "roles/iam.serviceAccountTokenCreator", "serviceAccount:"+caller.Email)
	if _, err := cc.GenerateAccessToken(callerCtx, dreq); err != nil {
		t.Fatalf("chain: %v", err)
	}

	// ID token verifiable against the emulator JWKS.
	idt, err := cc.GenerateIdToken(callerCtx, &credentialspb.GenerateIdTokenRequest{Name: saName(target), Audience: "https://svc.example", IncludeEmail: true})
	if err != nil {
		t.Fatal(err)
	}
	claims := verifyIDToken(t, inst, idt.Token)
	if claims["email"] != target.Email || claims["aud"] != "https://svc.example" || claims["iss"] != "https://accounts.google.com" || claims["sub"] != target.UniqueId {
		t.Fatalf("claims: %v", claims)
	}

	// signBlob verifiable against the published x509 certs.
	sb, err := cc.SignBlob(callerCtx, &credentialspb.SignBlobRequest{Name: saName(target), Payload: []byte("hello")})
	if err != nil {
		t.Fatal(err)
	}
	certs := fetchJSON[map[string]string](t, inst.GatewayURL()+"/service_accounts/v1/metadata/x509/"+url.PathEscape(target.Email))
	blk, _ := pem.Decode([]byte(certs[sb.KeyId]))
	if blk == nil {
		t.Fatalf("no cert for key %s in %v", sb.KeyId, certs)
	}
	cert, _ := x509.ParseCertificate(blk.Bytes)
	sum := sha256.Sum256([]byte("hello"))
	if err := rsa.VerifyPKCS1v15(cert.PublicKey.(*rsa.PublicKey), crypto.SHA256, sum[:], sb.SignedBlob); err != nil {
		t.Fatalf("signature: %v", err)
	}

	sj, err := cc.SignJwt(callerCtx, &credentialspb.SignJwtRequest{Name: saName(target), Payload: `{"sub":"x","aud":"y"}`})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := jwt.Parse(sj.SignedJwt, func(*jwt.Token) (any, error) { return cert.PublicKey, nil }, jwt.WithoutClaimsValidation()); err != nil {
		t.Fatalf("signJwt: %v", err)
	}
}

// TestCredentialsREST exercises the REST transport.
func TestCredentialsREST(t *testing.T) {
	inst := startIAM(t)
	admin := iamClient(t, inst)
	sa := createSA(t, admin, "rest-target")
	svc, err := iamcredentials.NewService(context.Background(), option.WithEndpoint(inst.GatewayURL()+"/iamcredentials/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	at, err := svc.Projects.ServiceAccounts.GenerateAccessToken(saName(sa), &iamcredentials.GenerateAccessTokenRequest{
		Scope: []string{"https://www.googleapis.com/auth/cloud-platform"}, Lifetime: "1200s",
	}).Do()
	if err != nil || at.AccessToken == "" || at.ExpireTime == "" {
		t.Fatalf("generateAccessToken: %v %+v", err, at)
	}
	if _, err := svc.Projects.ServiceAccounts.GenerateAccessToken(saName(sa), &iamcredentials.GenerateAccessTokenRequest{}).Do(); httpCode(err) != 400 {
		t.Fatalf("missing scope: want 400, got %v", err)
	}
	idt, err := svc.Projects.ServiceAccounts.GenerateIdToken(saName(sa), &iamcredentials.GenerateIdTokenRequest{Audience: "aud"}).Do()
	if err != nil {
		t.Fatal(err)
	}
	if c := verifyIDToken(t, inst, idt.Token); c["email"] != nil {
		t.Fatalf("email included without includeEmail: %v", c)
	}
	if _, err := svc.Projects.ServiceAccounts.SignBlob(saName(sa), &iamcredentials.SignBlobRequest{Payload: "aGVsbG8="}).Do(); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Projects.ServiceAccounts.GenerateAccessToken("projects/-/serviceAccounts/missing@test-project.iam.gserviceaccount.com",
		&iamcredentials.GenerateAccessTokenRequest{Scope: []string{"x"}}).Do(); httpCode(err) != 404 {
		t.Fatalf("missing account: want 404, got %v", err)
	}
}

// grant adds a binding to a service account's policy.
func grant(t *testing.T, c *iamv1.Service, sa *iamv1.ServiceAccount, role, member string) {
	t.Helper()
	pol, err := c.Projects.ServiceAccounts.GetIamPolicy(saName(sa)).Do()
	if err != nil {
		t.Fatal(err)
	}
	pol.Bindings = append(pol.Bindings, &iamv1.Binding{Role: role, Members: []string{member}})
	if _, err := c.Projects.ServiceAccounts.SetIamPolicy(saName(sa), &iamv1.SetIamPolicyRequest{Policy: pol}).Do(); err != nil {
		t.Fatal(err)
	}
}

func fetchJSON[T any](t *testing.T, u string) T {
	t.Helper()
	var v T
	resp, err := http.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("GET %s: %s", u, resp.Status)
	}
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

// verifyIDToken checks an ID token against /oauth2/v3/certs.
func verifyIDToken(t *testing.T, inst *testInstance, tok string) jwt.MapClaims {
	t.Helper()
	jwks := fetchJSON[struct {
		Keys []map[string]string `json:"keys"`
	}](t, inst.GatewayURL()+"/oauth2/v3/certs")
	claims := jwt.MapClaims{}
	_, err := jwt.ParseWithClaims(tok, claims, func(tk *jwt.Token) (any, error) {
		for _, k := range jwks.Keys {
			if k["kid"] == tk.Header["kid"] {
				return jwkToRSA(k)
			}
		}
		return nil, fmt.Errorf("kid %v not in JWKS", tk.Header["kid"])
	}, jwt.WithValidMethods([]string{"RS256"}))
	if err != nil {
		t.Fatalf("verify ID token: %v", err)
	}
	return claims
}

func jwkToRSA(k map[string]string) (*rsa.PublicKey, error) {
	n, err := base64.RawURLEncoding.DecodeString(k["n"])
	if err != nil {
		return nil, err
	}
	e, err := base64.RawURLEncoding.DecodeString(k["e"])
	if err != nil {
		return nil, err
	}
	return &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}, nil
}
