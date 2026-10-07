package iam

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"google.golang.org/api/googleapi"
	iamv1 "google.golang.org/api/iam/v1"
	stsv1 "google.golang.org/api/sts/v1"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/project"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// Workload Identity Federation (FR-IAM-009): workload identity pools and
// OIDC providers (iam v1) and the STS token exchange.

const (
	nsPools     = "iam/wipools"      // pool name → iamv1.WorkloadIdentityPool
	nsProviders = "iam/wiproviders"  // provider name → iamv1.WorkloadIdentityPoolProvider
	nsFederated = "iam/wif-subjects" // principal → federatedSubject

	tokenExchangeGrant = "urn:ietf:params:oauth:grant-type:token-exchange"
	accessTokenType    = "urn:ietf:params:oauth:token-type:access_token"
)

// federatedSubject records the attributes of a federated principal so
// principalSet members can match on them.
type federatedSubject struct {
	Pool       string            `json:"pool"`
	Attributes map[string]string `json:"attributes"`
}

// --- iam v1 pools and providers ---

func (s *Service) routeWorkloadIdentity(w http.ResponseWriter, r *http.Request, rt route) error {
	// v1/projects/{p}/locations/global/workloadIdentityPools[/{pool}[/providers[/{prov}]]][/operations/{op}]
	segs := rt.segs
	if len(segs) < 6 || segs[4] != "global" || segs[5] != "workloadIdentityPools" {
		notFoundRoute(w, r)
		return nil
	}
	var pid string
	_ = s.env.Store.View(func(tx store.Tx) error { pid = s.projectID(tx, segs[2]); return nil })
	if err := s.env.EnsureProject(pid); err != nil {
		return err
	}
	base := "projects/" + project.NumberString(pid) + "/locations/global/workloadIdentityPools"
	res := projectResource(pid)
	ctx := r.Context()
	rest := segs[6:]
	// Operations are complete on return; GET of one echoes done.
	if n := len(rest); n >= 2 && rest[n-2] == "operations" {
		writeJSON(w, &iamv1.Operation{Name: strings.Join(segs[1:], "/"), Done: true})
		return nil
	}
	switch {
	case len(rest) == 0 && rt.verb == "":
		switch r.Method {
		case http.MethodGet:
			return s.listWIF(w, r, ctx, res, nsPools, base+"/", "iam.workloadIdentityPools.list")
		case http.MethodPost:
			var in iamv1.WorkloadIdentityPool
			if err := readJSON(r, &in); err != nil {
				return err
			}
			id := r.URL.Query().Get("workloadIdentityPoolId")
			return s.createWIF(w, ctx, res, nsPools, base+"/"+id, id, "iam.workloadIdentityPools.create", func(name string) any {
				in.Name, in.State = name, "ACTIVE"
				return &in
			})
		}
	case len(rest) == 1:
		return s.wifItem(w, r, ctx, res, nsPools, base+"/"+rest[0], rt.verb, "iam.workloadIdentityPools.", &iamv1.WorkloadIdentityPool{})
	case len(rest) == 2 && rest[1] == "providers" && rt.verb == "":
		parent := base + "/" + rest[0]
		switch r.Method {
		case http.MethodGet:
			return s.listWIF(w, r, ctx, res, nsProviders, parent+"/providers/", "iam.workloadIdentityPoolProviders.list")
		case http.MethodPost:
			var in iamv1.WorkloadIdentityPoolProvider
			if err := readJSON(r, &in); err != nil {
				return err
			}
			if in.Oidc == nil {
				return apierr.Unimplemented("Only OIDC providers are supported by the emulator.")
			}
			if in.Oidc.IssuerUri == "" {
				return apierr.InvalidArgument("oidc.issuerUri is required.")
			}
			if in.AttributeMapping == nil {
				in.AttributeMapping = map[string]string{"google.subject": "assertion.sub"}
			}
			if err := validateMapping(&in); err != nil {
				return err
			}
			var exists bool
			_ = s.env.Store.View(func(tx store.Tx) error { exists = store.Exists(tx, nsPools, parent); return nil })
			if !exists {
				return apierr.NotFound("Requested entity was not found.")
			}
			id := r.URL.Query().Get("workloadIdentityPoolProviderId")
			return s.createWIF(w, ctx, res, nsProviders, parent+"/providers/"+id, id, "iam.workloadIdentityPoolProviders.create", func(name string) any {
				in.Name, in.State = name, "ACTIVE"
				return &in
			})
		}
	case len(rest) == 3 && rest[1] == "providers":
		return s.wifItem(w, r, ctx, res, nsProviders, base+"/"+rest[0]+"/providers/"+rest[2], rt.verb, "iam.workloadIdentityPoolProviders.", &iamv1.WorkloadIdentityPoolProvider{})
	}
	notFoundRoute(w, r)
	return nil
}

