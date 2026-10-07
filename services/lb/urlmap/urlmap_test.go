package urlmap

import (
	"net/http"
	"strings"
	"testing"

	compute "google.golang.org/api/compute/v1"
)

func refMap() *compute.UrlMap {
	return &compute.UrlMap{
		Name:           "m",
		DefaultService: "default-bs",
		HeaderAction: &compute.HttpHeaderAction{
			ResponseHeadersToAdd: []*compute.HttpHeaderOption{{HeaderName: "X-Level", HeaderValue: "map"}},
		},
		HostRules: []*compute.HostRule{
			{Hosts: []string{"example.com"}, PathMatcher: "paths"},
			{Hosts: []string{"*.example.com"}, PathMatcher: "wild"},
			{Hosts: []string{"*.a.example.com"}, PathMatcher: "wild-longer"},
			{Hosts: []string{"api.example.com:8080"}, PathMatcher: "routes"},
			{Hosts: []string{"api.example.com"}, PathMatcher: "routes"},
			{Hosts: []string{"redirect.test"}, PathMatcher: "redir"},
		},
		PathMatchers: []*compute.PathMatcher{
			{
				Name:           "paths",
				DefaultService: "paths-default",
				PathRules: []*compute.PathRule{
					{Paths: []string{"/static/*"}, Service: "static"},
					{Paths: []string{"/static/special"}, Service: "special"},
					{Paths: []string{"/static/deep/*"}, Service: "deep"},
					{Paths: []string{"/exact"}, Service: "exact"},
					{Paths: []string{"/v1/*"}, Service: "v1", RouteAction: &compute.HttpRouteAction{
						UrlRewrite: &compute.UrlRewrite{PathPrefixRewrite: "/", HostRewrite: "backend.internal"}}},
				},
			},
			{Name: "wild", DefaultService: "wild"},
			{Name: "wild-longer", DefaultService: "wild-longer"},
			{
				Name:           "routes",
				DefaultService: "routes-default",
				HeaderAction: &compute.HttpHeaderAction{
					RequestHeadersToAdd: []*compute.HttpHeaderOption{{HeaderName: "X-Matcher", HeaderValue: "routes"}},
				},
				RouteRules: []*compute.HttpRouteRule{
					{Priority: 20, MatchRules: []*compute.HttpRouteRuleMatch{{PrefixMatch: "/api/"}}, Service: "api"},
					{Priority: 10, MatchRules: []*compute.HttpRouteRuleMatch{{
						PrefixMatch:   "/api/",
						HeaderMatches: []*compute.HttpHeaderMatch{{HeaderName: "x-canary", ExactMatch: "1"}},
					}}, Service: "canary"},
					{Priority: 5, MatchRules: []*compute.HttpRouteRuleMatch{{
						FullPathMatch: "/Health", IgnoreCase: true,
					}}, Service: "health"},
					{Priority: 30, MatchRules: []*compute.HttpRouteRuleMatch{{
						RegexMatch: `/items/[0-9]+`,
					}}, Service: "items"},
					{Priority: 40, MatchRules: []*compute.HttpRouteRuleMatch{{
						PrefixMatch:           "/q",
						QueryParameterMatches: []*compute.HttpQueryParameterMatch{{Name: "mode", ExactMatch: "fast"}},
					}, {
						PrefixMatch:           "/q",
						QueryParameterMatches: []*compute.HttpQueryParameterMatch{{Name: "debug", PresentMatch: true}},
					}}, Service: "query"},
					{Priority: 50, MatchRules: []*compute.HttpRouteRuleMatch{{
						PathTemplateMatch: "/videos/{format}/{id=**}",
					}}, Service: "videos", RouteAction: &compute.HttpRouteAction{
						UrlRewrite: &compute.UrlRewrite{PathTemplateRewrite: "/{id}.{format}"}}},
					{Priority: 60, MatchRules: []*compute.HttpRouteRuleMatch{{
						PrefixMatch: "/hdr",
						HeaderMatches: []*compute.HttpHeaderMatch{
							{HeaderName: "x-num", RangeMatch: &compute.Int64RangeMatch{RangeStart: 10, RangeEnd: 20}},
							{HeaderName: "x-absent", PresentMatch: true, InvertMatch: true},
							{HeaderName: "user-agent", SuffixMatch: "bot"},
						},
					}}, Service: "hdr"},
					{Priority: 70, MatchRules: []*compute.HttpRouteRuleMatch{{PrefixMatch: "/split"}},
						RouteAction: &compute.HttpRouteAction{WeightedBackendServices: []*compute.WeightedBackendService{
							{BackendService: "blue", Weight: 90, HeaderAction: &compute.HttpHeaderAction{
								RequestHeadersToAdd: []*compute.HttpHeaderOption{{HeaderName: "X-Color", HeaderValue: "blue"}}}},
							{BackendService: "green", Weight: 10},
						}},
						HeaderAction: &compute.HttpHeaderAction{
							RequestHeadersToAdd: []*compute.HttpHeaderOption{{HeaderName: "X-Rule", HeaderValue: "split"}}}},
					{Priority: 80, MatchRules: []*compute.HttpRouteRuleMatch{{PrefixMatch: "/old/"}},
						UrlRedirect: &compute.HttpRedirectAction{PrefixRedirect: "/new/", RedirectResponseCode: "FOUND"}},
				},
			},
			{
				Name:               "redir",
				DefaultUrlRedirect: &compute.HttpRedirectAction{HttpsRedirect: true, StripQuery: false},
			},
		},
	}
}

