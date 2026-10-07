package ar

import (
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"time"

	"github.com/linuxuser586/gcpemu/internal/emu"
)

// FR-AR-003 registry authentication.

// anonymousToken is handed out by the token endpoint to callers without
// credentials when IAM is not enforcing; the gateway middleware attributes
// it to the default principal.
const anonymousToken = "gcpemu-anonymous"

var errBadCredentials = errors.New("invalid username or password")

// accessTokenUsers are the Basic user names whose password is an OAuth2
// access token (gcloud credential helper, docker-credential-gcr, manual
// `docker login -u oauth2accesstoken`).
var accessTokenUsers = map[string]bool{"oauth2accesstoken": true, "_token": true, "_dcgcloud_token": true}

// withRegistryAuth runs before the shared middleware: it validates Basic
// credentials and rewrites them into an equivalent Bearer token so the
// middleware resolves and logs the real principal.
func (s *Service) withRegistryAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if user, pass, ok := r.BasicAuth(); ok {
			tok, err := s.basicToToken(r.Context(), user, pass)
			if err != nil {
				w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
				s.challenge(w, r, "", "")
				return
			}
			r.Header.Set("Authorization", "Bearer "+tok)
		}
		next.ServeHTTP(w, r)
	})
}

// basicToToken validates Basic credentials and returns a bearer token for
// them. Invalid credentials are rejected only when IAM is enforcing;
// otherwise they fall back to anonymous access.
func (s *Service) basicToToken(ctx context.Context, user, pass string) (string, error) {
	switch {
	case accessTokenUsers[user]:
		if _, ok := s.env.Auth.Authenticate(ctx, pass); ok {
			return pass, nil
		}
	case user == "_json_key" || user == "_json_key_base64":
		key := pass
		if user == "_json_key_base64" {
			b, err := base64.StdEncoding.DecodeString(pass)
			if err != nil {
				break
			}
			key = string(b)
		}
		if tok, err := s.keyToken(ctx, key); err == nil {
			return tok, nil
		}
	}
	if s.enforcing() {
		return "", errBadCredentials
	}
	return anonymousToken, nil
}

// keyToken verifies a service account key JSON against the iam service and
// mints an access token for the account.
func (s *Service) keyToken(ctx context.Context, keyJSON string) (string, error) {
	var k struct {
		Type        string `json:"type"`
		ClientEmail string `json:"client_email"`
		PrivateKey  string `json:"private_key"`
	}
	if err := json.Unmarshal([]byte(keyJSON), &k); err != nil || k.ClientEmail == "" || k.PrivateKey == "" {
		return "", errBadCredentials
	}
	svc, ok := s.env.Lookup("iam")
	if !ok {
		return "", errBadCredentials
	}
	keys, ok := svc.(emu.ServiceAccountKeys)
	if !ok {
		return "", errBadCredentials
	}
	priv, err := parseRSAKey(k.PrivateKey)
	if err != nil {
		return "", errBadCredentials
	}
	pubs, err := keys.PublicKeys(ctx, k.ClientEmail)
	if err != nil {
		return "", errBadCredentials
	}
	for _, pub := range pubs {
		if pub.N.Cmp(priv.N) == 0 && pub.E == priv.E {
			tok, _, err := keys.AccessToken(ctx, emu.Principal("serviceAccount:"+k.ClientEmail))
			return tok, err
		}
	}
	return "", errBadCredentials
}

func parseRSAKey(p string) (*rsa.PrivateKey, error) {
	blk, _ := pem.Decode([]byte(p))
	if blk == nil {
		return nil, errBadCredentials
	}
	if k, err := x509.ParsePKCS8PrivateKey(blk.Bytes); err == nil {
		if rk, ok := k.(*rsa.PrivateKey); ok {
			return rk, nil
		}
		return nil, errBadCredentials
	}
	return x509.ParsePKCS1PrivateKey(blk.Bytes)
}

// token implements the registry token endpoint (GET or POST /v2/token).
// Credentials were already validated and rewritten by withRegistryAuth.
func (s *Service) token(w http.ResponseWriter, r *http.Request) {
	tok := ""
	if v, ok := cutBearer(r.Header.Get("Authorization")); ok {
		tok = v
	}
	if r.Method == http.MethodPost && tok == "" {
		_ = r.ParseForm()
		switch r.PostForm.Get("grant_type") {
		case "password":
			t, err := s.basicToToken(r.Context(), r.PostForm.Get("username"), r.PostForm.Get("password"))
			if err != nil {
				writeRegError(w, regErr(http.StatusUnauthorized, "UNAUTHORIZED", "%v", err))
				return
			}
			tok = t
		case "refresh_token":
			tok = r.PostForm.Get("refresh_token")
		}
	}
	if tok == "" {
		if s.enforcing() {
			writeRegError(w, regErr(http.StatusUnauthorized, "UNAUTHORIZED", "authentication required"))
			return
		}
		tok = anonymousToken
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"token":        tok,
		"access_token": tok,
		"expires_in":   3600,
		"issued_at":    s.env.Clock.Now().UTC().Format(time.RFC3339),
	})
}

func cutBearer(h string) (string, bool) {
	for _, p := range []string{"Bearer ", "bearer "} {
		if len(h) > len(p) && h[:len(p)] == p {
			return h[len(p):], true
		}
	}
	return "", false
}
