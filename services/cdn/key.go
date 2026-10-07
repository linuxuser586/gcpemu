package cdn

import (
	"net"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"
)

// signingParams are the signed URL query parameters; they never reach the
// origin and are not part of the cache key (FR-CDN-007).
var signingParams = []string{"Expires", "KeyName", "Signature", "URLPrefix"}

// scheme returns the client-facing protocol of r.
func scheme(r *http.Request) string {
	if r.TLS != nil {
		return "https"
	}
	if p := r.Header.Get("X-Forwarded-Proto"); p != "" {
		return strings.ToLower(p)
	}
	return "http"
}

// hostname returns r.Host without the port, lower-cased.
func hostname(r *http.Request) string {
	h := r.Host
	if hh, _, err := net.SplitHostPort(h); err == nil {
		h = hh
	}
	return strings.ToLower(h)
}

// cacheKey builds the cache key for r (FR-CDN-003). The key does not
// depend on query parameter order. signed requests get a separate key
// space so signed responses never serve unsigned requests.
func (k *keyPolicy) cacheKey(r *http.Request, signed bool) string {
	var b strings.Builder
	if signed {
		b.WriteString("signed|")
	}
	if k.protocol {
		b.WriteString(scheme(r))
		b.WriteString("://")
	}
	if k.host {
		b.WriteString(strings.ToLower(r.Host))
	}
	b.WriteString(r.URL.EscapedPath())
	if k.query && r.URL.RawQuery != "" {
		if q := k.query1(r.URL.RawQuery, signed); q != "" {
			b.WriteByte('?')
			b.WriteString(q)
		}
	}
	for _, h := range k.headers {
		b.WriteString("\n")
		b.WriteString(h)
		b.WriteByte(':')
		b.WriteString(strings.Join(r.Header.Values(h), ","))
	}
	for _, name := range k.cookies {
		b.WriteString("\ncookie:")
		b.WriteString(name)
		if c, err := r.Cookie(name); err == nil {
			b.WriteByte('=')
			b.WriteString(c.Value)
		}
	}
	return b.String()
}

// query1 returns the sorted, filtered query string for the key.
func (k *keyPolicy) query1(raw string, signed bool) string {
	parts := strings.Split(raw, "&")
	out := parts[:0:0]
	for _, p := range parts {
		if p == "" {
			continue
		}
		name, _, _ := strings.Cut(p, "=")
		if n, err := url.QueryUnescape(name); err == nil {
			name = n
		}
		if signed && slices.Contains(signingParams, name) {
			continue
		}
		if len(k.allow) > 0 && !slices.Contains(k.allow, name) {
			continue
		}
		if len(k.deny) > 0 && slices.Contains(k.deny, name) {
			continue
		}
		out = append(out, p)
	}
	sort.Strings(out)
	return strings.Join(out, "&")
}

// varyValues returns the request's values for the Vary'd header names.
func varyValues(r *http.Request, names []string) string {
	if len(names) == 0 {
		return ""
	}
	var b strings.Builder
	for _, n := range names {
		b.WriteString(strings.Join(r.Header.Values(n), ","))
		b.WriteByte(0)
	}
	return b.String()
}

// varyNames parses the Vary header of a response into canonical names.
func varyNames(h http.Header) []string {
	var out []string
	for _, line := range h.Values("Vary") {
		for _, v := range strings.Split(line, ",") {
			if v = strings.TrimSpace(v); v != "" {
				out = append(out, http.CanonicalHeaderKey(v))
			}
		}
	}
	sort.Strings(out)
	return slices.Compact(out)
}