func TestRouting(t *testing.T) {
	rt, err := Compile(refMap())
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, host, path, query string
		hdr                     map[string]string
		pick                    int64
		want                    string // service, or "redirect <code> <location>"
		wantPath, wantHost      string
		wantHeaders             []string // header names added, in order of actions
	}{
		{name: "no host match uses map default", host: "other.test", path: "/x", want: "default-bs"},
		{name: "exact host default path", host: "example.com", path: "/nothing", want: "paths-default"},
		{name: "host is case insensitive and port stripped", host: "EXAMPLE.com:443", path: "/exact", want: "exact"},
		{name: "prefix rule", host: "example.com", path: "/static/app.js", want: "static"},
		{name: "prefix includes trailing slash", host: "example.com", path: "/static/", want: "static"},
		{name: "prefix does not match bare", host: "example.com", path: "/static", want: "paths-default"},
		{name: "exact beats shorter prefix", host: "example.com", path: "/static/special", want: "special"},
		{name: "longest prefix wins", host: "example.com", path: "/static/deep/x", want: "deep"},
		{name: "exact path only", host: "example.com", path: "/exact/more", want: "paths-default"},
		{name: "path rule rewrite", host: "example.com", path: "/v1/users", want: "v1", wantPath: "/users", wantHost: "backend.internal"},
		{name: "wildcard host", host: "www.example.com", path: "/", want: "wild"},
		{name: "longer wildcard wins", host: "x.a.example.com", path: "/", want: "wild-longer"},
		{name: "wildcard needs a label", host: ".example.com", path: "/", want: "default-bs"},
		{name: "host with port exact", host: "api.example.com:8080", path: "/api/x", want: "api"},
		{name: "priority order header canary", host: "api.example.com", path: "/api/x", hdr: map[string]string{"X-Canary": "1"}, want: "canary"},
		{name: "header mismatch falls to next", host: "api.example.com", path: "/api/x", hdr: map[string]string{"X-Canary": "2"}, want: "api"},
		{name: "full path ignore case", host: "api.example.com", path: "/HEALTH", want: "health"},
		{name: "full path no prefix", host: "api.example.com", path: "/health/x", want: "routes-default"},
		{name: "regex full match", host: "api.example.com", path: "/items/42", want: "items"},
		{name: "regex anchored", host: "api.example.com", path: "/items/42/x", want: "routes-default"},
		{name: "query exact", host: "api.example.com", path: "/q", query: "mode=fast", want: "query"},
		{name: "query OR second rule", host: "api.example.com", path: "/q", query: "debug", want: "query"},
		{name: "query mismatch", host: "api.example.com", path: "/q", query: "mode=slow", want: "routes-default"},
		{name: "template rewrite", host: "api.example.com", path: "/videos/mp4/a/b", want: "videos", wantPath: "/a/b.mp4"},
		{name: "template no match", host: "api.example.com", path: "/videos", want: "routes-default"},
		{name: "headers range/invert/suffix", host: "api.example.com", path: "/hdr", hdr: map[string]string{"X-Num": "15", "User-Agent": "googlebot"}, want: "hdr"},
		{name: "range end exclusive", host: "api.example.com", path: "/hdr", hdr: map[string]string{"X-Num": "20", "User-Agent": "googlebot"}, want: "routes-default"},
		{name: "invert present", host: "api.example.com", path: "/hdr", hdr: map[string]string{"X-Num": "15", "User-Agent": "googlebot", "X-Absent": ""}, want: "routes-default"},
		{name: "weighted first", host: "api.example.com", path: "/split", pick: 89, want: "blue", wantHeaders: []string{"X-Color", "X-Rule", "X-Matcher", "X-Level"}},
		{name: "weighted second", host: "api.example.com", path: "/split", pick: 90, want: "green", wantHeaders: []string{"X-Rule", "X-Matcher", "X-Level"}},
		{name: "prefix redirect", host: "api.example.com", path: "/old/page", query: "a=1", want: "redirect 302 http://api.example.com/new/page?a=1"},
		{name: "https redirect strips port", host: "redirect.test:8080", path: "/p", query: "q=1", want: "redirect 301 https://redirect.test/p?q=1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := http.Header{}
			for k, v := range c.hdr {
				h.Set(k, v)
			}
			res := rt.Route(&Request{Scheme: "http", Method: "GET", Host: c.host, Path: c.path, RawQuery: c.query, Header: h},
				func(int64) int64 { return c.pick })
			got := res.Service
			if res.Redirect != nil {
				got = "redirect " + itoa(res.Redirect.Code) + " " + res.Redirect.Location
			}
			if got != c.want {
				t.Fatalf("got %q, want %q", got, c.want)
			}
			if c.wantPath != "" && res.Path != c.wantPath {
				t.Errorf("path = %q, want %q", res.Path, c.wantPath)
			}
			if c.wantHost != "" && res.Host != c.wantHost {
				t.Errorf("host = %q, want %q", res.Host, c.wantHost)
			}
			if c.wantHeaders != nil {
				var names []string
				for _, ha := range res.HeaderActions {
					for _, o := range ha.RequestHeadersToAdd {
						names = append(names, o.HeaderName)
					}
					for _, o := range ha.ResponseHeadersToAdd {
						names = append(names, o.HeaderName)
					}
				}
				if strings.Join(names, ",") != strings.Join(c.wantHeaders, ",") {
					t.Errorf("header actions %v, want %v", names, c.wantHeaders)
				}
			}
		})
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func TestValidate(t *testing.T) {
	cases := []struct {
		name string
		mod  func(m *compute.UrlMap)
		want string // substring of an error, "" for valid
	}{
		{name: "valid", mod: func(m *compute.UrlMap) {}},
		{name: "no default", mod: func(m *compute.UrlMap) { m.DefaultService = "" }, want: "default service"},
		{name: "service and redirect", mod: func(m *compute.UrlMap) {
			m.DefaultUrlRedirect = &compute.HttpRedirectAction{HttpsRedirect: true}
		}, want: "only one of defaultService and defaultUrlRedirect"},
		{name: "unknown matcher", mod: func(m *compute.UrlMap) { m.HostRules[0].PathMatcher = "nope" }, want: "does not exist"},
		{name: "duplicate host", mod: func(m *compute.UrlMap) { m.HostRules[1].Hosts = []string{"example.com"} }, want: "Duplicate host"},
		{name: "bad path", mod: func(m *compute.UrlMap) { m.PathMatchers[0].PathRules[0].Paths = []string{"/a*"} }, want: "Paths must start with /"},
		{name: "duplicate path", mod: func(m *compute.UrlMap) { m.PathMatchers[0].PathRules[1].Paths = []string{"/static/*"} }, want: "Duplicate path"},
		{name: "duplicate priority", mod: func(m *compute.UrlMap) { m.PathMatchers[3].RouteRules[1].Priority = 20 }, want: "must be unique"},
		{name: "bad regex", mod: func(m *compute.UrlMap) { m.PathMatchers[3].RouteRules[3].MatchRules[0].RegexMatch = "(" }, want: "regexMatch"},
		{name: "two path matches", mod: func(m *compute.UrlMap) { m.PathMatchers[3].RouteRules[0].MatchRules[0].FullPathMatch = "/x" }, want: "only one of prefixMatch"},
		{name: "rules and paths", mod: func(m *compute.UrlMap) {
			m.PathMatchers[3].PathRules = []*compute.PathRule{{Paths: []string{"/x"}, Service: "x"}}
		}, want: "cannot both be specified"},
		{name: "template rewrite unknown var", mod: func(m *compute.UrlMap) {
			m.PathMatchers[3].RouteRules[5].RouteAction.UrlRewrite.PathTemplateRewrite = "/{nope}"
		}, want: "not captured"},
		{name: "prefix rewrite with regex", mod: func(m *compute.UrlMap) {
			m.PathMatchers[3].RouteRules[3].RouteAction = &compute.HttpRouteAction{UrlRewrite: &compute.UrlRewrite{PathPrefixRewrite: "/x"}}
		}, want: "requires prefixMatch"},
		{name: "weight range", mod: func(m *compute.UrlMap) {
			m.PathMatchers[3].RouteRules[7].RouteAction.WeightedBackendServices[0].Weight = 1001
		}, want: "between 0 and 1000"},
		{name: "query match type", mod: func(m *compute.UrlMap) {
			m.PathMatchers[3].RouteRules[4].MatchRules[0].QueryParameterMatches[0].PresentMatch = true
		}, want: "exactly one of presentMatch"},
		{name: "redirect with service", mod: func(m *compute.UrlMap) {
			m.PathMatchers[3].RouteRules[8].Service = "x"
		}, want: "urlRedirect cannot be combined"},
		{name: "bad template", mod: func(m *compute.UrlMap) {
			m.PathMatchers[3].RouteRules[5].MatchRules[0].PathTemplateMatch = "/a/**/b"
		}, want: "last operator"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := refMap()
			c.mod(m)
			errs := Validate(m)
			if c.want == "" {
				if len(errs) > 0 {
					t.Fatalf("unexpected errors: %v", errs)
				}
				return
			}
			if !strings.Contains(strings.Join(errs, "\n"), c.want) {
				t.Fatalf("errors %v do not mention %q", errs, c.want)
			}
		})
	}
}

