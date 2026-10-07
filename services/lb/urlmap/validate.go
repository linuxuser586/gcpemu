package urlmap

import (
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	compute "google.golang.org/api/compute/v1"
)

// WalkServices calls fn for every backend reference in m (default
// services, rule services, weighted backends, mirrors, error services and
// test expectations) with the request field path, so callers can
// canonicalise and check them.
func WalkServices(m *compute.UrlMap, fn func(field string, ref *string) error) error {
	visitAction := func(f string, a *compute.HttpRouteAction) error {
		if a == nil {
			return nil
		}
		for i, w := range a.WeightedBackendServices {
			if err := fn(fmt.Sprintf("%s.weightedBackendServices[%d].backendService", f, i), &w.BackendService); err != nil {
				return err
			}
		}
		if a.RequestMirrorPolicy != nil && a.RequestMirrorPolicy.BackendService != "" {
			if err := fn(f+".requestMirrorPolicy.backendService", &a.RequestMirrorPolicy.BackendService); err != nil {
				return err
			}
		}
		return nil
	}
	visitErr := func(f string, p *compute.CustomErrorResponsePolicy) error {
		if p != nil && p.ErrorService != "" {
			return fn(f+".errorService", &p.ErrorService)
		}
		return nil
	}
	opt := func(f string, s *string) error {
		if *s != "" {
			return fn(f, s)
		}
		return nil
	}
	if err := opt("resource.defaultService", &m.DefaultService); err != nil {
		return err
	}
	if err := visitAction("resource.defaultRouteAction", m.DefaultRouteAction); err != nil {
		return err
	}
	if err := visitErr("resource.defaultCustomErrorResponsePolicy", m.DefaultCustomErrorResponsePolicy); err != nil {
		return err
	}
	for i, pm := range m.PathMatchers {
		pf := fmt.Sprintf("resource.pathMatchers[%d]", i)
		if err := opt(pf+".defaultService", &pm.DefaultService); err != nil {
			return err
		}
		if err := visitAction(pf+".defaultRouteAction", pm.DefaultRouteAction); err != nil {
			return err
		}
		if err := visitErr(pf+".defaultCustomErrorResponsePolicy", pm.DefaultCustomErrorResponsePolicy); err != nil {
			return err
		}
		for j, pr := range pm.PathRules {
			rf := fmt.Sprintf("%s.pathRules[%d]", pf, j)
			if err := opt(rf+".service", &pr.Service); err != nil {
				return err
			}
			if err := visitAction(rf+".routeAction", pr.RouteAction); err != nil {
				return err
			}
			if err := visitErr(rf+".customErrorResponsePolicy", pr.CustomErrorResponsePolicy); err != nil {
				return err
			}
		}
		for j, rr := range pm.RouteRules {
			rf := fmt.Sprintf("%s.routeRules[%d]", pf, j)
			if err := opt(rf+".service", &rr.Service); err != nil {
				return err
			}
			if err := visitAction(rf+".routeAction", rr.RouteAction); err != nil {
				return err
			}
			if err := visitErr(rf+".customErrorResponsePolicy", rr.CustomErrorResponsePolicy); err != nil {
				return err
			}
		}
	}
	for i, t := range m.Tests {
		if err := opt(fmt.Sprintf("resource.tests[%d].service", i), &t.Service); err != nil {
			return err
		}
	}
	return nil
}

var (
	pmNameRE = regexp.MustCompile(`^[a-z](?:[-a-z0-9]{0,61}[a-z0-9])?$`)
	hostRE   = regexp.MustCompile(`^(\*|\*[-.][-a-zA-Z0-9.]*|[a-zA-Z0-9][-a-zA-Z0-9.]*)(:[0-9]{1,5})?$`)
)

