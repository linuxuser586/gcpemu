package lb_test

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"path"
	"strconv"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/storage"
	computev1 "google.golang.org/api/compute/v1"
	"google.golang.org/api/option"
)

// Custom error responses (FR-LB-012): a URL map's customErrorResponsePolicy
// replaces a backend's error with a page from the policy's errorService.

// object is one Cloud Storage object of the SPA bucket.
type object struct {
	body, contentType, cacheControl string
}

// spaBucket creates bucket "spa" holding objects and backend bucket "spa"
// serving it.
func (e *env) spaBucket(cdn *computev1.BackendBucketCdnPolicy, objects map[string]object) {
	t, ctx := e.t, context.Background()
	sc, err := storage.NewClient(ctx, option.WithEndpoint("http://"+e.inst.Endpoint("gcs")+"/storage/v1/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	bkt := sc.Bucket("spa")
	if err := bkt.Create(ctx, proj, nil); err != nil {
		t.Fatal(err)
	}
	for name, o := range objects {
		e.putObject(sc, name, o)
	}
	e.do(e.c.BackendBuckets.Insert(proj, &computev1.BackendBucket{Name: "spa", BucketName: "spa", EnableCdn: cdn != nil, CdnPolicy: cdn,
		CustomResponseHeaders: []string{"X-Bucket:spa"}}).Do())
}

func (e *env) putObject(sc *storage.Client, name string, o object) {
	w := sc.Bucket("spa").Object(name).NewWriter(context.Background())
	w.ContentType, w.CacheControl = o.contentType, o.cacheControl
	_, _ = io.WriteString(w, o.body)
	if err := w.Close(); err != nil {
		e.t.Fatal(err)
	}
}

// errLB puts m behind an HTTP load balancer with scheme (global unless
// region is set) and returns its base URL.
func (e *env) errLB(m *computev1.UrlMap, scheme, region string) string {
	c := e.c
	m.Name = "map"
	if region == "" {
		e.do(c.UrlMaps.Insert(proj, m).Do())
		e.do(c.TargetHttpProxies.Insert(proj, &computev1.TargetHttpProxy{Name: "p", UrlMap: "global/urlMaps/map"}).Do())
		e.do(c.GlobalForwardingRules.Insert(proj, &computev1.ForwardingRule{Name: "fr", LoadBalancingScheme: scheme, PortRange: "8080", Target: "global/targetHttpProxies/p"}).Do())
		return "http://" + e.inst.Endpoint(ep("fr"))
	}
	e.do(c.RegionUrlMaps.Insert(proj, region, m).Do())
	e.do(c.RegionTargetHttpProxies.Insert(proj, region, &computev1.TargetHttpProxy{Name: "p", UrlMap: "regions/" + region + "/urlMaps/map"}).Do())
	e.do(c.ForwardingRules.Insert(proj, region, &computev1.ForwardingRule{Name: "fr", LoadBalancingScheme: scheme, PortRange: "8080",
		Network: "global/networks/vpc", Target: "regions/" + region + "/targetHttpProxies/p"}).Do())
	return "http://" + e.inst.Endpoint("lb:"+proj+"/"+region+"/fr")
}

func errRule(page string, override int64, codes ...string) *computev1.CustomErrorResponsePolicyCustomErrorResponseRule {
	return &computev1.CustomErrorResponsePolicyCustomErrorResponseRule{MatchResponseCodes: codes, Path: page, OverrideResponseCode: override}
}

func errPolicy(svc string, rules ...*computev1.CustomErrorResponsePolicyCustomErrorResponseRule) *computev1.CustomErrorResponsePolicy {
	return &computev1.CustomErrorResponsePolicy{ErrorService: svc, ErrorResponseRules: rules}
}

var spaObjects = map[string]object{
	"index.html":      {body: "<h1>app</h1>", contentType: "text/html"},
	"assets/app.js":   {body: "app()", contentType: "text/javascript"},
	"errors/4xx.html": {body: "client error", contentType: "text/html"},
}

// fetch sends a GET (or the method given) with headers and returns the
// status, body and response header.
func fetch(t *testing.T, method, url string, hdr map[string]string) (int, string, http.Header) {
	t.Helper()
	req, _ := http.NewRequest(method, url, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	cl := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := cl.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp.StatusCode, string(b), resp.Header
}

// TestCustomErrorResponseSPA serves a single-page app's deep links: the
// bucket's 404 becomes /index.html with 200, at every level a policy can
// be set, while other paths keep the bucket's 404.
func TestCustomErrorResponseSPA(t *testing.T) {
	spa := errPolicy("global/backendBuckets/spa", errRule("/index.html", 200, "404"))
	for name, m := range map[string]*computev1.UrlMap{
		"route rule": {DefaultService: "global/backendBuckets/spa",
			HostRules: []*computev1.HostRule{{Hosts: []string{"*"}, PathMatcher: "pm"}},
			PathMatchers: []*computev1.PathMatcher{{Name: "pm", DefaultService: "global/backendBuckets/spa",
				RouteRules: []*computev1.HttpRouteRule{
					{Priority: 1, MatchRules: []*computev1.HttpRouteRuleMatch{{PrefixMatch: "/assets/"}}, Service: "global/backendBuckets/spa"},
					{Priority: 2, MatchRules: []*computev1.HttpRouteRuleMatch{{PrefixMatch: "/"}}, Service: "global/backendBuckets/spa",
						CustomErrorResponsePolicy: spa},
				}}}},
		"path rule": {DefaultService: "global/backendBuckets/spa",
			HostRules: []*computev1.HostRule{{Hosts: []string{"*"}, PathMatcher: "pm"}},
			PathMatchers: []*computev1.PathMatcher{{Name: "pm", DefaultService: "global/backendBuckets/spa",
				PathRules: []*computev1.PathRule{
					{Paths: []string{"/assets/*"}, Service: "global/backendBuckets/spa"},
					{Paths: []string{"/settings", "/transactions/*"}, Service: "global/backendBuckets/spa", CustomErrorResponsePolicy: spa},
				}}}},
		"path matcher": {DefaultService: "global/backendBuckets/spa",
			HostRules: []*computev1.HostRule{{Hosts: []string{"*"}, PathMatcher: "pm"}},
			PathMatchers: []*computev1.PathMatcher{{Name: "pm", DefaultService: "global/backendBuckets/spa", DefaultCustomErrorResponsePolicy: spa,
				PathRules: []*computev1.PathRule{{Paths: []string{"/assets/*"}, Service: "global/backendBuckets/spa"}}}}},
	} {
		t.Run(name, func(t *testing.T) {
			e := start(t)
			e.spaBucket(nil, spaObjects)
			base := e.errLB(m, "EXTERNAL_MANAGED", "")
			for _, p := range []string{"/settings", "/transactions/42"} {
				if code, body, h := fetch(t, "GET", base+p, nil); code != 200 || body != "<h1>app</h1>" || h.Get("Content-Type") != "text/html" {
					t.Errorf("GET %s: %d %q %q", p, code, body, h.Get("Content-Type"))
				}
			}
			if code, body, _ := fetch(t, "GET", base+"/assets/app.js", nil); code != 200 || body != "app()" {
				t.Errorf("GET /assets/app.js: %d %q", code, body)
			}
			if name == "path matcher" {
				return // its default policy covers /assets/ too
			}
			if code, body, _ := fetch(t, "GET", base+"/assets/missing.js", nil); code != 404 || !strings.Contains(body, "NoSuchKey") {
				t.Errorf("GET /assets/missing.js: %d %q, want the bucket's 404", code, body)
			}
		})
	}
	t.Run("url map", func(t *testing.T) {
		e := start(t)
		e.spaBucket(nil, spaObjects)
		base := e.errLB(&computev1.UrlMap{DefaultService: "global/backendBuckets/spa", DefaultCustomErrorResponsePolicy: spa}, "EXTERNAL_MANAGED", "")
		if code, body, _ := fetch(t, "GET", base+"/settings", nil); code != 200 || body != "<h1>app</h1>" {
			t.Errorf("GET /settings: %d %q", code, body)
		}
	})
}

// statusOrigin answers .../N with status N.
func statusOrigin(w http.ResponseWriter, r *http.Request) {
	code, err := strconv.Atoi(path.Base(r.URL.Path))
	if err != nil {
		code = 200
	}
	w.Header().Set("Content-Type", "text/plain")
	w.Header().Set("X-Origin", "app")
	w.WriteHeader(code)
	_, _ = io.WriteString(w, "app "+strconv.Itoa(code))
}

// backendService creates backend service name behind a NEG of its own
// whose only endpoint is port.
func (e *env) backendService(name string, port int64, scheme string, custom ...string) {
	c := e.c
	e.do(c.NetworkEndpointGroups.Insert(proj, "us-central1-a", &computev1.NetworkEndpointGroup{Name: name, Network: "global/networks/vpc"}).Do())
	e.do(c.NetworkEndpointGroups.AttachNetworkEndpoints(proj, "us-central1-a", name, &computev1.NetworkEndpointGroupsAttachEndpointsRequest{
		NetworkEndpoints: []*computev1.NetworkEndpoint{{IpAddress: "127.0.0.1", Port: port}}}).Do())
	e.do(c.BackendServices.Insert(proj, &computev1.BackendService{Name: name, LoadBalancingScheme: scheme, Protocol: "HTTP",
		LogConfig: &computev1.BackendServiceLogConfig{Enable: true}, CustomResponseHeaders: custom,
		Backends: []*computev1.Backend{{Group: "zones/us-central1-a/networkEndpointGroups/" + name}}}).Do())
}

// TestCustomErrorResponseRules checks which responses a policy replaces
// and how the error page is fetched.
func TestCustomErrorResponseRules(t *testing.T) {
	e := start(t)
	e.do(e.c.Networks.Insert(proj, &computev1.Network{Name: "vpc", AutoCreateSubnetworks: false, ForceSendFields: []string{"AutoCreateSubnetworks"}}).Do())
	pages := make(chan *http.Request, 100)
	e.backendService("app", named(t, "app", statusOrigin), "EXTERNAL_MANAGED", "X-App:1")
	e.backendService("errors", named(t, "errors", func(w http.ResponseWriter, r *http.Request) {
		pages <- r
		if r.URL.Path == "/missing.html" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		w.Header().Set("X-Error-Origin", "1")
		w.Header().Set("Vary", "Accept-Encoding")
		_, _ = io.WriteString(w, "page "+r.URL.Path)
	}), "EXTERNAL_MANAGED", "X-Errors:1")
	app := "global/backendServices/app"
	route := func(prio int64, prefix string, p *computev1.CustomErrorResponsePolicy, fault *computev1.HttpFaultInjection) *computev1.HttpRouteRule {
		rr := &computev1.HttpRouteRule{Priority: prio, MatchRules: []*computev1.HttpRouteRuleMatch{{PrefixMatch: prefix}}, Service: app, CustomErrorResponsePolicy: p,
			HeaderAction: &computev1.HttpHeaderAction{ResponseHeadersToAdd: []*computev1.HttpHeaderOption{{HeaderName: "X-Route", HeaderValue: prefix}},
				RequestHeadersToAdd: []*computev1.HttpHeaderOption{{HeaderName: "X-Route-Request", HeaderValue: prefix}}}}
		if fault != nil {
			rr.RouteAction = &computev1.HttpRouteAction{FaultInjectionPolicy: fault}
		}
		return rr
	}
	errs := "global/backendServices/errors"
	// "down" has an endpoint nothing listens on.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln.Close()
	e.backendService("down", int64(ln.Addr().(*net.TCPAddr).Port), "EXTERNAL_MANAGED")
	base := e.errLB(&computev1.UrlMap{DefaultService: app,
		DefaultCustomErrorResponsePolicy: errPolicy(errs, errRule("/5xx.html", 0, "5xx"), errRule("/301.html", 200, "301")),
		HostRules:                        []*computev1.HostRule{{Hosts: []string{"*"}, PathMatcher: "pm"}},
		PathMatchers: []*computev1.PathMatcher{{Name: "pm", DefaultService: app, RouteRules: []*computev1.HttpRouteRule{
			route(1, "/status/", errPolicy(errs, errRule("/404.html", 200, "404"), errRule("/4xx.html", 299, "4xx")), nil),
			route(2, "/fail/", errPolicy(errs, errRule("/missing.html", 200, "404")), nil),
			route(3, "/abort/", nil, &computev1.HttpFaultInjection{Abort: &computev1.HttpFaultAbort{HttpStatus: 503, Percentage: 100}}),
			route(4, "/down/", errPolicy("global/backendServices/down", errRule("/404.html", 200, "404")), nil),
			{Priority: 5, MatchRules: []*computev1.HttpRouteRuleMatch{{PrefixMatch: "/old"}}, UrlRedirect: &computev1.HttpRedirectAction{PathRedirect: "/new"}},
		}}}}, "EXTERNAL_MANAGED", "")

	for _, tc := range []struct {
		path string
		hdr  map[string]string
		code int
		body string
	}{
		{"/status/404", nil, 200, "page /404.html"}, // the route's exact rule
		{"/status/401", nil, 299, "page /4xx.html"}, // the route's class rule
		{"/status/503", nil, 503, "page /5xx.html"}, // the URL map's rule, no override
		{"/status/302", nil, 302, "app 302"},        // listed nowhere
		{"/status/200", nil, 200, "app 200"},        // not an error
		{"/status/404", map[string]string{"Authorization": "Bearer x"}, 404, "app 404"},
		{"/fail/404", nil, 404, "404 Not Found\n"},     // the page is missing
		{"/abort/x", nil, 503, "fault filter abort\n"}, // the data plane's own answer
		{"/down/404", nil, 404, "404 Not Found\n"},     // the error service is unreachable
	} {
		code, body, h := fetch(t, "GET", base+tc.path+"?q=1", tc.hdr)
		if code != tc.code || body != tc.body {
			t.Errorf("GET %s %v: %d %q, want %d %q", tc.path, tc.hdr, code, body, tc.code, tc.body)
		}
		if strings.HasPrefix(body, "page ") {
			if h.Get("X-Errors") != "" || h.Get("X-Error-Origin") != "" || h.Get("X-Origin") != "" {
				t.Errorf("GET %s: error page carries the error service's or backend's headers: %v", tc.path, h)
			}
			if h.Get("Vary") != "Accept-Encoding" {
				t.Errorf("GET %s: error page lost its Vary: %v", tc.path, h)
			}
			if h.Get("X-App") != "1" || (strings.HasPrefix(tc.path, "/status/") && h.Get("X-Route") != "/status/") {
				t.Errorf("GET %s: matched backend's and route's headers missing: %v", tc.path, h)
			}
		}
		if tc.path == "/fail/404" && h.Get("Cache-Control") != "no-store" {
			t.Errorf("fallback Cache-Control = %q", h.Get("Cache-Control"))
		}
	}

	// The fetch has no query string, Range or conditional headers; client
	// headers otherwise pass on.
	for len(pages) > 0 {
		<-pages
	}
	code, body, _ := fetch(t, "GET", base+"/status/404?q=1", map[string]string{"Range": "bytes=0-1", "If-None-Match": `"x"`, "X-Client": "c"})
	if code != 200 || body != "page /404.html" {
		t.Fatalf("Range request: %d %q, want the whole page", code, body)
	}
	pr := <-pages
	if pr.URL.RawQuery != "" || pr.Method != "GET" || pr.Header.Get("Range") != "" || pr.Header.Get("If-None-Match") != "" || pr.Header.Get("X-Client") != "c" ||
		pr.Header.Get("X-Route-Request") != "" {
		t.Errorf("error page fetch: %s %s %v", pr.Method, pr.URL, pr.Header)
	}
	if code, body, h := fetch(t, "HEAD", base+"/status/404", nil); code != 200 || body != "" || h.Get("Content-Type") != "text/html" {
		t.Errorf("HEAD: %d %q %v", code, body, h)
	}
	if pr := <-pages; pr.Method != "HEAD" {
		t.Errorf("HEAD fetched the page with %s", pr.Method)
	}
	// A redirect is the load balancer's own answer, even with a rule for
	// its status.
	if code, _, h := fetch(t, "GET", base+"/old", nil); code != 301 || h.Get("Location") == "" {
		t.Errorf("redirect: %d %v", code, h)
	}

	// The access log has the status served and, when one was applied, the
	// override code.
	logs := map[string]logLine{}
	for _, l := range accessLog(t, e, 12) {
		u := fmt.Sprint(l.HTTPRequest["requestUrl"])
		if _, seen := logs[u]; !seen {
			logs[u] = l
		}
	}
	for path, want := range map[string]struct {
		status   float64
		override any
		details  string
	}{
		"/status/404?q=1": {200, float64(200), "response_sent_by_backend"},
		"/status/401?q=1": {299, float64(299), "response_sent_by_backend"},
		"/status/503?q=1": {503, nil, "response_sent_by_backend"},
		"/fail/404?q=1":   {404, nil, "custom_error_response_fetch_failed"},
		"/down/404?q=1":   {404, nil, "custom_error_response_fetch_failed"},
	} {
		l := logs[base+path]
		if l.HTTPRequest["status"] != want.status || l.JSONPayload["overrideResponseCodeServed"] != want.override || l.JSONPayload["statusDetails"] != want.details {
			t.Errorf("log for %s: status %v override %v details %v, want %+v", path, l.HTTPRequest["status"], l.JSONPayload["overrideResponseCodeServed"], l.JSONPayload["statusDetails"], want)
		}
	}
}

// TestCustomErrorResponseScope checks that only a global EXTERNAL_MANAGED
// load balancer serves the policy, and only for backend responses.
func TestCustomErrorResponseScope(t *testing.T) {
	spa := errPolicy("global/backendBuckets/spa", errRule("/index.html", 200, "404"), errRule("/index.html", 200, "5xx"))
	t.Run("classic", func(t *testing.T) {
		e := start(t)
		e.spaBucket(nil, spaObjects)
		base := e.errLB(&computev1.UrlMap{DefaultService: "global/backendBuckets/spa", DefaultCustomErrorResponsePolicy: spa}, "EXTERNAL", "")
		if code, body, _ := fetch(t, "GET", base+"/settings", nil); code != 404 || !strings.Contains(body, "NoSuchKey") {
			t.Errorf("GET /settings: %d %q, want the bucket's 404", code, body)
		}
	})
	t.Run("regional", func(t *testing.T) {
		e := start(t)
		c := e.c
		e.neg(named(t, "app", statusOrigin))
		for _, n := range []string{"app", "errors"} {
			e.do(c.RegionBackendServices.Insert(proj, "us-central1", &computev1.BackendService{Name: n, LoadBalancingScheme: "EXTERNAL_MANAGED", Protocol: "HTTP",
				Backends: []*computev1.Backend{{Group: "zones/us-central1-a/networkEndpointGroups/neg", BalancingMode: "RATE", MaxRatePerEndpoint: 100}}}).Do())
		}
		base := e.errLB(&computev1.UrlMap{DefaultService: "regions/us-central1/backendServices/app",
			DefaultCustomErrorResponsePolicy: errPolicy("regions/us-central1/backendServices/errors", errRule("/200", 200, "404"))}, "EXTERNAL_MANAGED", "us-central1")
		if code, body, _ := fetch(t, "GET", base+"/404", nil); code != 404 || body != "app 404" {
			t.Errorf("GET /404: %d %q, want the backend's 404", code, body)
		}
	})
	t.Run("no healthy upstream", func(t *testing.T) {
		e := start(t)
		e.spaBucket(nil, spaObjects)
		e.neg()
		e.do(e.c.BackendServices.Insert(proj, &computev1.BackendService{Name: "empty", LoadBalancingScheme: "EXTERNAL_MANAGED", Protocol: "HTTP",
			Backends: []*computev1.Backend{{Group: "zones/us-central1-a/networkEndpointGroups/neg"}}}).Do())
		base := e.errLB(&computev1.UrlMap{DefaultService: "global/backendServices/empty", DefaultCustomErrorResponsePolicy: spa}, "EXTERNAL_MANAGED", "")
		if code, body, _ := fetch(t, "GET", base+"/x", nil); code != 503 || body != "no healthy upstream" {
			t.Errorf("GET /x: %d %q, want the data plane's 503", code, body)
		}
	})
}

// TestCustomErrorResponseCDN checks that Cloud CDN caches the error page
// under the requested URL, with the error object's freshness and the
// override code.
func TestCustomErrorResponseCDN(t *testing.T) {
	spa := errPolicy("global/backendBuckets/spa", errRule("/index.html", 200, "404"))
	setup := func(t *testing.T, cdn *computev1.BackendBucketCdnPolicy, p *computev1.CustomErrorResponsePolicy, objects map[string]object) (*env, string) {
		e := start(t)
		e.spaBucket(cdn, objects)
		return e, e.errLB(&computev1.UrlMap{DefaultService: "global/backendBuckets/spa", DefaultCustomErrorResponsePolicy: p,
			HeaderAction: &computev1.HttpHeaderAction{ResponseHeadersToAdd: []*computev1.HttpHeaderOption{
				{HeaderName: "X-Cache-Status", HeaderValue: "{cdn_cache_status}", Replace: true}}}}, "EXTERNAL_MANAGED", "")
	}
	// expect fetches path once per wanted cache status, each served as
	// code with body.
	expect := func(t *testing.T, base, path string, code int, body string, statuses ...string) {
		t.Helper()
		for i, st := range statuses {
			c, b, h := fetch(t, "GET", base+path, nil)
			if c != code || b != body || h.Get("X-Cache-Status") != st {
				t.Errorf("GET %s #%d: %d %q cache %q, want %d %q cache %q", path, i+1, c, b, h.Get("X-Cache-Status"), code, body, st)
			}
		}
	}
	static := func(cc string) map[string]object {
		return map[string]object{"index.html": {body: "<h1>app</h1>", contentType: "text/html", cacheControl: cc}}
	}
	t.Run("force cache all", func(t *testing.T) {
		_, base := setup(t, &computev1.BackendBucketCdnPolicy{CacheMode: "FORCE_CACHE_ALL", DefaultTtl: 60}, spa, static(""))
		expect(t, base, "/settings", 200, "<h1>app</h1>", "miss", "hit")
	})
	t.Run("cache all static with max-age", func(t *testing.T) {
		_, base := setup(t, &computev1.BackendBucketCdnPolicy{CacheMode: "CACHE_ALL_STATIC"}, spa, static("public, max-age=30"))
		expect(t, base, "/settings", 200, "<h1>app</h1>", "miss", "hit")
	})
	t.Run("cache all static html", func(t *testing.T) {
		_, base := setup(t, &computev1.BackendBucketCdnPolicy{CacheMode: "CACHE_ALL_STATIC"}, spa, static(""))
		expect(t, base, "/settings", 200, "<h1>app</h1>", "uncacheable", "uncacheable")
	})
	t.Run("negative caching", func(t *testing.T) {
		_, base := setup(t, &computev1.BackendBucketCdnPolicy{CacheMode: "CACHE_ALL_STATIC", NegativeCaching: true},
			errPolicy("global/backendBuckets/spa", errRule("/index.html", 0, "404")), static(""))
		expect(t, base, "/settings", 404, "<h1>app</h1>", "miss", "hit")
	})
	t.Run("invalidation", func(t *testing.T) {
		e, base := setup(t, &computev1.BackendBucketCdnPolicy{CacheMode: "FORCE_CACHE_ALL", DefaultTtl: 60}, spa, static(""))
		expect(t, base, "/settings", 200, "<h1>app</h1>", "miss")
		e.do(e.c.UrlMaps.InvalidateCache(proj, "map", &computev1.CacheInvalidationRule{Path: "/index.html"}).Do())
		expect(t, base, "/settings", 200, "<h1>app</h1>", "hit")
		e.do(e.c.UrlMaps.InvalidateCache(proj, "map", &computev1.CacheInvalidationRule{Path: "/settings"}).Do())
		expect(t, base, "/settings", 200, "<h1>app</h1>", "miss")
	})
	t.Run("failed fetch", func(t *testing.T) {
		e, base := setup(t, &computev1.BackendBucketCdnPolicy{CacheMode: "FORCE_CACHE_ALL", DefaultTtl: 60, NegativeCaching: true}, spa, nil)
		expect(t, base, "/settings", 404, "404 Not Found\n", "uncacheable")
		sc, err := storage.NewClient(context.Background(), option.WithEndpoint("http://"+e.inst.Endpoint("gcs")+"/storage/v1/"), option.WithoutAuthentication())
		if err != nil {
			t.Fatal(err)
		}
		e.putObject(sc, "index.html", object{body: "<h1>app</h1>", contentType: "text/html"})
		expect(t, base, "/settings", 200, "<h1>app</h1>", "miss", "hit")
	})
	t.Run("stale refresh", func(t *testing.T) {
		e, base := setup(t, &computev1.BackendBucketCdnPolicy{CacheMode: "FORCE_CACHE_ALL", DefaultTtl: 60}, spa, static(""))
		expect(t, base, "/settings", 200, "<h1>app</h1>", "miss")
		sc, err := storage.NewClient(context.Background(), option.WithEndpoint("http://"+e.inst.Endpoint("gcs")+"/storage/v1/"), option.WithoutAuthentication())
		if err != nil {
			t.Fatal(err)
		}
		e.putObject(sc, "index.html", object{body: "<h1>app v2</h1>", contentType: "text/html"})
		e.inst.Internal().Env.Clock.(interface{ Advance(time.Duration) }).Advance(2 * time.Minute)
		expect(t, base, "/settings", 200, "<h1>app v2</h1>", "miss", "hit")
	})
	t.Run("error service cache bypassed", func(t *testing.T) {
		e, base := setup(t, &computev1.BackendBucketCdnPolicy{CacheMode: "FORCE_CACHE_ALL", DefaultTtl: 60}, spa, static(""))
		expect(t, base, "/index.html", 200, "<h1>app</h1>", "miss", "hit")
		sc, err := storage.NewClient(context.Background(), option.WithEndpoint("http://"+e.inst.Endpoint("gcs")+"/storage/v1/"), option.WithoutAuthentication())
		if err != nil {
			t.Fatal(err)
		}
		e.putObject(sc, "index.html", object{body: "<h1>app v2</h1>", contentType: "text/html"})
		expect(t, base, "/settings", 200, "<h1>app v2</h1>", "miss")
	})
	t.Run("log", func(t *testing.T) {
		e, base := setup(t, &computev1.BackendBucketCdnPolicy{CacheMode: "FORCE_CACHE_ALL", DefaultTtl: 60}, spa, static(""))
		expect(t, base, "/settings", 200, "<h1>app</h1>", "miss", "hit")
		l := accessLog(t, e, 2)
		if l[0].JSONPayload["overrideResponseCodeServed"] != float64(200) || l[1].JSONPayload["overrideResponseCodeServed"] != nil {
			t.Errorf("overrideResponseCodeServed: miss %v, hit %v", l[0].JSONPayload["overrideResponseCodeServed"], l[1].JSONPayload["overrideResponseCodeServed"])
		}
	})
}
