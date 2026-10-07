package cdn

import (
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	compute "google.golang.org/api/compute/v1"
)

// Cache modes (FR-CDN-002).
const (
	CacheAllStatic   = "CACHE_ALL_STATIC"
	UseOriginHeaders = "USE_ORIGIN_HEADERS"
	ForceCacheAll    = "FORCE_CACHE_ALL"
)

// GCP defaults and limits (Cloud CDN "Caching overview", "TTL settings").
const (
	defaultDefaultTTL = 3600
	defaultMaxTTL     = 86400
	defaultClientTTL  = 3600
	defaultSignedTTL  = 3600
	// originTTLCap: expiration values beyond 30 days are treated as 30 days.
	originTTLCap = 2592000
	// maxObjectNoRanges is the largest cacheable response from an origin
	// that does not support byte ranges (10 MiB).
	maxObjectNoRanges = 10 << 20
	// maxObjectRanges is the largest cacheable response from an origin that
	// supports byte ranges (100 GiB).
	maxObjectRanges = 100 << 30
)

// cacheableStatus lists the status codes Cloud CDN can cache.
var cacheableStatus = map[int]bool{
	200: true, 203: true, 204: true, 206: true, 300: true, 301: true, 302: true,
	307: true, 308: true, 404: true, 405: true, 410: true, 421: true, 451: true, 501: true,
}

// successStatus are the codes that receive defaultTtl in CACHE_ALL_STATIC
// and FORCE_CACHE_ALL without explicit freshness headers.
var successStatus = map[int]bool{200: true, 203: true, 204: true, 206: true}

// defaultNegativeTTL is the negative caching TTL per status code applied
// when negativeCaching is on and no negativeCachingPolicy is given.
var defaultNegativeTTL = map[int]int64{
	300: 600, 301: 600, 308: 600,
	404: 120, 405: 60, 410: 120, 451: 120, 501: 60,
}

// allowedVary are the Vary header values that keep a response cacheable
// (besides headers named in the cache key policy).
var allowedVary = map[string]bool{
	"Accept": true, "Accept-Encoding": true, "Access-Control-Request-Headers": true,
	"Access-Control-Request-Method": true, "Available-Dictionary": true, "Origin": true,
	"Sec-Fetch-Dest": true, "Sec-Fetch-Mode": true, "Sec-Fetch-Site": true,
	"X-Goog-Allowed-Resources": true, "X-Origin": true,
}

// isStatic reports whether a Content-Type is cached by CACHE_ALL_STATIC.
func isStatic(ct string) bool {
	mt, _, _ := strings.Cut(ct, ";")
	mt = strings.ToLower(strings.TrimSpace(mt))
	switch mt {
	case "text/css", "text/ecmascript", "text/javascript", "application/javascript",
		"application/pdf", "application/postscript":
		return true
	}
	for _, p := range []string{"font/", "image/", "video/", "audio/"} {
		if strings.HasPrefix(mt, p) {
			return true
		}
	}
	return false
}

// policy is a Backend's CDN configuration with GCP defaults applied.
type policy struct {
	mode            string
	defaultTTL      int64
	maxTTL          int64
	clientTTL       int64
	negative        bool
	negTTL          map[int]int64
	serveWhileStale int64
	coalesce        bool
	signedTTL       int64
	bypass          []string
	key             keyPolicy
}

// keyPolicy is the effective cache key policy (FR-CDN-003).
type keyPolicy struct {
	protocol, host, query bool
	allow, deny           []string
	headers               []string // canonical names
	cookies               []string
}

func forced(fields []string, name string) bool { return slices.Contains(fields, name) }

// ttlOr returns v, or def when v is zero and not explicitly sent.
func ttlOr(v int64, fields []string, name string, def int64) int64 {
	if v == 0 && !forced(fields, name) {
		return def
	}
	return v
}

