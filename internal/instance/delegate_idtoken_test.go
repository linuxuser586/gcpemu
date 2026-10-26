package instance

import (
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/golang-jwt/jwt/v5"
)

// idTokenThrough gets an ID token for email from generateIdToken through
// in's API gateway.
func idTokenThrough(t *testing.T, in *Instance, email string) string {
	t.Helper()
	var reply struct{ Token string }
	callAdmin(t, in, http.MethodPost, "/iamcredentials/v1/projects/-/serviceAccounts/"+email+":generateIdToken", map[string]string{"audience": "https://app.example"}, &reply)
	if strings.Count(reply.Token, ".") != 2 {
		t.Fatalf("generateIdToken for %s through %s = %q", email, in.Config.Instance, reply.Token)
	}
	return reply.Token
}

// jwksOf returns the public keys of in's /oauth2/v3/certs, in order, by
// key ID.
func jwksOf(t *testing.T, in *Instance) ([]string, map[string]*rsa.PublicKey) {
	t.Helper()
	code, body := restGet(t, in, http.MethodGet, "/oauth2/v3/certs")
	var set struct{ Keys []struct{ Kid, N, E string } }
	if code != http.StatusOK || json.Unmarshal([]byte(body), &set) != nil {
		t.Fatalf("/oauth2/v3/certs = %d %s", code, body)
	}
	var kids []string
	keys := map[string]*rsa.PublicKey{}
	for _, k := range set.Keys {
		n, errN := base64.RawURLEncoding.DecodeString(k.N)
		e, errE := base64.RawURLEncoding.DecodeString(k.E)
		if errN != nil || errE != nil {
			t.Fatalf("JWK %s: %v %v", k.Kid, errN, errE)
		}
		kids = append(kids, k.Kid)
		keys[k.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
	}
	return kids, keys
}

// certsOf returns the public keys of in's /oauth2/v1/certs, by key ID.
func certsOf(t *testing.T, in *Instance) map[string]*rsa.PublicKey {
	t.Helper()
	code, body := restGet(t, in, http.MethodGet, "/oauth2/v1/certs")
	var certs map[string]string
	if code != http.StatusOK || json.Unmarshal([]byte(body), &certs) != nil {
		t.Fatalf("/oauth2/v1/certs = %d %s", code, body)
	}
	keys := map[string]*rsa.PublicKey{}
	for kid, p := range certs {
		blk, _ := pem.Decode([]byte(p))
		if blk == nil {
			t.Fatalf("certificate %s: no PEM", kid)
		}
		c, err := x509.ParseCertificate(blk.Bytes)
		if err != nil {
			t.Fatalf("certificate %s: %v", kid, err)
		}
		keys[kid] = c.PublicKey.(*rsa.PublicKey)
	}
	return keys
}

// verifies reports whether the ID token tok verifies against keys, by its
// kid, as a client library verifying it would.
func verifies(tok string, keys map[string]*rsa.PublicKey) error {
	_, err := jwt.Parse(tok, func(tk *jwt.Token) (any, error) {
		kid, _ := tk.Header["kid"].(string)
		if k, ok := keys[kid]; ok {
			return k, nil
		}
		return nil, jwt.ErrTokenUnverifiable
	}, jwt.WithValidMethods([]string{"RS256"}))
	return err
}

// tokenInfo asks in's tokeninfo about the ID token tok.
func tokenInfo(t *testing.T, in *Instance, tok string) (int, string) {
	t.Helper()
	return restGet(t, in, http.MethodGet, "/oauth2/tokeninfo?id_token="+url.QueryEscape(tok))
}

// An ID token the office issues for a service account in the Delegated
// Project, through the laptop, verifies against the laptop's certificate
// endpoints and tokeninfo, as the laptop's own ID tokens still do, and
// still does while the office is down (#693).
func TestDelegatedIDTokenVerifiesThroughFrontInstance(t *testing.T) {
	laptop, office := delegated(t, nil, nil)
	callAdmin(t, laptop, http.MethodPost, "/iam/v1/projects/"+shared+"/serviceAccounts", map[string]string{"accountId": "runner"}, nil)
	callAdmin(t, laptop, http.MethodPost, "/iam/v1/projects/laptop-own/serviceAccounts", map[string]string{"accountId": "local-runner"}, nil)
	delegatedTok := idTokenThrough(t, laptop, runner)
	ownTok := idTokenThrough(t, laptop, "local-runner@laptop-own.iam.gserviceaccount.com")

	check := func(when string) {
		t.Helper()
		kids, jwks := jwksOf(t, laptop)
		if len(kids) != 2 {
			t.Errorf("%s: /oauth2/v3/certs lists %v, want the laptop's key and the office's", when, kids)
		}
		for name, tok := range map[string]string{"delegated": delegatedTok, "own": ownTok} {
			if err := verifies(tok, jwks); err != nil {
				t.Errorf("%s: the %s ID token against /oauth2/v3/certs: %v", when, name, err)
			}
			if err := verifies(tok, certsOf(t, laptop)); err != nil {
				t.Errorf("%s: the %s ID token against /oauth2/v1/certs: %v", when, name, err)
			}
			if code, body := tokenInfo(t, laptop, tok); code != http.StatusOK || !strings.Contains(body, `"aud":"https://app.example"`) {
				t.Errorf("%s: tokeninfo of the %s ID token = %d %s", when, name, code, body)
			}
		}
		ownKid, _ := jwt.NewParser().Parse(ownTok, nil)
		if len(kids) > 0 && ownKid != nil && kids[0] != ownKid.Header["kid"] {
			t.Errorf("%s: /oauth2/v3/certs lists %v, want the laptop's own key %v first", when, kids, ownKid.Header["kid"])
		}
	}
	check("with the office up")
	if err := office.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	check("with the office down")

	callAdmin(t, laptop, http.MethodDelete, "/_emu/v1/links/delegations/"+shared, nil, nil)
	if kids, _ := jwksOf(t, laptop); len(kids) != 1 {
		t.Errorf("/oauth2/v3/certs after undelegating lists %v, want the laptop's own key only", kids)
	}
	if code, _ := tokenInfo(t, laptop, delegatedTok); code != http.StatusBadRequest {
		t.Errorf("tokeninfo of the office's ID token after undelegating = %d, want 400", code)
	}
}
