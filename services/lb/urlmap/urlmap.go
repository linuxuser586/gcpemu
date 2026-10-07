// Package urlmap is the load balancer's URL map routing engine (FR-LB-003):
// a pure library that compiles a compute v1 UrlMap and selects, for a
// request, the backend (service or bucket), redirect, URL rewrite and
// header actions exactly as GCP's Application Load Balancers do:
//
//   - host rules: exact host (with port, then without) beats the longest
//     wildcard suffix ("*.example.com", "*-example.com"), which beats "*";
//     no match uses the URL map's defaults;
//   - path rules: the longest matching path wins ("/a/*" matches "/a/" and
//     below, never "/a"); an exact path beats a prefix of equal length; no
//     match uses the path matcher's defaults;
//   - route rules: the lowest priority whose any matchRule matches (path
//     prefix/full/regex/template, header and query parameter matches);
//   - actions: service, routeAction (weighted backend services, URL
//     rewrite, timeout, retries, ...), urlRedirect; header actions apply from
//     the most specific level (weighted backend, rule, path matcher, map).
//
// Validate reports the configuration errors GCP rejects, and RunTests runs
// a URL map's tests[] (urlMaps.validate, insert).
package urlmap

import (
	"fmt"
	"math/rand/v2"
	"net/http"
	"regexp"
	"sort"
	"strings"

	compute "google.golang.org/api/compute/v1"
)

// Request is the routing input.
type Request struct {
	// Scheme is "http" or "https".
	Scheme string
	Method string
	// Host is the Host header (or :authority), possibly with a port.
	Host string
	// Path is the URL path (without query).
	Path string
	// RawQuery is the query string without "?".
	RawQuery string
	Header   http.Header
}

// Redirect is a computed redirect response.
type Redirect struct {
	Code     int
	Location string
}

// Result is the routing decision.
type Result struct {
	// Redirect is set when the request is answered with a redirect.
	Redirect *Redirect
	// Service is the selected backend service or bucket (as written in
	// the URL map, normally a selfLink).
	Service string
	// Action is the effective route action (nil when none).
	Action *compute.HttpRouteAction
	// HeaderActions apply in order (most specific first).
	HeaderActions []*compute.HttpHeaderAction
	// Host and Path are the request host and path after URL rewrites.
	Host, Path string
	// ErrorPolicy is the effective custom error response policy.
	ErrorPolicy *compute.CustomErrorResponsePolicy
	// PathMatcher names the matched path matcher ("" for map defaults).
	PathMatcher string
}

// Router is a compiled URL map. It is immutable and safe for concurrent use.
type Router struct {
	m       *compute.UrlMap
	exact   map[string]*matcher // lower-cased host (with or without port)
	wild    []wildHost          // sorted longest suffix first
	star    *matcher
	byName  map[string]*matcher
	regexes map[string]*regexp.Regexp
	tmpls   map[string]*template
}

type wildHost struct {
	suffix string // after "*", e.g. ".example.com"
	m      *matcher
}

type matcher struct {
	pm    *compute.PathMatcher
	paths []pathEntry
	rules []*compute.HttpRouteRule // sorted by priority
}

type pathEntry struct {
	pattern string // without trailing "*"
	prefix  bool
	rule    *compute.PathRule
}

