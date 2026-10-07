package cdn

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

// SignedCookieName is the Cloud CDN signed cookie (FR-CDN-007).
const SignedCookieName = "Cloud-CDN-Cookie"

var errSignature = errors.New("invalid signature")

// verifySigned validates a signed URL or signed cookie on r against the
// backend's keys (FR-CDN-007). It reports whether r is a validly signed
// request; an error means the request must be rejected with 403. Requests
// carrying neither a Signature parameter nor the signed cookie are not
// blocked, and backends without keys never validate — as on GCP, the
// origin must reject unsigned requests itself.
func verifySigned(r *http.Request, keys map[string][]byte, now time.Time) (bool, error) {
	if len(keys) == 0 {
		return false, nil
	}
	raw := r.URL.RawQuery
	if hasParam(raw, "Signature") {
		return true, verifySignedURL(r, raw, keys, now)
	}
	if c, err := r.Cookie(SignedCookieName); err == nil {
		return true, verifySignedCookie(r, c.Value, keys, now)
	}
	return false, nil
}

func hasParam(raw, name string) bool {
	for _, p := range strings.Split(raw, "&") {
		if n, _, _ := strings.Cut(p, "="); n == name {
			return true
		}
	}
	return false
}

// verifySignedURL checks Expires/KeyName/Signature, with the optional
// URLPrefix variant.
func verifySignedURL(r *http.Request, raw string, keys map[string][]byte, now time.Time) error {
	q, err := url.ParseQuery(raw)
	if err != nil {
		return errSignature
	}
	exp, keyName, sig := q.Get("Expires"), q.Get("KeyName"), q.Get("Signature")
	if exp == "" || keyName == "" || sig == "" {
		return errSignature
	}
	key, ok := keys[keyName]
	if !ok {
		return errSignature
	}
	if err := checkExpires(exp, now); err != nil {
		return err
	}
	var signed string
	if prefix := q.Get("URLPrefix"); prefix != "" {
		pb, err := decodeB64(prefix)
		if err != nil {
			return errSignature
		}
		if !strings.HasPrefix(requestURL(r, true), string(pb)) {
			return errSignature
		}
		signed = "URLPrefix=" + prefix + "&Expires=" + exp + "&KeyName=" + keyName
	} else {
		// The signature covers the full URL up to "&Signature=".
		full := scheme(r) + "://" + r.Host + r.URL.EscapedPath() + "?" + raw
		i := strings.LastIndex(full, "&Signature=")
		if i < 0 {
			return errSignature
		}
		signed = full[:i]
	}
	return checkMAC(key, signed, sig)
}

// verifySignedCookie checks a Cloud-CDN-Cookie value of the form
// URLPrefix=B64:Expires=TS:KeyName=NAME:Signature=B64.
func verifySignedCookie(r *http.Request, v string, keys map[string][]byte, now time.Time) error {
	fields := map[string]string{}
	for _, part := range strings.Split(v, ":") {
		k, val, ok := strings.Cut(part, "=")
		if !ok {
			return errSignature
		}
		fields[k] = val
	}
	prefix, exp, keyName, sig := fields["URLPrefix"], fields["Expires"], fields["KeyName"], fields["Signature"]
	if prefix == "" || exp == "" || keyName == "" || sig == "" {
		return errSignature
	}
	key, ok := keys[keyName]
	if !ok {
		return errSignature
	}
	if err := checkExpires(exp, now); err != nil {
		return err
	}
	pb, err := decodeB64(prefix)
	if err != nil || !strings.HasPrefix(requestURL(r, true), string(pb)) {
		return errSignature
	}
	return checkMAC(key, "URLPrefix="+prefix+":Expires="+exp+":KeyName="+keyName, sig)
}

func checkExpires(exp string, now time.Time) error {
	ts, err := strconv.ParseInt(exp, 10, 64)
	if err != nil || now.Unix() > ts {
		return errSignature
	}
	return nil
}

func checkMAC(key []byte, signed, sig string) error {
	got, err := decodeB64(sig)
	if err != nil {
		return errSignature
	}
	m := hmac.New(sha1.New, key)
	m.Write([]byte(signed))
	if !hmac.Equal(m.Sum(nil), got) {
		return errSignature
	}
	return nil
}

// decodeB64 decodes base64url with or without padding.
func decodeB64(s string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
}

// requestURL returns the client URL of r; strip removes signing params.
func requestURL(r *http.Request, strip bool) string {
	u := scheme(r) + "://" + r.Host + r.URL.EscapedPath()
	q := r.URL.RawQuery
	if strip {
		q = stripSigning(q)
	}
	if q != "" {
		u += "?" + q
	}
	return u
}

// stripSigning removes the signed URL parameters from a raw query.
func stripSigning(raw string) string {
	if raw == "" {
		return ""
	}
	parts := strings.Split(raw, "&")
	parts = slices.DeleteFunc(parts, func(p string) bool {
		n, _, _ := strings.Cut(p, "=")
		return p == "" || slices.Contains(signingParams, n)
	})
	return strings.Join(parts, "&")
}

// SignURL signs u (which must not already carry signing params) with a
// Cloud CDN key, like `gcloud compute sign-url`. Exported for tests and
// tooling.
func SignURL(u, keyName string, key []byte, expires time.Time) string {
	sep := "?"
	if strings.Contains(u, "?") {
		sep = "&"
	}
	s := u + sep + "Expires=" + strconv.FormatInt(expires.Unix(), 10) + "&KeyName=" + keyName
	return s + "&Signature=" + mac(key, s)
}

// SignURLPrefix returns the query string granting access to every URL
// starting with prefix.
func SignURLPrefix(prefix, keyName string, key []byte, expires time.Time) string {
	s := "URLPrefix=" + base64.URLEncoding.EncodeToString([]byte(prefix)) +
		"&Expires=" + strconv.FormatInt(expires.Unix(), 10) + "&KeyName=" + keyName
	return s + "&Signature=" + mac(key, s)
}

// SignCookie returns a Cloud-CDN-Cookie value for prefix.
func SignCookie(prefix, keyName string, key []byte, expires time.Time) string {
	s := "URLPrefix=" + base64.URLEncoding.EncodeToString([]byte(prefix)) +
		":Expires=" + strconv.FormatInt(expires.Unix(), 10) + ":KeyName=" + keyName
	return s + ":Signature=" + mac(key, s)
}

func mac(key []byte, s string) string {
	m := hmac.New(sha1.New, key)
	m.Write([]byte(s))
	return base64.URLEncoding.EncodeToString(m.Sum(nil))
}