var wifIDRE = regexp.MustCompile(`^[a-z0-9-]{4,32}$`)

// doneOp wraps a resource in a completed LRO.
func (s *Service) doneOp(name string, v any) *iamv1.Operation {
	b, _ := json.Marshal(v)
	return &iamv1.Operation{Name: name + "/operations/" + s.env.IDs.Hex(8), Done: true, Response: googleapi.RawMessage(b)}
}

func (s *Service) createWIF(w http.ResponseWriter, ctx context.Context, res, ns, name, id, perm string, build func(string) any) error {
	if err := s.check(ctx, perm, res); err != nil {
		return err
	}
	if !wifIDRE.MatchString(id) || strings.HasPrefix(id, "gcp-") {
		return apierr.InvalidArgument("Invalid ID %q: it must be 4 to 32 lowercase letters, digits or hyphens.", id)
	}
	v := build(name)
	err := s.env.Store.Update(func(tx store.Tx) error {
		if store.Exists(tx, ns, name) {
			return apierr.AlreadyExists("Requested entity already exists.")
		}
		return store.PutJSON(tx, ns, name, v)
	})
	if err != nil {
		return err
	}
	writeJSON(w, s.doneOp(name, v))
	return nil
}

func (s *Service) listWIF(w http.ResponseWriter, r *http.Request, ctx context.Context, res, ns, prefix, perm string) error {
	if err := s.check(ctx, perm, res); err != nil {
		return err
	}
	showDeleted := r.URL.Query().Get("showDeleted") == "true"
	var items []json.RawMessage
	_ = s.env.Store.View(func(tx store.Tx) error {
		tx.Scan(ns, prefix, func(k string, b []byte) bool {
			if strings.Contains(strings.TrimPrefix(k, prefix), "/") {
				return true
			}
			var st struct{ State string }
			_ = json.Unmarshal(b, &st)
			if st.State != "DELETED" || showDeleted {
				items = append(items, append(json.RawMessage(nil), b...))
			}
			return true
		})
		return nil
	})
	key := "workloadIdentityPools"
	if ns == nsProviders {
		key = "workloadIdentityPoolProviders"
	}
	writeJSON(w, map[string]any{key: items})
	return nil
}

// wifItem serves get/patch/delete/undelete of a pool or provider.
func (s *Service) wifItem(w http.ResponseWriter, r *http.Request, ctx context.Context, res, ns, name, verb, permPrefix string, v any) error {
	var perm string
	switch {
	case verb == "" && r.Method == http.MethodGet:
		perm = "get"
	case verb == "" && r.Method == http.MethodPatch:
		perm = "update"
	case verb == "" && r.Method == http.MethodDelete:
		perm = "delete"
	case verb == "undelete" && r.Method == http.MethodPost:
		perm = "undelete"
	default:
		notFoundRoute(w, r)
		return nil
	}
	if err := s.check(ctx, permPrefix+perm, res); err != nil {
		return err
	}
	var raw []byte
	var found bool
	_ = s.env.Store.View(func(tx store.Tx) error { raw, found = tx.Get(ns, name); return nil })
	if !found {
		return apierr.NotFound("Requested entity was not found.")
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return err
	}
	if perm == "get" {
		writeJSON(w, v)
		return nil
	}
	var patch map[string]json.RawMessage
	if perm == "update" {
		if err := readJSON(r, &patch); err != nil {
			return err
		}
	}
	var cur map[string]json.RawMessage
	_ = json.Unmarshal(raw, &cur)
	switch perm {
	case "update":
		mask := r.URL.Query().Get("updateMask")
		if mask == "" {
			return apierr.InvalidArgument("updateMask is required.")
		}
		for _, f := range strings.Split(mask, ",") {
			f = strings.TrimSpace(f)
			top, _, _ := strings.Cut(f, ".")
			if val, ok := patch[top]; ok {
				cur[top] = val
			} else {
				delete(cur, top)
			}
		}
	case "delete":
		cur["state"], _ = json.Marshal("DELETED")
		cur["expireTime"], _ = json.Marshal(s.env.Clock.Now().Add(30 * 24 * time.Hour).UTC().Format(time.RFC3339))
	case "undelete":
		cur["state"], _ = json.Marshal("ACTIVE")
		delete(cur, "expireTime")
	}
	b, _ := json.Marshal(cur)
	if ns == nsProviders {
		var p iamv1.WorkloadIdentityPoolProvider
		if err := json.Unmarshal(b, &p); err == nil {
			if err := validateMapping(&p); err != nil {
				return err
			}
		}
	}
	if err := s.env.Store.Update(func(tx store.Tx) error { return tx.Put(ns, name, b) }); err != nil {
		return err
	}
	_ = json.Unmarshal(b, v)
	writeJSON(w, s.doneOp(name, v))
	return nil
}