// Compile prepares m for routing. It fails on configurations Validate rejects.
func Compile(m *compute.UrlMap) (*Router, error) {
	if errs := Validate(m); len(errs) > 0 {
		return nil, fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return compile(m), nil
}

func compile(m *compute.UrlMap) *Router {
	rt := &Router{m: m, exact: map[string]*matcher{}, byName: map[string]*matcher{},
		regexes: map[string]*regexp.Regexp{}, tmpls: map[string]*template{}}
	for _, pm := range m.PathMatchers {
		mt := &matcher{pm: pm}
		for _, pr := range pm.PathRules {
			for _, p := range pr.Paths {
				e := pathEntry{pattern: p, rule: pr}
				if strings.HasSuffix(p, "*") {
					e.pattern, e.prefix = strings.TrimSuffix(p, "*"), true
				}
				mt.paths = append(mt.paths, e)
			}
		}
		sort.SliceStable(mt.paths, func(i, j int) bool {
			a, b := mt.paths[i], mt.paths[j]
			if len(a.pattern) != len(b.pattern) {
				return len(a.pattern) > len(b.pattern)
			}
			return !a.prefix && b.prefix
		})
		mt.rules = append(mt.rules, pm.RouteRules...)
		sort.SliceStable(mt.rules, func(i, j int) bool { return mt.rules[i].Priority < mt.rules[j].Priority })
		for _, rr := range pm.RouteRules {
			for _, mr := range rr.MatchRules {
				if mr.RegexMatch != "" {
					rt.regexes[mr.RegexMatch], _ = regexp.Compile(anchor(mr.RegexMatch))
				}
				if mr.PathTemplateMatch != "" {
					rt.tmpls[mr.PathTemplateMatch], _ = parseTemplate(mr.PathTemplateMatch)
				}
				for _, h := range mr.HeaderMatches {
					if h.RegexMatch != "" {
						rt.regexes[h.RegexMatch], _ = regexp.Compile(anchor(h.RegexMatch))
					}
				}
				for _, q := range mr.QueryParameterMatches {
					if q.RegexMatch != "" {
						rt.regexes[q.RegexMatch], _ = regexp.Compile(anchor(q.RegexMatch))
					}
				}
			}
			if a := rr.RouteAction; a != nil && a.UrlRewrite != nil && a.UrlRewrite.RegexRewrite != nil {
				rx := a.UrlRewrite.RegexRewrite.PathPattern
				rt.regexes[rx], _ = regexp.Compile(rx)
			}
		}
		rt.byName[pm.Name] = mt
	}
	for _, hr := range m.HostRules {
		mt := rt.byName[hr.PathMatcher]
		for _, h := range hr.Hosts {
			h = strings.ToLower(h)
			switch {
			case h == "*":
				rt.star = mt
			case strings.HasPrefix(h, "*"):
				rt.wild = append(rt.wild, wildHost{suffix: h[1:], m: mt})
			default:
				rt.exact[h] = mt
			}
		}
	}
	sort.SliceStable(rt.wild, func(i, j int) bool { return len(rt.wild[i].suffix) > len(rt.wild[j].suffix) })
	return rt
}

// anchor makes a regex match the whole input (RE2 full match).
func anchor(re string) string { return `^(?:` + re + `)$` }

// hostMatcher selects the path matcher for host.
func (rt *Router) hostMatcher(host string) *matcher {
	h := strings.ToLower(strings.TrimSuffix(host, "."))
	if m, ok := rt.exact[h]; ok {
		return m
	}
	bare := h
	if i := strings.LastIndexByte(h, ':'); i >= 0 && !strings.HasSuffix(h, "]") {
		bare = h[:i]
		if m, ok := rt.exact[bare]; ok {
			return m
		}
	}
	for _, w := range rt.wild {
		for _, cand := range []string{h, bare} {
			if strings.HasSuffix(cand, w.suffix) && len(cand) > len(w.suffix) {
				return w.m
			}
		}
	}
	return rt.star
}

// Route selects the action for req. pick chooses among weighted backends
// (nil uses a random choice proportional to weight).
func (rt *Router) Route(req *Request, pick func(total int64) int64) *Result {
	if pick == nil {
		pick = func(total int64) int64 { return rand.Int64N(total) }
	}
	res := &Result{Host: req.Host, Path: req.Path}
	mt := rt.hostMatcher(req.Host)
	if mt == nil {
		rt.apply(res, req, pick, rt.m.DefaultService, rt.m.DefaultRouteAction, rt.m.DefaultUrlRedirect, nil, matchInfo{kind: matchNone})
		res.ErrorPolicy = rt.m.DefaultCustomErrorResponsePolicy
		rt.addHeaderAction(res, rt.m.HeaderAction)
		return res
	}
	pm := mt.pm
	res.PathMatcher = pm.Name
	res.ErrorPolicy = rt.m.DefaultCustomErrorResponsePolicy
	if pm.DefaultCustomErrorResponsePolicy != nil {
		res.ErrorPolicy = pm.DefaultCustomErrorResponsePolicy
	}
	matched := false
	if len(mt.rules) > 0 {
		for _, rr := range mt.rules {
			if mi, ok := rt.matchRule(rr, req); ok {
				matched = true
				var ha []*compute.HttpHeaderAction
				ha = append(ha, rr.HeaderAction)
				rt.apply(res, req, pick, rr.Service, rr.RouteAction, rr.UrlRedirect, ha, mi)
				if rr.CustomErrorResponsePolicy != nil {
					res.ErrorPolicy = rr.CustomErrorResponsePolicy
				}
				break
			}
		}
	} else {
		for _, e := range mt.paths {
			if mi, ok := matchPath(e, req.Path); ok {
				matched = true
				rt.apply(res, req, pick, e.rule.Service, e.rule.RouteAction, e.rule.UrlRedirect, nil, mi)
				if e.rule.CustomErrorResponsePolicy != nil {
					res.ErrorPolicy = e.rule.CustomErrorResponsePolicy
				}
				break
			}
		}
	}
	if !matched {
		rt.apply(res, req, pick, pm.DefaultService, pm.DefaultRouteAction, pm.DefaultUrlRedirect, nil, matchInfo{kind: matchNone})
	}
	rt.addHeaderAction(res, pm.HeaderAction)
	rt.addHeaderAction(res, rt.m.HeaderAction)
	return res
}

func (rt *Router) addHeaderAction(res *Result, ha *compute.HttpHeaderAction) {
	if ha != nil {
		res.HeaderActions = append(res.HeaderActions, ha)
	}
}

// matchKind is how the path was matched (for prefix rewrites).
type matchKind int

const (
	matchNone matchKind = iota
	matchPrefix
	matchFull
	matchRegex
	matchTemplate
)

type matchInfo struct {
	kind   matchKind
	prefix string            // the matched portion for matchPrefix
	vars   map[string]string // template captures
}

func matchPath(e pathEntry, path string) (matchInfo, bool) {
	if e.prefix {
		if strings.HasPrefix(path, e.pattern) {
			return matchInfo{kind: matchPrefix, prefix: e.pattern}, true
		}
		return matchInfo{}, false
	}
	if path == e.pattern {
		return matchInfo{kind: matchFull}, true
	}
	return matchInfo{}, false
}

// matchRule reports whether any matchRule of rr matches req.
func (rt *Router) matchRule(rr *compute.HttpRouteRule, req *Request) (matchInfo, bool) {
	if len(rr.MatchRules) == 0 {
		return matchInfo{kind: matchPrefix, prefix: "/"}, true
	}
	for _, mr := range rr.MatchRules {
		if mi, ok := rt.matchOne(mr, req); ok {
			return mi, true
		}
	}
	return matchInfo{}, false
}

func (rt *Router) matchOne(mr *compute.HttpRouteRuleMatch, req *Request) (matchInfo, bool) {
	var mi matchInfo
	path := req.Path
	switch {
	case mr.PrefixMatch != "":
		p, pre := path, mr.PrefixMatch
		if mr.IgnoreCase {
			p, pre = strings.ToLower(p), strings.ToLower(pre)
		}
		if !strings.HasPrefix(p, pre) {
			return mi, false
		}
		mi = matchInfo{kind: matchPrefix, prefix: path[:len(mr.PrefixMatch)]}
	case mr.FullPathMatch != "":
		ok := path == mr.FullPathMatch
		if mr.IgnoreCase {
			ok = strings.EqualFold(path, mr.FullPathMatch)
		}
		if !ok {
			return mi, false
		}
		mi = matchInfo{kind: matchFull}
	case mr.RegexMatch != "":
		re := rt.regexes[mr.RegexMatch]
		if re == nil || !re.MatchString(path) {
			return mi, false
		}
		mi = matchInfo{kind: matchRegex}
	case mr.PathTemplateMatch != "":
		t := rt.tmpls[mr.PathTemplateMatch]
		if t == nil {
			return mi, false
		}
		vars, ok := t.match(path)
		if !ok {
			return mi, false
		}
		mi = matchInfo{kind: matchTemplate, vars: vars}
	default:
		mi = matchInfo{kind: matchPrefix, prefix: "/"}
	}
	for _, h := range mr.HeaderMatches {
		if !rt.matchHeader(h, req) {
			return mi, false
		}
	}
	if len(mr.QueryParameterMatches) > 0 {
		q := parseQuery(req.RawQuery)
		for _, qm := range mr.QueryParameterMatches {
			if !rt.matchQuery(qm, q) {
				return mi, false
			}
		}
	}
	return mi, true
}

// headerValue returns a request header (pseudo-headers included) and
// whether it is present.
func headerValue(req *Request, name string) (string, bool) {
	switch strings.ToLower(name) {
	case ":authority", "host":
		return req.Host, true
	case ":path":
		if req.RawQuery != "" {
			return req.Path + "?" + req.RawQuery, true
		}
		return req.Path, true
	case ":method":
		return req.Method, true
	case ":scheme":
		return req.Scheme, true
	}
	vs, ok := req.Header[http.CanonicalHeaderKey(name)]
	if !ok {
		return "", false
	}
	return strings.Join(vs, ","), true
}

func (rt *Router) matchHeader(h *compute.HttpHeaderMatch, req *Request) bool {
	v, present := headerValue(req, h.HeaderName)
	var ok bool
	switch {
	case h.ExactMatch != "":
		ok = present && v == h.ExactMatch
	case h.PrefixMatch != "":
		ok = present && strings.HasPrefix(v, h.PrefixMatch)
	case h.SuffixMatch != "":
		ok = present && strings.HasSuffix(v, h.SuffixMatch)
	case h.RegexMatch != "":
		re := rt.regexes[h.RegexMatch]
		ok = present && re != nil && re.MatchString(v)
	case h.RangeMatch != nil:
		var n int64
		_, err := fmt.Sscanf(strings.TrimSpace(v), "%d", &n)
		ok = present && err == nil && fmt.Sprint(n) == strings.TrimSpace(v) &&
			n >= h.RangeMatch.RangeStart && n < h.RangeMatch.RangeEnd
	case h.PresentMatch:
		ok = present
	default:
		ok = present
	}
	if h.InvertMatch {
		return !ok
	}
	return ok
}

func (rt *Router) matchQuery(qm *compute.HttpQueryParameterMatch, q map[string][]string) bool {
	vs, present := q[qm.Name]
	switch {
	case qm.PresentMatch:
		return present
	case qm.ExactMatch != "":
		return present && vs[0] == qm.ExactMatch
	case qm.RegexMatch != "":
		re := rt.regexes[qm.RegexMatch]
		return present && re != nil && re.MatchString(vs[0])
	}
	return present
}

// parseQuery splits a raw query without unescaping errors failing the match.
func parseQuery(raw string) map[string][]string {
	out := map[string][]string{}
	for _, kv := range strings.Split(raw, "&") {
		if kv == "" {
			continue
		}
		k, v, _ := strings.Cut(kv, "=")
		k, v = unescape(k), unescape(v)
		out[k] = append(out[k], v)
	}
	return out
}

func unescape(s string) string {
	s = strings.ReplaceAll(s, "+", " ")
	if !strings.Contains(s, "%") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) && isHex(s[i+1]) && isHex(s[i+2]) {
			b.WriteByte(unhex(s[i+1])<<4 | unhex(s[i+2]))
			i += 2
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func isHex(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

func unhex(c byte) byte {
	switch {
	case c >= '0' && c <= '9':
		return c - '0'
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10
	}
	return c - 'A' + 10
}

// apply fills res from the selected action.
func (rt *Router) apply(res *Result, req *Request, pick func(int64) int64, service string, action *compute.HttpRouteAction, redirect *compute.HttpRedirectAction, ha []*compute.HttpHeaderAction, mi matchInfo) {
	if redirect != nil {
		res.Redirect = computeRedirect(req, redirect, mi)
		for _, h := range ha {
			rt.addHeaderAction(res, h)
		}
		return
	}
	res.Service = service
	res.Action = action
	var wbsAction *compute.HttpHeaderAction
	if action != nil && len(action.WeightedBackendServices) > 0 {
		w := pickWeighted(action.WeightedBackendServices, pick)
		res.Service = w.BackendService
		wbsAction = w.HeaderAction
	}
	if wbsAction != nil {
		rt.addHeaderAction(res, wbsAction)
	}
	for _, h := range ha {
		rt.addHeaderAction(res, h)
	}
	if action != nil && action.UrlRewrite != nil {
		rt.rewrite(res, req, action.UrlRewrite, mi)
	}
}

// pickWeighted chooses a backend proportionally to weight.
func pickWeighted(ws []*compute.WeightedBackendService, pick func(int64) int64) *compute.WeightedBackendService {
	var total int64
	for _, w := range ws {
		total += w.Weight
	}
	if total <= 0 {
		return ws[0]
	}
	n := pick(total)
	for _, w := range ws {
		if n < w.Weight {
			return w
		}
		n -= w.Weight
	}
	return ws[len(ws)-1]
}

func (rt *Router) rewrite(res *Result, req *Request, u *compute.UrlRewrite, mi matchInfo) {
	if u.HostRewrite != "" {
		res.Host = u.HostRewrite
	}
	switch {
	case u.PathPrefixRewrite != "":
		switch mi.kind {
		case matchPrefix:
			res.Path = u.PathPrefixRewrite + req.Path[len(mi.prefix):]
		case matchFull:
			res.Path = u.PathPrefixRewrite
		case matchNone:
			// Defaults route on the prefix "/".
			res.Path = u.PathPrefixRewrite + strings.TrimPrefix(req.Path, "/")
		}
	case u.PathTemplateRewrite != "" && mi.kind == matchTemplate:
		res.Path = expandTemplate(u.PathTemplateRewrite, mi.vars)
	case u.RegexRewrite != nil && u.RegexRewrite.PathPattern != "":
		if re := rt.regexes[u.RegexRewrite.PathPattern]; re != nil {
			res.Path = re.ReplaceAllString(req.Path, convertSubst(u.RegexRewrite.PathSubstitution))
		}
	}
	if res.Path == "" {
		res.Path = "/"
	}
}

// convertSubst turns RE2 "\1" substitutions into Go's "${1}".
func convertSubst(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) && s[i+1] >= '0' && s[i+1] <= '9' {
			b.WriteString("${" + string(s[i+1]) + "}")
			i++
			continue
		}
		if s[i] == '$' {
			b.WriteString("$$")
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// RedirectCode maps redirectResponseCode to an HTTP status.
func RedirectCode(c string) int {
	switch c {
	case "FOUND":
		return http.StatusFound
	case "SEE_OTHER":
		return http.StatusSeeOther
	case "TEMPORARY_REDIRECT":
		return http.StatusTemporaryRedirect
	case "PERMANENT_REDIRECT":
		return http.StatusPermanentRedirect
	}
	return http.StatusMovedPermanently
}

func computeRedirect(req *Request, r *compute.HttpRedirectAction, mi matchInfo) *Redirect {
	scheme := req.Scheme
	if scheme == "" {
		scheme = "http"
	}
	host := req.Host
	if r.HttpsRedirect {
		if scheme != "https" {
			host = stripPort(host)
		}
		scheme = "https"
	}
	if r.HostRedirect != "" {
		host = r.HostRedirect
	}
	path := req.Path
	switch {
	case r.PathRedirect != "":
		path = r.PathRedirect
	case r.PrefixRedirect != "":
		switch mi.kind {
		case matchPrefix:
			path = r.PrefixRedirect + req.Path[len(mi.prefix):]
		case matchNone:
			path = r.PrefixRedirect + strings.TrimPrefix(req.Path, "/")
		default:
			path = r.PrefixRedirect
		}
	}
	loc := scheme + "://" + host + path
	if !r.StripQuery && req.RawQuery != "" {
		if strings.Contains(path, "?") {
			loc += "&" + req.RawQuery
		} else {
			loc += "?" + req.RawQuery
		}
	}
	return &Redirect{Code: RedirectCode(r.RedirectResponseCode), Location: loc}
}

func stripPort(h string) string {
	if strings.HasSuffix(h, "]") {
		return h
	}
	if i := strings.LastIndexByte(h, ':'); i >= 0 {
		return h[:i]
	}
	return h
}