// Validate returns the errors GCP reports for an invalid URL map (empty
// when valid).
func Validate(m *compute.UrlMap) []string {
	var errs []string
	add := func(f string, a ...any) { errs = append(errs, fmt.Sprintf(f, a...)) }

	checkDefault := func(where, svc string, a *compute.HttpRouteAction, r *compute.HttpRedirectAction, required bool) {
		hasWeighted := a != nil && len(a.WeightedBackendServices) > 0
		switch {
		case svc != "" && r != nil:
			add("Invalid value for field '%s': only one of defaultService and defaultUrlRedirect may be set.", where)
		case svc != "" && hasWeighted:
			add("Invalid value for field '%s': defaultService must not be set when defaultRouteAction specifies weightedBackendServices.", where)
		case r != nil && a != nil:
			add("Invalid value for field '%s': defaultRouteAction must not be set with defaultUrlRedirect.", where)
		case required && svc == "" && r == nil && !hasWeighted:
			add("Invalid value for field '%s': a default service, default route action with weighted backend services, or default URL redirect is required.", where)
		}
		checkAction(where+".defaultRouteAction", a, matchNone, add)
		checkRedirect(where+".defaultUrlRedirect", r, add)
	}
	checkDefault("resource", m.DefaultService, m.DefaultRouteAction, m.DefaultUrlRedirect, true)
	checkHeaderAction("resource.headerAction", m.HeaderAction, add)

	names := map[string]bool{}
	for i, pm := range m.PathMatchers {
		pf := fmt.Sprintf("resource.pathMatchers[%d]", i)
		if !pmNameRE.MatchString(pm.Name) {
			add("Invalid value for field '%s.name': '%s'. Must be a match of regex '(?:[a-z](?:[-a-z0-9]{0,61}[a-z0-9])?)'", pf, pm.Name)
		}
		if names[pm.Name] {
			add("Invalid value for field '%s.name': '%s'. Duplicate path matcher name.", pf, pm.Name)
		}
		names[pm.Name] = true
		checkDefault(pf, pm.DefaultService, pm.DefaultRouteAction, pm.DefaultUrlRedirect, true)
		checkHeaderAction(pf+".headerAction", pm.HeaderAction, add)
		if len(pm.PathRules) > 0 && len(pm.RouteRules) > 0 {
			add("Invalid value for field '%s': pathRules and routeRules cannot both be specified.", pf)
		}
		seen := map[string]bool{}
		for j, pr := range pm.PathRules {
			rf := fmt.Sprintf("%s.pathRules[%d]", pf, j)
			if len(pr.Paths) == 0 {
				add("Invalid value for field '%s.paths': ''. At least one path is required.", rf)
			}
			for _, p := range pr.Paths {
				if !validRulePath(p) {
					add("Invalid value for field '%s.paths': '%s'. Paths must start with / and may only contain * at the end, following a /.", rf, p)
				}
				if seen[p] {
					add("Invalid value for field '%s.paths': '%s'. Duplicate path in path matcher '%s'.", rf, p, pm.Name)
				}
				seen[p] = true
			}
			checkRuleAction(rf, pr.Service, pr.RouteAction, pr.UrlRedirect, matchPrefix, add)
		}
		prios := map[int64]bool{}
		for j, rr := range pm.RouteRules {
			rf := fmt.Sprintf("%s.routeRules[%d]", pf, j)
			if rr.Priority < 0 || rr.Priority > 2147483647 {
				add("Invalid value for field '%s.priority': '%d'. Must be between 0 and 2147483647.", rf, rr.Priority)
			}
			if prios[rr.Priority] {
				add("Invalid value for field '%s.priority': '%d'. Priorities of route rules in a path matcher must be unique.", rf, rr.Priority)
			}
			prios[rr.Priority] = true
			kinds := map[matchKind]bool{}
			for k, mr := range rr.MatchRules {
				kinds[checkMatch(fmt.Sprintf("%s.matchRules[%d]", rf, k), mr, add)] = true
			}
			mk := matchPrefix
			if len(kinds) == 1 {
				for k := range kinds {
					mk = k
				}
			} else if len(kinds) > 1 {
				mk = matchNone
			}
			checkRuleAction(rf, rr.Service, rr.RouteAction, rr.UrlRedirect, mk, add)
			checkHeaderAction(rf+".headerAction", rr.HeaderAction, add)
			if a := rr.RouteAction; a != nil && a.UrlRewrite != nil && a.UrlRewrite.PathTemplateRewrite != "" {
				vars, err := rewriteVars(a.UrlRewrite.PathTemplateRewrite)
				if err != nil {
					add("Invalid value for field '%s.routeAction.urlRewrite.pathTemplateRewrite': '%s'. %v", rf, a.UrlRewrite.PathTemplateRewrite, err)
				}
				for _, mr := range rr.MatchRules {
					t, err := parseTemplate(mr.PathTemplateMatch)
					if err != nil {
						continue
					}
					for _, v := range vars {
						if !contains(t.vars, v) {
							add("Invalid value for field '%s.routeAction.urlRewrite.pathTemplateRewrite': '%s'. Variable '%s' is not captured by pathTemplateMatch '%s'.", rf, a.UrlRewrite.PathTemplateRewrite, v, mr.PathTemplateMatch)
						}
					}
				}
			}
		}
	}
	hosts := map[string]bool{}
	for i, hr := range m.HostRules {
		hf := fmt.Sprintf("resource.hostRules[%d]", i)
		if len(hr.Hosts) == 0 {
			add("Invalid value for field '%s.hosts': ''. At least one host is required.", hf)
		}
		for _, h := range hr.Hosts {
			if !hostRE.MatchString(h) {
				add("Invalid value for field '%s.hosts': '%s'. Invalid host.", hf, h)
			}
			lh := strings.ToLower(h)
			if hosts[lh] {
				add("Invalid value for field '%s.hosts': '%s'. Duplicate host in host rules.", hf, h)
			}
			hosts[lh] = true
		}
		if !names[hr.PathMatcher] {
			add("Invalid value for field '%s.pathMatcher': '%s'. The path matcher does not exist.", hf, hr.PathMatcher)
		}
	}
	return errs
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

func validRulePath(p string) bool {
	if !strings.HasPrefix(p, "/") || strings.ContainsAny(p, "?#") {
		return false
	}
	if i := strings.IndexByte(p, '*'); i >= 0 {
		return i == len(p)-1 && strings.HasSuffix(p, "/*")
	}
	return true
}

func checkRuleAction(rf, svc string, a *compute.HttpRouteAction, r *compute.HttpRedirectAction, mk matchKind, add func(string, ...any)) {
	hasWeighted := a != nil && len(a.WeightedBackendServices) > 0
	switch {
	case r != nil && (svc != "" || a != nil):
		add("Invalid value for field '%s': urlRedirect cannot be combined with service or routeAction.", rf)
	case svc != "" && hasWeighted:
		add("Invalid value for field '%s': service must not be set when routeAction specifies weightedBackendServices.", rf)
	case svc == "" && r == nil && !hasWeighted:
		add("Invalid value for field '%s': one of service, routeAction.weightedBackendServices or urlRedirect is required.", rf)
	}
	checkAction(rf+".routeAction", a, mk, add)
	checkRedirect(rf+".urlRedirect", r, add)
}

func checkAction(f string, a *compute.HttpRouteAction, mk matchKind, add func(string, ...any)) {
	if a == nil {
		return
	}
	for i, w := range a.WeightedBackendServices {
		if w.Weight < 0 || w.Weight > 1000 {
			add("Invalid value for field '%s.weightedBackendServices[%d].weight': '%d'. Must be between 0 and 1000.", f, i, w.Weight)
		}
		checkHeaderAction(fmt.Sprintf("%s.weightedBackendServices[%d].headerAction", f, i), w.HeaderAction, add)
	}
	if u := a.UrlRewrite; u != nil {
		n := 0
		for _, v := range []string{u.PathPrefixRewrite, u.PathTemplateRewrite} {
			if v != "" {
				n++
			}
		}
		if u.RegexRewrite != nil {
			n++
			if _, err := regexp.Compile(u.RegexRewrite.PathPattern); err != nil {
				add("Invalid value for field '%s.urlRewrite.regexRewrite.pathPattern': '%s'. %v", f, u.RegexRewrite.PathPattern, err)
			}
		}
		if n > 1 {
			add("Invalid value for field '%s.urlRewrite': only one of pathPrefixRewrite, pathTemplateRewrite and regexRewrite may be set.", f)
		}
		if u.PathPrefixRewrite != "" && (mk == matchRegex || mk == matchTemplate) {
			add("Invalid value for field '%s.urlRewrite.pathPrefixRewrite': '%s'. pathPrefixRewrite requires prefixMatch or fullPathMatch.", f, u.PathPrefixRewrite)
		}
		if u.PathTemplateRewrite != "" && mk != matchTemplate {
			add("Invalid value for field '%s.urlRewrite.pathTemplateRewrite': '%s'. pathTemplateRewrite requires pathTemplateMatch.", f, u.PathTemplateRewrite)
		}
		if len(u.PathPrefixRewrite) > 1024 || len(u.HostRewrite) > 255 {
			add("Invalid value for field '%s.urlRewrite': rewrite is too long.", f)
		}
	}
	if rp := a.RetryPolicy; rp != nil {
		for _, c := range rp.RetryConditions {
			switch c {
			case "5xx", "gateway-error", "connect-failure", "retriable-4xx", "refused-stream", "cancelled", "deadline-exceeded", "internal", "resource-exhausted", "unavailable":
			default:
				add("Invalid value for field '%s.retryPolicy.retryConditions': '%s'.", f, c)
			}
		}
	}
	if fi := a.FaultInjectionPolicy; fi != nil {
		if fi.Abort != nil && (fi.Abort.HttpStatus < 200 || fi.Abort.HttpStatus > 599) {
			add("Invalid value for field '%s.faultInjectionPolicy.abort.httpStatus': '%d'. Must be between 200 and 599.", f, fi.Abort.HttpStatus)
		}
	}
}

func checkRedirect(f string, r *compute.HttpRedirectAction, add func(string, ...any)) {
	if r == nil {
		return
	}
	if r.PathRedirect != "" && r.PrefixRedirect != "" {
		add("Invalid value for field '%s': pathRedirect and prefixRedirect cannot both be set.", f)
	}
	switch r.RedirectResponseCode {
	case "", "MOVED_PERMANENTLY_DEFAULT", "FOUND", "SEE_OTHER", "TEMPORARY_REDIRECT", "PERMANENT_REDIRECT":
	default:
		add("Invalid value for field '%s.redirectResponseCode': '%s'.", f, r.RedirectResponseCode)
	}
	for _, p := range []string{r.PathRedirect, r.PrefixRedirect} {
		if p != "" && !strings.HasPrefix(p, "/") {
			add("Invalid value for field '%s': '%s'. Redirect paths must start with /.", f, p)
		}
	}
}

func checkHeaderAction(f string, h *compute.HttpHeaderAction, add func(string, ...any)) {
	if h == nil {
		return
	}
	for _, o := range append(append([]*compute.HttpHeaderOption{}, h.RequestHeadersToAdd...), h.ResponseHeadersToAdd...) {
		if o.HeaderName == "" || strings.ContainsAny(o.HeaderName, " :\t\r\n") {
			add("Invalid value for field '%s': '%s'. Invalid header name.", f, o.HeaderName)
		}
	}
}

// checkMatch validates a match rule and returns its path match kind.
func checkMatch(f string, mr *compute.HttpRouteRuleMatch, add func(string, ...any)) matchKind {
	n := 0
	mk := matchPrefix
	if mr.PrefixMatch != "" {
		n++
		if !strings.HasPrefix(mr.PrefixMatch, "/") {
			add("Invalid value for field '%s.prefixMatch': '%s'. Must start with /.", f, mr.PrefixMatch)
		}
	}
	if mr.FullPathMatch != "" {
		n++
		mk = matchFull
		if !strings.HasPrefix(mr.FullPathMatch, "/") {
			add("Invalid value for field '%s.fullPathMatch': '%s'. Must start with /.", f, mr.FullPathMatch)
		}
	}
	if mr.RegexMatch != "" {
		n++
		mk = matchRegex
		if _, err := regexp.Compile(mr.RegexMatch); err != nil {
			add("Invalid value for field '%s.regexMatch': '%s'. %v", f, mr.RegexMatch, err)
		}
	}
	if mr.PathTemplateMatch != "" {
		n++
		mk = matchTemplate
		if _, err := parseTemplate(mr.PathTemplateMatch); err != nil {
			add("Invalid value for field '%s.pathTemplateMatch': '%s'. %v", f, mr.PathTemplateMatch, err)
		}
	}
	if n > 1 {
		add("Invalid value for field '%s': only one of prefixMatch, fullPathMatch, regexMatch and pathTemplateMatch may be set.", f)
	}
	for i, h := range mr.HeaderMatches {
		k := 0
		for _, set := range []bool{h.ExactMatch != "", h.PrefixMatch != "", h.SuffixMatch != "", h.RegexMatch != "", h.RangeMatch != nil, h.PresentMatch} {
			if set {
				k++
			}
		}
		if k > 1 {
			add("Invalid value for field '%s.headerMatches[%d]': only one match type may be set.", f, i)
		}
		if h.HeaderName == "" {
			add("Required field '%s.headerMatches[%d].headerName' not specified", f, i)
		}
		if h.RegexMatch != "" {
			if _, err := regexp.Compile(h.RegexMatch); err != nil {
				add("Invalid value for field '%s.headerMatches[%d].regexMatch': '%s'. %v", f, i, h.RegexMatch, err)
			}
		}
	}
	for i, q := range mr.QueryParameterMatches {
		k := 0
		for _, set := range []bool{q.ExactMatch != "", q.RegexMatch != "", q.PresentMatch} {
			if set {
				k++
			}
		}
		if k != 1 {
			add("Invalid value for field '%s.queryParameterMatches[%d]': exactly one of presentMatch, exactMatch and regexMatch must be set.", f, i)
		}
		if q.RegexMatch != "" {
			if _, err := regexp.Compile(q.RegexMatch); err != nil {
				add("Invalid value for field '%s.queryParameterMatches[%d].regexMatch': '%s'. %v", f, i, q.RegexMatch, err)
			}
		}
	}
	return mk
}

// RunTests evaluates m's tests[] and returns the failures. same reports
// whether two service references name the same resource.
func RunTests(m *compute.UrlMap, same func(a, b string) bool) []*compute.TestFailure {
	if same == nil {
		same = func(a, b string) bool { return a == b }
	}
	rt := compile(m)
	var out []*compute.TestFailure
	for _, t := range m.Tests {
		host, path, raw := t.Host, t.Path, ""
		if i := strings.IndexByte(path, '?'); i >= 0 {
			path, raw = path[:i], path[i+1:]
		}
		h := map[string][]string{}
		var th []*compute.UrlMapTestHeader
		for _, x := range t.Headers {
			h[canonical(x.Name)] = append(h[canonical(x.Name)], x.Value)
			th = append(th, x)
		}
		scheme := "http"
		res := rt.Route(&Request{Scheme: scheme, Method: "GET", Host: host, Path: path, RawQuery: raw, Header: h}, func(int64) int64 { return 0 })
		f := &compute.TestFailure{Host: t.Host, Path: t.Path, Headers: th}
		failed := false
		if t.Service != "" {
			f.ExpectedService = t.Service
			f.ActualService = res.Service
			if res.Redirect != nil || !same(res.Service, t.Service) {
				failed = true
			}
		}
		if t.ExpectedOutputUrl != "" {
			actual := outputURL(res, scheme)
			f.ExpectedOutputUrl, f.ActualOutputUrl = t.ExpectedOutputUrl, actual
			if !sameURL(t.ExpectedOutputUrl, actual, res.Redirect != nil) {
				failed = true
			}
		}
		if t.ExpectedRedirectResponseCode != 0 {
			f.ExpectedRedirectResponseCode = t.ExpectedRedirectResponseCode
			if res.Redirect != nil {
				f.ActualRedirectResponseCode = int64(res.Redirect.Code)
			}
			if f.ActualRedirectResponseCode != t.ExpectedRedirectResponseCode {
				failed = true
			}
		}
		if failed {
			out = append(out, f)
		}
	}
	return out
}

func canonical(s string) string { return http.CanonicalHeaderKey(s) }

// outputURL renders the routed URL (the redirect location, or the
// rewritten request URL).
func outputURL(res *Result, scheme string) string {
	if res.Redirect != nil {
		return res.Redirect.Location
	}
	return scheme + "://" + res.Host + res.Path
}

// sameURL compares an expectedOutputUrl (which may omit the scheme for
// non-redirects) with the actual URL.
func sameURL(expected, actual string, redirect bool) bool {
	if expected == actual {
		return true
	}
	e, err1 := url.Parse(expected)
	a, err2 := url.Parse(actual)
	if err1 != nil || err2 != nil {
		return false
	}
	if !redirect && e.Scheme == "" {
		e2, err := url.Parse("http://" + expected)
		if err != nil {
			return false
		}
		e = e2
		a.Scheme = "http"
	}
	return e.Scheme == a.Scheme && e.Host == a.Host && e.Path == a.Path && e.RawQuery == a.RawQuery
}
