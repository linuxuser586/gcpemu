package iam

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// OAuth2 token endpoint and OIDC discovery (FR-IAM-006, FR-CORE-052).
// Served under /oauth2/ on the gateway and at the root of
// oauth2.googleapis.com / accounts.google.com in host mode.

// tokenResponse is the OAuth2 token endpoint response.
type tokenResponse struct {
	AccessToken string `json:"access_token,omitempty"`
	ExpiresIn   int    `json:"expires_in,omitempty"`
	Scope       string `json:"scope,omitempty"`
	TokenType   string `json:"token_type,omitempty"`
	IDToken     string `json:"id_token,omitempty"`
	// STS token exchange fields (RFC 8693).
	IssuedTokenType string `json:"issued_token_type,omitempty"`
}

// oauthError writes an RFC 6749 error response.
func oauthError(w http.ResponseWriter, code int, kind, desc string) {
	writeJSONStatus(w, code, map[string]string{"error": kind, "error_description": desc})
}

func (s *Service) serveOAuth2(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimSuffix(r.URL.Path, "/")
	switch path {
	case "/token", "/o/oauth2/token", "/oauth2/v4/token":
		s.serveToken(w, r)
	case "/tokeninfo", "/v1/tokeninfo", "/v3/tokeninfo", "/oauth2/v3/tokeninfo":
		s.serveTokenInfo(w, r)
	case "/v3/certs", "/oauth2/v3/certs":
		writeJSON(w, s.signer.jwks())
	case "/v1/certs", "/oauth2/v1/certs":
		writeJSON(w, map[string]string{s.signer.kid: s.signer.certPEM})
	case "/.well-known/openid-configuration":
		s.serveDiscovery(w, r)
	case "/userinfo", "/v1/userinfo", "/v2/userinfo", "/v3/userinfo", "/oauth2/v3/userinfo":
		p := emu.PrincipalFrom(r.Context())
		writeJSON(w, map[string]any{"sub": uniqueIDFor(p.Email()), "email": p.Email(), "email_verified": true})
	case "/revoke", "/o/oauth2/revoke":
		_ = r.ParseForm()
		if t := r.Form.Get("token"); t != "" {
			_ = s.env.Store.Update(func(tx store.Tx) error { return tx.Delete(nsTokens, t) })
		}
		writeJSON(w, struct{}{})
	default:
		notFoundRoute(w, r)
	}
}

// serveDiscovery serves the OIDC discovery document for emulator ID tokens.
func (s *Service) serveDiscovery(w http.ResponseWriter, r *http.Request) {
	gw := s.gatewayURL()
	writeJSON(w, map[string]any{
		"issuer":                                oidcIssuer,
		"authorization_endpoint":                gw + "/oauth2/auth",
		"token_endpoint":                        s.tokenURI(),
		"userinfo_endpoint":                     gw + "/oauth2/v3/userinfo",
		"revocation_endpoint":                   gw + "/oauth2/revoke",
		"jwks_uri":                              gw + "/oauth2/v3/certs",
		"response_types_supported":              []string{"code", "token", "id_token"},
		"subject_types_supported":               []string{"public"},
		"id_token_signing_alg_values_supported": []string{"RS256"},
		"scopes_supported":                      []string{"openid", "email", "profile"},
		"token_endpoint_auth_methods_supported": []string{"client_secret_post", "client_secret_basic"},
		"claims_supported":                      []string{"aud", "email", "email_verified", "exp", "iat", "iss", "sub", "azp"},
		"grant_types_supported": []string{
			"refresh_token", "urn:ietf:params:oauth:grant-type:jwt-bearer",
			"urn:ietf:params:oauth:grant-type:token-exchange",
		},
	})
}

// serveToken implements the JWT-bearer (service-account key) and
// refresh-token (user ADC) grants, plus STS token exchange.
func (s *Service) serveToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		oauthError(w, http.StatusMethodNotAllowed, "invalid_request", "POST required")
		return
	}
	form, err := tokenForm(r)
	if err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	switch form.Get("grant_type") {
	case "urn:ietf:params:oauth:grant-type:jwt-bearer":
		s.jwtBearerGrant(w, form)
	case "refresh_token":
		s.refreshGrant(w, form)
	case "urn:ietf:params:oauth:grant-type:token-exchange":
		s.tokenExchange(w, form)
	case "":
		oauthError(w, http.StatusBadRequest, "invalid_request", "Missing required parameter: grant_type")
	default:
		oauthError(w, http.StatusBadRequest, "unsupported_grant_type", "Invalid grant_type: "+form.Get("grant_type"))
	}
}

// tokenForm reads form or JSON token requests.
func tokenForm(r *http.Request) (url.Values, error) {
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		var m map[string]any
		if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
			return nil, err
		}
		v := url.Values{}
		for k, x := range m {
			switch t := x.(type) {
			case string:
				v.Set(k, t)
			default:
				b, _ := json.Marshal(t)
				v.Set(k, string(b))
			}
		}
		return v, nil
	}
	if err := r.ParseForm(); err != nil {
		return nil, err
	}
	return r.Form, nil
}