// resolvePolicy applies GCP defaults to a Backend's cdnPolicy.
func resolvePolicy(b *Backend) *policy {
	p := &policy{mode: CacheAllStatic, key: keyPolicy{protocol: true, host: true, query: true}}
	var neg []struct{ code, ttl int64 }
	switch {
	case b.BucketPolicy != nil:
		bp := b.BucketPolicy
		p.mode = orDefault(bp.CacheMode, CacheAllStatic)
		p.defaultTTL = ttlOr(bp.DefaultTtl, bp.ForceSendFields, "DefaultTtl", defaultDefaultTTL)
		p.maxTTL = ttlOr(bp.MaxTtl, bp.ForceSendFields, "MaxTtl", defaultMaxTTL)
		p.clientTTL = ttlOr(bp.ClientTtl, bp.ForceSendFields, "ClientTtl", defaultClientTTL)
		p.negative = bp.NegativeCaching
		for _, n := range bp.NegativeCachingPolicy {
			neg = append(neg, struct{ code, ttl int64 }{n.Code, n.Ttl})
		}
		p.serveWhileStale = bp.ServeWhileStale
		p.coalesce = bp.RequestCoalescing
		p.signedTTL = bp.SignedUrlCacheMaxAgeSec
		for _, h := range bp.BypassCacheOnRequestHeaders {
			p.bypass = append(p.bypass, h.HeaderName)
		}
		// Backend bucket keys omit protocol and host (FR-CDN-003).
		p.key = keyPolicy{query: true}
		if k := bp.CacheKeyPolicy; k != nil {
			p.key.allow = k.QueryStringWhitelist
			p.key.headers = canonical(k.IncludeHttpHeaders)
		}
	case b.ServicePolicy != nil:
		sp := b.ServicePolicy
		p.mode = orDefault(sp.CacheMode, CacheAllStatic)
		p.defaultTTL = ttlOr(sp.DefaultTtl, sp.ForceSendFields, "DefaultTtl", defaultDefaultTTL)
		p.maxTTL = ttlOr(sp.MaxTtl, sp.ForceSendFields, "MaxTtl", defaultMaxTTL)
		p.clientTTL = ttlOr(sp.ClientTtl, sp.ForceSendFields, "ClientTtl", defaultClientTTL)
		p.negative = sp.NegativeCaching
		for _, n := range sp.NegativeCachingPolicy {
			neg = append(neg, struct{ code, ttl int64 }{n.Code, n.Ttl})
		}
		p.serveWhileStale = sp.ServeWhileStale
		p.coalesce = sp.RequestCoalescing
		p.signedTTL = sp.SignedUrlCacheMaxAgeSec
		for _, h := range sp.BypassCacheOnRequestHeaders {
			p.bypass = append(p.bypass, h.HeaderName)
		}
		if k := sp.CacheKeyPolicy; k != nil {
			p.key = serviceKeyPolicy(k)
		}
	default:
		p.defaultTTL, p.maxTTL, p.clientTTL = defaultDefaultTTL, defaultMaxTTL, defaultClientTTL
		p.coalesce = true
	}
	if b.SignedURLCacheMaxAgeSec != 0 {
		p.signedTTL = b.SignedURLCacheMaxAgeSec
	}
	if p.signedTTL == 0 {
		p.signedTTL = defaultSignedTTL
	}
	if len(neg) > 0 {
		// An explicit policy replaces all defaults.
		p.negTTL = make(map[int]int64, len(neg))
		for _, n := range neg {
			p.negTTL[int(n.code)] = n.ttl
		}
	} else {
		p.negTTL = defaultNegativeTTL
	}
	return p
}

func serviceKeyPolicy(k *compute.CacheKeyPolicy) keyPolicy {
	return keyPolicy{
		protocol: k.IncludeProtocol,
		host:     k.IncludeHost,
		query:    k.IncludeQueryString,
		allow:    k.QueryStringWhitelist,
		deny:     k.QueryStringBlacklist,
		headers:  canonical(k.IncludeHttpHeaders),
		cookies:  k.IncludeNamedCookies,
	}
}

func canonical(names []string) []string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = http.CanonicalHeaderKey(n)
	}
	return out
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// cacheControl is a parsed Cache-Control header. Absent second values are -1.
type cacheControl struct {
	present                                      bool
	noStore, noCache, private, public, mustReval bool
	proxyReval, noTransform                      bool
	maxAge, sMaxAge, staleWhileRevalidate        int64
	raw                                          []string
}

func parseCacheControl(v []string) cacheControl {
	cc := cacheControl{maxAge: -1, sMaxAge: -1, staleWhileRevalidate: -1}
	for _, line := range v {
		for _, d := range strings.Split(line, ",") {
			d = strings.TrimSpace(d)
			if d == "" {
				continue
			}
			cc.present = true
			cc.raw = append(cc.raw, d)
			name, val, _ := strings.Cut(d, "=")
			val = strings.Trim(strings.TrimSpace(val), `"`)
			switch strings.ToLower(strings.TrimSpace(name)) {
			case "no-store":
				cc.noStore = true
			case "no-cache":
				cc.noCache = true
			case "private":
				cc.private = true
			case "public":
				cc.public = true
			case "must-revalidate":
				cc.mustReval = true
			case "proxy-revalidate":
				cc.proxyReval = true
			case "no-transform":
				cc.noTransform = true
			case "max-age":
				cc.maxAge = parseSeconds(val)
			case "s-maxage":
				cc.sMaxAge = parseSeconds(val)
			case "stale-while-revalidate":
				cc.staleWhileRevalidate = parseSeconds(val)
			}
		}
	}
	return cc
}

