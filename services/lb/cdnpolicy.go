package lb

import (
	"cmp"
	"fmt"

	computev1 "google.golang.org/api/compute/v1"
)

// Cloud CDN policy limits (FR-CDN-002, FR-CDN-006), as GCP enforces them on
// backendServices and backendBuckets.
const (
	maxCdnTTL          = 31622400 // one year
	maxServeWhileStale = 604800   // one week
	maxNegativeTTL     = 1800     // 30 minutes
	maxBypassHeaders   = 5
)

// negativeCodes are the status codes negativeCachingPolicy may name.
var negativeCodes = map[int64]bool{300: true, 301: true, 302: true, 307: true, 308: true,
	404: true, 405: true, 410: true, 421: true, 451: true, 501: true}

// cdnPolicy is the part of a backend service's or bucket's cdnPolicy the
// two share.
type cdnPolicy struct {
	cacheMode                                                    string
	defaultTTL, maxTTL, clientTTL, serveWhileStale, signedMaxAge int64
	negative                                                     bool
	negativePolicy                                               [][2]int64 // code, ttl
	bypass                                                       int
	allow, deny                                                  []string
	includeQuery                                                 bool
}

func serviceCdnPolicy(p *computev1.BackendServiceCdnPolicy) cdnPolicy {
	c := cdnPolicy{cacheMode: p.CacheMode, defaultTTL: p.DefaultTtl, maxTTL: p.MaxTtl, clientTTL: p.ClientTtl,
		serveWhileStale: p.ServeWhileStale, signedMaxAge: p.SignedUrlCacheMaxAgeSec,
		negative: p.NegativeCaching, bypass: len(p.BypassCacheOnRequestHeaders), includeQuery: true}
	for _, n := range p.NegativeCachingPolicy {
		c.negativePolicy = append(c.negativePolicy, [2]int64{n.Code, n.Ttl})
	}
	if k := p.CacheKeyPolicy; k != nil {
		c.allow, c.deny, c.includeQuery = k.QueryStringWhitelist, k.QueryStringBlacklist, k.IncludeQueryString
	}
	return c
}

func bucketCdnPolicy(p *computev1.BackendBucketCdnPolicy) cdnPolicy {
	c := cdnPolicy{cacheMode: p.CacheMode, defaultTTL: p.DefaultTtl, maxTTL: p.MaxTtl, clientTTL: p.ClientTtl,
		serveWhileStale: p.ServeWhileStale, signedMaxAge: p.SignedUrlCacheMaxAgeSec,
		negative: p.NegativeCaching, bypass: len(p.BypassCacheOnRequestHeaders), includeQuery: true}
	for _, n := range p.NegativeCachingPolicy {
		c.negativePolicy = append(c.negativePolicy, [2]int64{n.Code, n.Ttl})
	}
	if k := p.CacheKeyPolicy; k != nil {
		c.allow = k.QueryStringWhitelist
	}
	return c
}

// validateCdnPolicy checks a cdnPolicy as sent, before defaults are filled.
func validateCdnPolicy(p cdnPolicy) error {
	const f = "resource.cdnPolicy."
	switch p.cacheMode {
	case "", "CACHE_ALL_STATIC", "USE_ORIGIN_HEADERS", "FORCE_CACHE_ALL":
	default:
		return errInvalid(f+"cacheMode", p.cacheMode, "")
	}
	for _, t := range []struct {
		name string
		v    int64
		max  int64
	}{
		{"defaultTtl", p.defaultTTL, maxCdnTTL},
		{"maxTtl", p.maxTTL, maxCdnTTL},
		{"clientTtl", p.clientTTL, maxCdnTTL},
		{"serveWhileStale", p.serveWhileStale, maxServeWhileStale},
		{"signedUrlCacheMaxAgeSec", p.signedMaxAge, maxCdnTTL},
	} {
		if t.v < 0 || t.v > t.max {
			return errInvalid(f+t.name, t.v, fmt.Sprintf("Must be between 0 and %d.", t.max))
		}
	}
	if p.cacheMode == "CACHE_ALL_STATIC" || p.cacheMode == "" {
		// An unset maxTtl becomes the default (defaultServiceCdnPolicy).
		maxTTL := cmp.Or(p.maxTTL, 86400)
		if p.defaultTTL > maxTTL {
			return errInvalid(f+"defaultTtl", p.defaultTTL, "Default TTL must be less than or equal to max TTL.")
		}
		if p.clientTTL > maxTTL {
			return errInvalid(f+"clientTtl", p.clientTTL, "Client TTL must be less than or equal to max TTL.")
		}
	}
	if len(p.negativePolicy) > 0 && !p.negative {
		return errInvalid(f+"negativeCachingPolicy", len(p.negativePolicy), "Negative caching policy requires negative caching to be enabled.")
	}
	seen := map[int64]bool{}
	for i, n := range p.negativePolicy {
		field := fmt.Sprintf("%snegativeCachingPolicy[%d]", f, i)
		if !negativeCodes[n[0]] {
			return errInvalid(field+".code", n[0], "Status code must be one of 300, 301, 302, 307, 308, 404, 405, 410, 421, 451 or 501.")
		}
		if seen[n[0]] {
			return errInvalid(field+".code", n[0], "Status codes must be unique.")
		}
		seen[n[0]] = true
		if n[1] < 0 || n[1] > maxNegativeTTL {
			return errInvalid(field+".ttl", n[1], fmt.Sprintf("Must be between 0 and %d.", maxNegativeTTL))
		}
	}
	if p.bypass > maxBypassHeaders {
		return errInvalid(f+"bypassCacheOnRequestHeaders", p.bypass, fmt.Sprintf("At most %d headers can be specified.", maxBypassHeaders))
	}
	if len(p.allow) > 0 && len(p.deny) > 0 {
		return errInvalid(f+"cacheKeyPolicy.queryStringBlacklist", len(p.deny), "Only one of queryStringWhitelist and queryStringBlacklist can be specified.")
	}
	if !p.includeQuery && len(p.allow)+len(p.deny) > 0 {
		return errInvalid(f+"cacheKeyPolicy.includeQueryString", false, "Query string lists require includeQueryString.")
	}
	return nil
}