func (s *Service) jwtBearerGrant(w http.ResponseWriter, form url.Values) {
	assertion := form.Get("assertion")
	email, err := s.verifyAccountJWT(assertion, "")
	if err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_grant", "Invalid JWT Signature. "+err.Error())
		return
	}
	claims := jwt.MapClaims{}
	_, _, _ = jwt.NewParser().ParseUnverified(assertion, claims)
	if aud, _ := claims["target_audience"].(string); aud != "" {
		tok, err := s.mintIDToken(email, aud, true, nil)
		if err != nil {
			oauthError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		writeJSON(w, tokenResponse{IDToken: tok})
		return
	}
	scope, _ := claims["scope"].(string)
	scopes := strings.Fields(scope)
	tok, exp, err := s.mintAccessToken(emu.Principal("serviceAccount:"+email), scopes, "", tokenLifetime)
	if err != nil {
		oauthError(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	writeJSON(w, tokenResponse{AccessToken: tok, ExpiresIn: s.expiresIn(exp), TokenType: "Bearer"})
}

func (s *Service) expiresIn(exp time.Time) int {
	return max(0, int(exp.Sub(s.env.Clock.Now()).Seconds())-1)
}

// refreshPrincipal maps a user ADC refresh token to a principal: a token
// of the form "user:EMAIL" or "serviceAccount:EMAIL" names the principal,
// one containing "@" is a user email, anything else is the default
// principal (FR-CORE-050).
func (s *Service) refreshPrincipal(rt string) emu.Principal {
	switch {
	case strings.HasPrefix(rt, "user:"), strings.HasPrefix(rt, "serviceAccount:"):
		return emu.Principal(rt)
	case strings.Contains(rt, "@"):
		return emu.Principal("user:" + rt)
	}
	return emu.Principal(s.env.Config.DefaultPrincipal)
}

func (s *Service) refreshGrant(w http.ResponseWriter, form url.Values) {
	rt := form.Get("refresh_token")
	if rt == "" {
		oauthError(w, http.StatusBadRequest, "invalid_request", "Missing required parameter: refresh_token")
		return
	}
	p := s.refreshPrincipal(rt)
	scopes := strings.Fields(form.Get("scope"))
	if len(scopes) == 0 {
		scopes = []string{cloudPlatformScope, "openid", "https://www.googleapis.com/auth/userinfo.email"}
	}
	clientID := form.Get("client_id")
	tok, exp, err := s.mintAccessToken(p, scopes, clientID, tokenLifetime)
	if err != nil {
		oauthError(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	resp := tokenResponse{AccessToken: tok, ExpiresIn: s.expiresIn(exp), Scope: strings.Join(scopes, " "), TokenType: "Bearer"}
	aud := clientID
	if aud == "" {
		aud = "gcpemu"
	}
	resp.IDToken, _ = s.mintIDToken(p.Email(), aud, true, nil)
	writeJSON(w, resp)
}

// serveTokenInfo describes an access token or verifies an ID token.
func (s *Service) serveTokenInfo(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	if idt := r.Form.Get("id_token"); idt != "" {
		claims := jwt.MapClaims{}
		_, err := jwt.ParseWithClaims(idt, claims, func(*jwt.Token) (any, error) { return &s.signer.priv.PublicKey, nil },
			jwt.WithValidMethods([]string{"RS256"}), jwt.WithTimeFunc(s.env.Clock.Now))
		if err != nil {
			oauthError(w, http.StatusBadRequest, "invalid_token", "Invalid Value")
			return
		}
		out := map[string]string{}
		for k, v := range claims {
			switch t := v.(type) {
			case string:
				out[k] = t
			case float64:
				out[k] = strconv.FormatInt(int64(t), 10)
			case bool:
				out[k] = strconv.FormatBool(t)
			}
		}
		writeJSON(w, out)
		return
	}
	tok := r.Form.Get("access_token")
	if tok == "" {
		tok, _ = strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	}
	rec, ok := s.lookupToken(tok)
	if !ok {
		oauthError(w, http.StatusBadRequest, "invalid_token", "Invalid Value")
		return
	}
	p := emu.Principal(rec.Principal)
	azp := rec.ClientID
	if azp == "" {
		azp = uniqueIDFor(p.Email())
	}
	writeJSON(w, map[string]string{
		"azp":            azp,
		"aud":            azp,
		"sub":            uniqueIDFor(p.Email()),
		"scope":          strings.Join(rec.Scopes, " "),
		"exp":            strconv.FormatInt(rec.Expiry.Unix(), 10),
		"expires_in":     strconv.Itoa(s.expiresIn(rec.Expiry)),
		"email":          p.Email(),
		"email_verified": "true",
		"access_type":    "online",
	})
}

// serveX509 serves a service account's public certificates (the
// client_x509_cert_url of key files) as x509 PEM or JWK sets.
func (s *Service) serveX509(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/service_accounts/v1/metadata/")
	kind, email, ok := strings.Cut(rest, "/")
	if !ok || (kind != "x509" && kind != "jwk") {
		notFoundRoute(w, r)
		return
	}
	var keys []*keyRecord
	var found bool
	_ = s.env.Store.View(func(tx store.Tx) error {
		if sa, ok := s.getAccount(tx, email); ok {
			found = true
			keys = listKeys(tx, sa.Email)
		}
		return nil
	})
	if !found {
		notFoundRoute(w, r)
		return
	}
	if kind == "x509" {
		out := map[string]string{}
		for _, k := range keys {
			if !k.Disabled {
				out[k.KeyID] = k.CertPEM
			}
		}
		writeJSON(w, out)
		return
	}
	var jwks []map[string]string
	for _, k := range keys {
		pub, err := k.publicKey()
		if err != nil || k.Disabled {
			continue
		}
		jwks = append(jwks, map[string]string{
			"kty": "RSA", "alg": "RS256", "use": "sig", "kid": k.KeyID,
			"n": b64url(pub.N.Bytes()), "e": b64url(bigEndian(pub.E)),
		})
	}
	writeJSON(w, map[string]any{"keys": jwks})
}