func validateMapping(p *iamv1.WorkloadIdentityPoolProvider) error {
	if _, ok := p.AttributeMapping["google.subject"]; !ok {
		return apierr.InvalidArgument("attributeMapping must contain google.subject.")
	}
	for k, expr := range p.AttributeMapping {
		if !strings.HasPrefix(k, "google.") && !strings.HasPrefix(k, "attribute.") {
			return apierr.InvalidArgument("Invalid attribute mapping key %q.", k)
		}
		if _, err := parseCond(expr); err != nil {
			return apierr.InvalidArgument("Invalid attribute mapping %q: %v", expr, err)
		}
	}
	if p.AttributeCondition != "" {
		if _, err := parseCond(p.AttributeCondition); err != nil {
			return apierr.InvalidArgument("Invalid attribute condition: %v", err)
		}
	}
	return nil
}

// --- STS ---

// serveSTS serves sts.googleapis.com v1 token exchange.
func (s *Service) serveSTS(w http.ResponseWriter, r *http.Request) {
	switch strings.TrimSuffix(r.URL.Path, "/") {
	case "/v1/token", "/v1beta/token":
	default:
		notFoundRoute(w, r)
		return
	}
	if r.Method != http.MethodPost {
		notFoundRoute(w, r)
		return
	}
	form, err := tokenForm(r)
	if err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	// JSON bodies use camelCase field names.
	for camelKey, snake := range map[string]string{"grantType": "grant_type", "subjectToken": "subject_token", "subjectTokenType": "subject_token_type", "requestedTokenType": "requested_token_type"} {
		if v := form.Get(camelKey); v != "" && form.Get(snake) == "" {
			form.Set(snake, v)
		}
	}
	s.tokenExchange(w, form)
}

