package lb_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	computev1 "google.golang.org/api/compute/v1"
)

// TestConsoleReports checks what the Web console's Load Balancer view
// reads from the admin API: each forwarding rule's local listener, and
// which route and backend of a URL map serve a request.
func TestConsoleReports(t *testing.T) {
	e := start(t)
	fr := e.stack(newBackend(t))

	ls := e.rest(http.MethodGet, "/_emu/v1/lb/listeners?project="+proj, nil)["listeners"].([]any)
	if len(ls) != 1 {
		t.Fatalf("listeners = %v", ls)
	}
	l := ls[0].(map[string]any)
	if l["forwardingRule"] != "projects/"+proj+"/global/forwardingRules/https-fr" || l["ipAddress"] != fr.IPAddress ||
		l["port"] != float64(8080) || l["https"] != true || l["listener"] == "" || l["mode"] == "" {
		t.Fatalf("listener = %v", l)
	}
	if got := e.rest(http.MethodGet, "/_emu/v1/lb/listeners?project=other-proj", nil)["listeners"].([]any); len(got) != 0 {
		t.Errorf("other Project's listeners = %v", got)
	}

	// Route rules with a header match, a weighted split and a rewrite.
	e.do(e.c.UrlMaps.Insert(proj, &computev1.UrlMap{
		Name: "routes", DefaultService: "global/backendBuckets/static",
		HostRules: []*computev1.HostRule{{Hosts: []string{"*.example.test"}, PathMatcher: "pm"}},
		PathMatchers: []*computev1.PathMatcher{{
			Name: "pm", DefaultService: "global/backendBuckets/static",
			RouteRules: []*computev1.HttpRouteRule{
				{Priority: 1, Service: "global/backendServices/api", MatchRules: []*computev1.HttpRouteRuleMatch{
					{PrefixMatch: "/api/", HeaderMatches: []*computev1.HttpHeaderMatch{{HeaderName: "x-canary", ExactMatch: "1"}}}}},
				{Priority: 2, MatchRules: []*computev1.HttpRouteRuleMatch{{PrefixMatch: "/api/"}},
					RouteAction: &computev1.HttpRouteAction{
						UrlRewrite: &computev1.UrlRewrite{PathPrefixRewrite: "/v2/"},
						WeightedBackendServices: []*computev1.WeightedBackendService{
							{BackendService: "global/backendServices/api", Weight: 90},
							{BackendService: "global/backendBuckets/static", Weight: 10},
						}}},
			},
		}},
	}).Do())

	route := func(body map[string]any, code int) map[string]any {
		t.Helper()
		b, _ := json.Marshal(body)
		resp, err := http.Post(e.inst.GatewayURL()+"/_emu/v1/lb/route", "application/json", bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		if resp.StatusCode != code {
			t.Fatalf("route %v: %d %v", body, resp.StatusCode, out)
		}
		return out
	}
	link := func(p string) string { return "https://www.googleapis.com/compute/v1/projects/" + proj + "/" + p }
	m := "projects/" + proj + "/global/urlMaps/map"

	got := route(map[string]any{"urlMap": m, "host": "app.example.test", "path": "/api/users?x=1"}, 200)
	if got["match"] != "pathRule" || got["pathMatcher"] != "app" || got["service"] != link("global/backendServices/api") {
		t.Errorf("path rule = %v", got)
	}
	got = route(map[string]any{"urlMap": m, "host": "app.example.test", "path": "/old"}, 200)
	if r, _ := got["redirect"].(map[string]any); got["service"] != nil || r["code"] != float64(302) || r["location"] != "http://app.example.test/new" {
		t.Errorf("redirect = %v", got)
	}
	got = route(map[string]any{"urlMap": m, "host": "other.test", "path": "/"}, 200)
	if got["match"] != "urlMapDefault" || got["service"] != link("global/backendBuckets/static") {
		t.Errorf("default = %v", got)
	}

	rm := link("global/urlMaps/routes")
	got = route(map[string]any{"urlMap": rm, "host": "a.example.test", "path": "/api/x", "headers": map[string]string{"X-Canary": "1"}}, 200)
	if rr, _ := got["routeRule"].(map[string]any); got["match"] != "routeRule" || rr["priority"] != float64(1) {
		t.Errorf("header match = %v", got)
	}
	got = route(map[string]any{"urlMap": rm, "host": "a.example.test", "path": "/api/x"}, 200)
	if w, _ := got["weightedBackendServices"].([]any); len(w) != 2 || got["path"] != "/v2/x" {
		t.Errorf("weighted = %v", got)
	}
	got = route(map[string]any{"urlMap": rm, "host": "a.example.test", "path": "/web"}, 200)
	if got["match"] != "pathMatcherDefault" {
		t.Errorf("path matcher default = %v", got)
	}

	route(map[string]any{"urlMap": "projects/" + proj + "/global/urlMaps/missing", "host": "a"}, 404)
	route(map[string]any{"urlMap": rm}, 400)
	route(map[string]any{"urlMap": "nope", "host": "a"}, 400)
}

// TestCdnPolicyValidation checks the cdnPolicy limits GCP enforces, which
// the Web console's Cloud CDN view shows before sending.
func TestCdnPolicyValidation(t *testing.T) {
	e := start(t)
	bad := []struct {
		policy *computev1.BackendBucketCdnPolicy
		msg    string
	}{
		{&computev1.BackendBucketCdnPolicy{CacheMode: "CACHE_SOME"}, "'resource.cdnPolicy.cacheMode': 'CACHE_SOME'"},
		{&computev1.BackendBucketCdnPolicy{DefaultTtl: 31622401}, "Must be between 0 and 31622400."},
		{&computev1.BackendBucketCdnPolicy{DefaultTtl: 7200, MaxTtl: 3600}, "Default TTL must be less than or equal to max TTL."},
		{&computev1.BackendBucketCdnPolicy{ServeWhileStale: 604801}, "Must be between 0 and 604800."},
		{&computev1.BackendBucketCdnPolicy{NegativeCachingPolicy: []*computev1.BackendBucketCdnPolicyNegativeCachingPolicy{{Code: 404, Ttl: 60}}},
			"Negative caching policy requires negative caching to be enabled."},
		{&computev1.BackendBucketCdnPolicy{NegativeCaching: true, NegativeCachingPolicy: []*computev1.BackendBucketCdnPolicyNegativeCachingPolicy{{Code: 500, Ttl: 60}}},
			"'resource.cdnPolicy.negativeCachingPolicy[0].code': '500'"},
		{&computev1.BackendBucketCdnPolicy{NegativeCaching: true, NegativeCachingPolicy: []*computev1.BackendBucketCdnPolicyNegativeCachingPolicy{{Code: 404, Ttl: 1801}}},
			"'resource.cdnPolicy.negativeCachingPolicy[0].ttl': '1801'. Must be between 0 and 1800."},
	}
	for _, c := range bad {
		_, err := e.c.BackendBuckets.Insert(proj, &computev1.BackendBucket{Name: "b", BucketName: "x", EnableCdn: true, CdnPolicy: c.policy}).Do()
		if code, _ := apiErr(err); code != http.StatusBadRequest || !strings.Contains(err.Error(), c.msg) {
			t.Errorf("%+v: %v, want %q", c.policy, err, c.msg)
		}
	}
	_, err := e.c.BackendServices.Insert(proj, &computev1.BackendService{Name: "s", EnableCDN: true,
		CdnPolicy: &computev1.BackendServiceCdnPolicy{CacheKeyPolicy: &computev1.CacheKeyPolicy{IncludeQueryString: true,
			QueryStringWhitelist: []string{"a"}, QueryStringBlacklist: []string{"b"}}}}).Do()
	if code, _ := apiErr(err); code != http.StatusBadRequest || !strings.Contains(err.Error(), "Only one of queryStringWhitelist and queryStringBlacklist") {
		t.Errorf("both query lists: %v", err)
	}
	e.do(e.c.BackendBuckets.Insert(proj, &computev1.BackendBucket{Name: "ok", BucketName: "x", EnableCdn: true,
		CdnPolicy: &computev1.BackendBucketCdnPolicy{CacheMode: "FORCE_CACHE_ALL", DefaultTtl: 60, NegativeCaching: true,
			NegativeCachingPolicy: []*computev1.BackendBucketCdnPolicyNegativeCachingPolicy{{Code: 404, Ttl: 30}}}}).Do())
}
