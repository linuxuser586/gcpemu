package lb_test

import (
	"bytes"
	"encoding/json"
	"net/http"
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