func TestRunTests(t *testing.T) {
	m := refMap()
	m.Tests = []*compute.UrlMapTest{
		{Host: "example.com", Path: "/static/a.js", Service: "static"},
		{Host: "example.com", Path: "/v1/users", ExpectedOutputUrl: "http://backend.internal/users"},
		{Host: "api.example.com", Path: "/old/x", ExpectedOutputUrl: "http://api.example.com/new/x", ExpectedRedirectResponseCode: 302},
		{Host: "api.example.com", Path: "/api/x", Headers: []*compute.UrlMapTestHeader{{Name: "x-canary", Value: "1"}}, Service: "canary"},
	}
	if f := RunTests(m, nil); len(f) != 0 {
		t.Fatalf("unexpected failures: %+v", f[0])
	}
	m.Tests = append(m.Tests, &compute.UrlMapTest{Host: "example.com", Path: "/exact", Service: "static"})
	f := RunTests(m, nil)
	if len(f) != 1 || f[0].ActualService != "exact" || f[0].ExpectedService != "static" {
		t.Fatalf("failures = %+v", f)
	}
}

func TestTemplates(t *testing.T) {
	cases := []struct {
		tmpl, path string
		ok         bool
		vars       map[string]string
	}{
		{"/**", "/", true, nil},
		{"/**", "/a/b", true, nil},
		{"/*/x", "/a/x", true, nil},
		{"/*/x", "/x", false, nil},
		{"/a/{b}/{c=**}", "/a/1/2/3", true, map[string]string{"b": "1", "c": "2/3"}},
		{"/a/{b=x/*}", "/a/x/9", true, map[string]string{"b": "x/9"}},
		{"/a/{b=x/*}", "/a/y/9", false, nil},
	}
	for _, c := range cases {
		tp, err := parseTemplate(c.tmpl)
		if err != nil {
			t.Fatalf("%s: %v", c.tmpl, err)
		}
		vars, ok := tp.match(c.path)
		if ok != c.ok {
			t.Fatalf("%s ~ %s = %v, want %v", c.tmpl, c.path, ok, c.ok)
		}
		for k, v := range c.vars {
			if vars[k] != v {
				t.Errorf("%s ~ %s: %s = %q, want %q", c.tmpl, c.path, k, vars[k], v)
			}
		}
	}
}

func BenchmarkRoute(b *testing.B) {
	rt, err := Compile(refMap())
	if err != nil {
		b.Fatal(err)
	}
	req := &Request{Scheme: "https", Method: "GET", Host: "api.example.com", Path: "/videos/mp4/a/b", Header: http.Header{}}
	b.ReportAllocs()
	for b.Loop() {
		rt.Route(req, func(int64) int64 { return 0 })
	}
}