// tokenExchange implements RFC 8693 exchange of an OIDC token for a
// federated access token.
func (s *Service) tokenExchange(w http.ResponseWriter, form url.Values) {
	if form.Get("grant_type") != tokenExchangeGrant {
		oauthError(w, http.StatusBadRequest, "unsupported_grant_type", "grant_type must be "+tokenExchangeGrant)
		return
	}
	switch form.Get("subject_token_type") {
	case "urn:ietf:params:oauth:token-type:jwt", "urn:ietf:params:oauth:token-type:id_token":
	default:
		oauthError(w, http.StatusBadRequest, "invalid_request", "Unsupported subject_token_type: "+form.Get("subject_token_type"))
		return
	}
	if rt := form.Get("requested_token_type"); rt != "" && rt != accessTokenType {
		oauthError(w, http.StatusBadRequest, "invalid_request", "Unsupported requested_token_type: "+rt)
		return
	}
	principal, attrs, pool, err := s.federate(form.Get("audience"), form.Get("subject_token"))
	if err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_grant", err.Error())
		return
	}
	err = s.env.Store.Update(func(tx store.Tx) error {
		return store.PutJSON(tx, nsFederated, string(principal), federatedSubject{Pool: pool, Attributes: attrs})
	})
	if err != nil {
		oauthError(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	scopes := strings.Fields(form.Get("scope"))
	tok, exp, err := s.mintAccessToken(principal, scopes, "", tokenLifetime)
	if err != nil {
		oauthError(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	writeJSON(w, &stsv1.GoogleIdentityStsV1ExchangeTokenResponse{
		AccessToken: tok, IssuedTokenType: accessTokenType, TokenType: "Bearer", ExpiresIn: int64(s.expiresIn(exp)),
	})
}

// federate verifies subjectToken for the provider named by audience
// ("//iam.googleapis.com/projects/N/locations/global/workloadIdentityPools/P/providers/X")
// and returns the federated principal and its attributes.
func (s *Service) federate(audience, subjectToken string) (emu.Principal, map[string]string, string, error) {
	provName := strings.TrimPrefix(strings.TrimPrefix(audience, "https:"), "//iam.googleapis.com/")
	poolName, _, ok := strings.Cut(provName, "/providers/")
	if !ok {
		return "", nil, "", fmt.Errorf("invalid audience %q", audience)
	}
	var prov iamv1.WorkloadIdentityPoolProvider
	var pool iamv1.WorkloadIdentityPool
	var err error
	_ = s.env.Store.View(func(tx store.Tx) error {
		if store.GetJSON(tx, nsProviders, provName, &prov) != nil || store.GetJSON(tx, nsPools, poolName, &pool) != nil {
			err = fmt.Errorf("the audience %q does not match a workload identity pool provider", audience)
		}
		return nil
	})
	if err != nil {
		return "", nil, "", err
	}
	if prov.State != "ACTIVE" || prov.Disabled || pool.State != "ACTIVE" || pool.Disabled {
		return "", nil, "", errors.New("the workload identity pool or provider is disabled or deleted")
	}
	claims := jwt.MapClaims{}
	_, err = jwt.ParseWithClaims(subjectToken, claims, func(t *jwt.Token) (any, error) {
		kid, _ := t.Header["kid"].(string)
		return s.issuerKey(prov.Oidc, kid)
	}, jwt.WithValidMethods([]string{"RS256", "RS384", "RS512"}), jwt.WithTimeFunc(s.env.Clock.Now),
		jwt.WithIssuer(prov.Oidc.IssuerUri), jwt.WithExpirationRequired())
	if err != nil {
		return "", nil, "", fmt.Errorf("invalid subject token: %v", err)
	}
	auds, _ := claims.GetAudience()
	allowed := prov.Oidc.AllowedAudiences
	if len(allowed) == 0 {
		allowed = []string{"https://iam.googleapis.com/" + provName, "//iam.googleapis.com/" + provName}
	}
	if !intersects(auds, allowed) {
		return "", nil, "", fmt.Errorf("the audience in the subject token %v does not match the allowed audiences %v", auds, allowed)
	}
	assertion := map[string]any(claims)
	env := condEnv{now: s.env.Clock.Now(), vars: map[string]any{"assertion": assertion}}
	attrs := map[string]string{}
	google := map[string]any{}
	attrVals := map[string]any{}
	for k, expr := range prov.AttributeMapping {
		n, err := parseCond(expr)
		if err != nil {
			return "", nil, "", err
		}
		v, err := n.eval(env)
		if err != nil {
			return "", nil, "", fmt.Errorf("attribute mapping %s: %v", k, err)
		}
		str, ok := v.(string)
		if !ok {
			continue
		}
		if a, ok := strings.CutPrefix(k, "attribute."); ok {
			attrs[a], attrVals[a] = str, str
		} else if g, ok := strings.CutPrefix(k, "google."); ok {
			google[g] = str
		}
	}
	sub, _ := google["subject"].(string)
	if sub == "" || len(sub) > 127 {
		return "", nil, "", errors.New("the mapped google.subject must be 1 to 127 characters")
	}
	if prov.AttributeCondition != "" {
		env.vars["attribute"], env.vars["google"] = attrVals, google
		n, _ := parseCond(prov.AttributeCondition)
		v, err := n.eval(env)
		if b, ok := v.(bool); err != nil || !ok || !b {
			return "", nil, "", errors.New("the given credential is rejected by the attribute condition")
		}
	}
	return emu.Principal("principal://iam.googleapis.com/" + poolName + "/subject/" + sub), attrs, poolName, nil
}

func intersects(a, b []string) bool {
	for _, x := range a {
		for _, y := range b {
			if x == y {
				return true
			}
		}
	}
	return false
}

// jwksCache caches issuer key sets fetched by discovery.
var jwksCache sync.Map // issuer → cachedJWKS

type cachedJWKS struct {
	keys    map[string]*rsa.PublicKey
	fetched time.Time
}

// issuerKey finds the verification key for an OIDC provider: inline
// jwksJson, the emulator's own key for its issuer, or the issuer's
// published JWKS (not in --offline mode).
func (s *Service) issuerKey(oidc *iamv1.Oidc, kid string) (*rsa.PublicKey, error) {
	if oidc.JwksJson != "" {
		keys, err := parseJWKS([]byte(oidc.JwksJson))
		if err != nil {
			return nil, err
		}
		return pickKey(keys, kid)
	}
	if oidc.IssuerUri == oidcIssuer {
		return &s.signer.priv.PublicKey, nil
	}
	if c, ok := jwksCache.Load(oidc.IssuerUri); ok && time.Since(c.(cachedJWKS).fetched) < 10*time.Minute {
		return pickKey(c.(cachedJWKS).keys, kid)
	}
	if s.env.Config.Offline {
		return nil, errors.New("cannot fetch issuer keys in offline mode; set oidc.jwksJson on the provider")
	}
	keys, err := fetchJWKS(oidc.IssuerUri)
	if err != nil {
		return nil, err
	}
	jwksCache.Store(oidc.IssuerUri, cachedJWKS{keys: keys, fetched: time.Now()})
	return pickKey(keys, kid)
}

func pickKey(keys map[string]*rsa.PublicKey, kid string) (*rsa.PublicKey, error) {
	if k, ok := keys[kid]; ok {
		return k, nil
	}
	if kid == "" && len(keys) == 1 {
		for _, k := range keys {
			return k, nil
		}
	}
	return nil, fmt.Errorf("no key %q in issuer JWKS", kid)
}

func parseJWKS(b []byte) (map[string]*rsa.PublicKey, error) {
	var set struct {
		Keys []struct {
			Kty, Kid, N, E string
		} `json:"keys"`
	}
	if err := json.Unmarshal(b, &set); err != nil {
		return nil, err
	}
	out := map[string]*rsa.PublicKey{}
	for _, k := range set.Keys {
		if k.Kty != "RSA" {
			continue
		}
		n, err1 := base64.RawURLEncoding.DecodeString(k.N)
		e, err2 := base64.RawURLEncoding.DecodeString(k.E)
		if err1 != nil || err2 != nil {
			continue
		}
		out[k.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
	}
	return out, nil
}

func fetchJWKS(issuer string) (map[string]*rsa.PublicKey, error) {
	cl := &http.Client{Timeout: 10 * time.Second}
	get := func(u string) ([]byte, error) {
		resp, err := cl.Get(u)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("GET %s: %s", u, resp.Status)
		}
		return io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	}
	b, err := get(strings.TrimSuffix(issuer, "/") + "/.well-known/openid-configuration")
	if err != nil {
		return nil, err
	}
	var disc struct {
		JwksURI string `json:"jwks_uri"`
	}
	if err := json.Unmarshal(b, &disc); err != nil || disc.JwksURI == "" {
		return nil, fmt.Errorf("issuer %s: no jwks_uri", issuer)
	}
	if b, err = get(disc.JwksURI); err != nil {
		return nil, err
	}
	return parseJWKS(b)
}

// principalSetMatches matches principalSet members
// ("//iam.googleapis.com/projects/N/locations/global/workloadIdentityPools/P/<selector>")
// against a federated principal.
func (s *Service) principalSetMatches(tx store.Tx, set string, p emu.Principal) bool {
	rest, ok := strings.CutPrefix(set, "//iam.googleapis.com/")
	if !ok {
		return false
	}
	poolName, sel, ok := strings.Cut(rest, "/")
	if i := strings.Index(rest, "/workloadIdentityPools/"); i >= 0 {
		after := rest[i+len("/workloadIdentityPools/"):]
		pool, s2, ok2 := strings.Cut(after, "/")
		poolName, sel, ok = rest[:i]+"/workloadIdentityPools/"+pool, s2, ok2
	}
	if !ok {
		return false
	}
	var fs federatedSubject
	if store.GetJSON(tx, nsFederated, string(p), &fs) != nil || fs.Pool != poolName {
		return false
	}
	switch {
	case sel == "*":
		return true
	case strings.HasPrefix(sel, "attribute."):
		name, val, _ := strings.Cut(strings.TrimPrefix(sel, "attribute."), "/")
		return fs.Attributes[name] == val
	}
	return false
}
