package gcs

import (
	"context"
	"crypto"
	"crypto/hmac"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/linuxuser586/gcpemu/internal/emu"
)

// V4 signatures (FR-GCS-006): signed URLs (query string), Authorization
// header signatures and POST policies, with RSA keys of emulator service
// accounts (from the iam service) or HMAC keys (FR-GCS-005).

const (
	maxSignedExpiry = 7 * 24 * 3600
	iso8601         = "20060102T150405Z"
)

// sigError is an XML API authentication failure.
type sigError struct {
	status  int
	code    string
	message string
	details string
}

func (e *sigError) Error() string { return e.code + ": " + e.details }

func errSignature(details string) *sigError {
	return &sigError{http.StatusForbidden, "SignatureDoesNotMatch",
		"Access denied.", details}
}

func errExpired(details string) *sigError {
	return &sigError{http.StatusBadRequest, "ExpiredToken", "The provided token has expired.", details}
}

func errMalformed(details string) *sigError {
	return &sigError{http.StatusBadRequest, "AuthorizationQueryParametersError", "Error parsing the X-Goog-Credential parameter; the credential scope is malformed.", details}
}

// sigParams are the parsed parts of a V4 signature.
type sigParams struct {
	algorithm     string // GOOG4-RSA-SHA256, GOOG4-HMAC-SHA256, AWS4-HMAC-SHA256
	credential    string // id/date/region/service/request
	date          string // yyyymmddThhmmssZ
	expires       int64  // seconds; 0 for header auth
	signedHeaders []string
	signature     string // hex
	query         bool   // query-string (signed URL) form
}

// signedURLParams extracts query-string signature parameters, or nil if the
// request is not a signed URL.
func signedURLParams(q url.Values) (*sigParams, error) {
	get := func(name string) string {
		for _, p := range []string{"X-Goog-", "X-Amz-"} {
			if v := q.Get(p + name); v != "" {
				return v
			}
		}
		return ""
	}
	alg := get("Algorithm")
	if alg == "" {
		return nil, nil
	}
	p := &sigParams{
		algorithm:     alg,
		credential:    get("Credential"),
		date:          get("Date"),
		signedHeaders: strings.Split(strings.ToLower(get("SignedHeaders")), ";"),
		signature:     strings.ToLower(get("Signature")),
		query:         true,
	}
	exp, err := strconv.ParseInt(get("Expires"), 10, 64)
	if err != nil || exp <= 0 || exp > maxSignedExpiry {
		return nil, errMalformed("Invalid X-Goog-Expires: must be between 1 and 604800 seconds.")
	}
	p.expires = exp
	return p, nil
}

// headerSigParams parses "Authorization: GOOG4-HMAC-SHA256 Credential=...,
// SignedHeaders=..., Signature=..." (or AWS4-HMAC-SHA256).
func headerSigParams(r *http.Request) *sigParams {
	authz := r.Header.Get("Authorization")
	alg, rest, ok := strings.Cut(authz, " ")
	if !ok || !strings.HasSuffix(alg, "-HMAC-SHA256") && !strings.HasSuffix(alg, "-RSA-SHA256") {
		return nil
	}
	p := &sigParams{algorithm: alg}
	for _, kv := range strings.Split(rest, ",") {
		k, v, _ := strings.Cut(strings.TrimSpace(kv), "=")
		switch k {
		case "Credential":
			p.credential = v
		case "SignedHeaders":
			p.signedHeaders = strings.Split(strings.ToLower(v), ";")
		case "Signature":
			p.signature = strings.ToLower(v)
		}
	}
	p.date = r.Header.Get("X-Goog-Date")
	if p.date == "" {
		p.date = r.Header.Get("X-Amz-Date")
	}
	return p
}

// canonicalHeaderValue trims and collapses whitespace.
func canonicalHeaderValue(v string) string { return strings.Join(strings.Fields(v), " ") }

// canonicalQuery encodes the query (minus the signature) as V4 requires.
func canonicalQuery(q url.Values) string {
	c := url.Values{}
	for k, v := range q {
		if strings.EqualFold(k, "X-Goog-Signature") || strings.EqualFold(k, "X-Amz-Signature") {
			continue
		}
		c[k] = v
	}
	keys := make([]string, 0, len(c))
	for k := range c {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		vs := append([]string(nil), c[k]...)
		sort.Strings(vs)
		for _, v := range vs {
			parts = append(parts, v4Escape(k)+"="+v4Escape(v))
		}
	}
	return strings.Join(parts, "&")
}

func v4Escape(s string) string {
	return strings.ReplaceAll(url.QueryEscape(s), "+", "%20")
}