// parseSeconds parses a delta-seconds value; malformed values mean 0
// (treated as already stale, RFC 9111 section 1.2.2).
func parseSeconds(v string) int64 {
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// decision is the outcome of evaluating a response for caching.
type decision struct {
	cacheable bool
	ttl       int64  // freshness lifetime in seconds
	clientCC  string // Cache-Control sent to clients; "" keeps the origin's
	noStale   bool   // must-revalidate or s-maxage: never served stale
	swr       int64  // stale-while-revalidate seconds
	reason    string // why not cacheable (debug)
}

func no(reason string) decision { return decision{reason: reason} }

// decide applies the Cloud CDN cacheability rules (FR-CDN-002, FR-CDN-006)
// to an origin response for req. signed reports a validated signed request.
func (p *policy) decide(req *http.Request, status int, h http.Header, signed bool, now time.Time) decision {
	if !cacheableStatus[status] || status == 206 {
		// 206 is only produced for ranges we never forward; a partial
		// body is never stored.
		return no("status")
	}
	if len(h.Values("Set-Cookie")) > 0 {
		return no("set-cookie")
	}
	for _, line := range h.Values("Vary") {
		for _, v := range strings.Split(line, ",") {
			v = http.CanonicalHeaderKey(strings.TrimSpace(v))
			if v == "" {
				continue
			}
			if v == "*" || (!allowedVary[v] && !slices.Contains(p.key.headers, v)) {
				return no("vary")
			}
		}
	}
	ccv := h.Values("Cdn-Cache-Control")
	if len(ccv) == 0 {
		ccv = h.Values("Cache-Control")
	}
	cc := parseCacheControl(ccv)
	d := decision{noStale: cc.mustReval || cc.proxyReval || cc.sMaxAge >= 0}
	if cc.staleWhileRevalidate > 0 {
		d.swr = cc.staleWhileRevalidate
	}

	if signed {
		// Responses to signed requests behave as "public,
		// max-age=signedUrlCacheMaxAgeSec"; client headers are unaltered.
		d.cacheable, d.ttl = true, p.signedTTL
		return d
	}
	if p.mode != ForceCacheAll {
		if cc.private || cc.noStore {
			return no("private/no-store")
		}
		if req.Header.Get("Authorization") != "" && !cc.public && !cc.mustReval && cc.sMaxAge < 0 {
			return no("authorization")
		}
	}

	// Explicit freshness from the origin.
	explicit, ttl := false, int64(0)
	switch {
	case cc.noCache:
		explicit = true
	case cc.sMaxAge >= 0:
		explicit, ttl = true, cc.sMaxAge
	case cc.maxAge >= 0:
		explicit, ttl = true, cc.maxAge
	case !cc.present && h.Get("Expires") != "":
		explicit = true
		if exp, err := http.ParseTime(h.Get("Expires")); err == nil {
			base := now
			if dt, err := http.ParseTime(h.Get("Date")); err == nil {
				base = dt
			}
			if s := int64(exp.Sub(base) / time.Second); s > 0 {
				ttl = s
			}
		}
	}
	negTTL, isNeg := int64(0), false
	if p.negative {
		negTTL, isNeg = p.negTTL[status]
	}

	switch p.mode {
	case ForceCacheAll:
		switch {
		case isNeg:
			d.ttl = negTTL
		case successStatus[status]:
			d.ttl = p.defaultTTL
		default:
			return no("force: status")
		}
		d.noStale = false
		d.clientCC = "public, max-age=" + strconv.FormatInt(min(d.ttl, p.clientTTL), 10)
	case UseOriginHeaders:
		switch {
		case explicit:
			d.ttl = min(ttl, originTTLCap)
		case isNeg:
			d.ttl = negTTL
		default:
			return no("no freshness headers")
		}
	default: // CACHE_ALL_STATIC
		switch {
		case explicit:
			d.ttl = min(ttl, p.maxTTL)
		case isNeg:
			d.ttl = negTTL
		case successStatus[status] && isStatic(h.Get("Content-Type")):
			d.ttl = p.defaultTTL
		default:
			return no("not static")
		}
		d.clientCC = clientCacheControl(cc, d.ttl, p.clientTTL)
	}
	d.cacheable = true
	return d
}

// clientCacheControl rewrites the origin Cache-Control for clients in
// CACHE_ALL_STATIC: max-age is clamped to clientTtl.
func clientCacheControl(cc cacheControl, ttl, clientTTL int64) string {
	if !cc.present {
		return "public, max-age=" + strconv.FormatInt(min(ttl, clientTTL), 10)
	}
	if cc.noCache {
		return "" // keep: clients must revalidate anyway
	}
	ma := ttl
	if cc.maxAge >= 0 {
		ma = cc.maxAge
	}
	ma = min(ma, clientTTL)
	out := make([]string, 0, len(cc.raw)+1)
	for _, d := range cc.raw {
		name, _, _ := strings.Cut(d, "=")
		if strings.EqualFold(strings.TrimSpace(name), "max-age") {
			continue
		}
		out = append(out, d)
	}
	out = append(out, "max-age="+strconv.FormatInt(ma, 10))
	return strings.Join(out, ", ")
}