// pathEncodeV4 percent-encodes a path the way the client libraries do
// (every byte except unreserved characters and '/').
func pathEncodeV4(p string) string {
	var b strings.Builder
	for i := 0; i < len(p); i++ {
		c := p[i]
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || strings.IndexByte("-._~/", c) >= 0 {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// canonicalRequests returns the candidate canonical requests (path and host
// spellings differ between clients).
func canonicalRequests(r *http.Request, p *sigParams, uris []string) []string {
	var hosts []string
	hosts = append(hosts, r.Host)
	if h, _, err := splitHostPort(r.Host); err == nil && h != r.Host {
		hosts = append(hosts, h)
	}
	payload := "UNSIGNED-PAYLOAD"
	for _, h := range []string{"X-Goog-Content-Sha256", "X-Amz-Content-Sha256"} {
		if v := r.Header.Get(h); v != "" && contains(p.signedHeaders, strings.ToLower(h)) {
			payload = v
		} else if v != "" && !p.query {
			payload = v
		}
	}
	query := canonicalQuery(r.URL.Query())
	var out []string
	for _, uri := range uris {
		for _, host := range hosts {
			var hdrs []string
			for _, name := range p.signedHeaders {
				v := ""
				if name == "host" {
					v = host
				} else {
					v = canonicalHeaderValue(strings.Join(r.Header.Values(name), ","))
				}
				hdrs = append(hdrs, name+":"+v)
			}
			out = append(out, strings.Join([]string{
				r.Method, uri, query, strings.Join(hdrs, "\n") + "\n", strings.Join(p.signedHeaders, ";"), payload,
			}, "\n"))
		}
	}
	return out
}

func splitHostPort(h string) (string, string, error) {
	i := strings.LastIndexByte(h, ':')
	if i < 0 || strings.HasSuffix(h, "]") {
		return h, "", fmt.Errorf("no port")
	}
	return strings.Trim(h[:i], "[]"), h[i+1:], nil
}

// verifyV4 verifies a V4 signature (URL or header form) on r for the
// resource path bucket/object and returns the signing principal.
func (s *Service) verifyV4(r *http.Request, p *sigParams, bucket, object string) (emu.Principal, error) {
	cred := strings.Split(p.credential, "/")
	if len(cred) != 5 {
		return "", errMalformed("Invalid credential scope: " + p.credential)
	}
	id, scope := cred[0], strings.Join(cred[1:], "/")
	t, err := time.Parse(iso8601, p.date)
	if err != nil {
		return "", errMalformed("Invalid X-Goog-Date: " + p.date)
	}
	now := s.now()
	if p.query && now.After(t.Add(time.Duration(p.expires)*time.Second)) {
		return "", errExpired("Request signature expired at: " + t.Add(time.Duration(p.expires)*time.Second).Format(time.RFC3339))
	}
	if p.query && t.After(now.Add(15*time.Minute)) {
		return "", errExpired("Request is not yet valid.")
	}
	uris := []string{r.URL.EscapedPath()}
	res := "/" + bucket
	if object != "" {
		res += "/" + object
	}
	if enc := pathEncodeV4(res); enc != uris[0] {
		uris = append(uris, enc)
	}
	if object == "" {
		uris = append(uris, pathEncodeV4(res+"/"))
	}
	var candidates []string
	for _, cr := range canonicalRequests(r, p, uris) {
		sum := sha256.Sum256([]byte(cr))
		candidates = append(candidates, strings.Join([]string{p.algorithm, p.date, scope, hex.EncodeToString(sum[:])}, "\n"))
	}
	sig, err := hex.DecodeString(p.signature)
	if err != nil {
		return "", errSignature("The signature is not valid hex.")
	}
	return s.verifySignature(r.Context(), p.algorithm, id, cred, sig, candidates)
}

// verifySignature checks sig over any of the candidate strings-to-sign and
// returns the signer.
func (s *Service) verifySignature(ctx context.Context, alg, id string, cred []string, sig []byte, candidates []string) (emu.Principal, error) {
	switch {
	case strings.HasSuffix(alg, "-RSA-SHA256"):
		keys, err := s.saKeys(ctx, id)
		if err != nil {
			return "", err
		}
		for _, sts := range candidates {
			sum := sha256.Sum256([]byte(sts))
			for _, k := range keys {
				if rsa.VerifyPKCS1v15(k, crypto.SHA256, sum[:], sig) == nil {
					return emu.Principal("serviceAccount:" + id), nil
				}
				// SignBytes-style callers hash before signing.
				sum2 := sha256.Sum256(sum[:])
				if rsa.VerifyPKCS1v15(k, crypto.SHA256, sum2[:], sig) == nil {
					return emu.Principal("serviceAccount:" + id), nil
				}
			}
		}
		return "", errSignature("The request signature we calculated does not match the signature you provided. Check your Google secret key and signing method.")
	case strings.HasSuffix(alg, "-HMAC-SHA256"):
		key, err := s.hmacSecret(ctx, id)
		if err != nil {
			return "", err
		}
		prefix := strings.TrimSuffix(alg, "-HMAC-SHA256")
		k := hmacSHA256([]byte(prefix+key.Secret), cred[1])
		for _, part := range cred[2:] {
			k = hmacSHA256(k, part)
		}
		for _, sts := range candidates {
			if hmac.Equal(hmacSHA256(k, sts), sig) {
				return emu.Principal("serviceAccount:" + key.Meta.ServiceAccountEmail), nil
			}
		}
		return "", errSignature("The request signature we calculated does not match the signature you provided. Check your Google secret key and signing method.")
	}
	return "", errMalformed("Unsupported signing algorithm: " + alg)
}

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}

// saKeys returns the public keys of a service account from the iam service.
func (s *Service) saKeys(ctx context.Context, email string) ([]*rsa.PublicKey, error) {
	svc, ok := s.env.Lookup("iam")
	var ks emu.ServiceAccountKeys
	if ok {
		ks, _ = svc.(emu.ServiceAccountKeys)
	}
	if ks == nil {
		return nil, errSignature("Service account keys are unavailable (iam service not running).")
	}
	keys, err := ks.PublicKeys(ctx, email)
	if err != nil || len(keys) == 0 {
		return nil, errSignature("No active keys for service account " + email + ".")
	}
	return keys, nil
}
