package lb

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"

	computev1 "google.golang.org/api/compute/v1"

	"github.com/linuxuser586/gcpemu/services/lb/urlmap"
)

// The Web console's Load Balancer view reads two things GCP has no API
// for (SRS 4.8.3), on the admin API:
//
//	GET  /_emu/v1/lb/listeners[?project=P]  each forwarding rule's local listener (FR-LB-002)
//	POST /_emu/v1/lb/route                  which route and backend of a URL map serve a request

// listenerInfo is one forwarding rule's mapped listener.
type listenerInfo struct {
	ForwardingRule string `json:"forwardingRule"`
	IPAddress      string `json:"ipAddress"`
	Port           int    `json:"port"`
	Listener       string `json:"listener"`
	Mode           string `json:"mode"`
	HTTPS          bool   `json:"https"`
}

// serveListeners is GET /_emu/v1/lb/listeners.
func (s *Service) serveListeners(w http.ResponseWriter, r *http.Request) {
	project := r.URL.Query().Get("project")
	cfg := s.dp.cfg.Load()
	out := []listenerInfo{}
	s.dp.mu.Lock()
	for key, l := range s.dp.listeners {
		fe := cfg.fronts[key]
		if fe == nil || project != "" && fe.project != project {
			continue
		}
		out = append(out, listenerInfo{ForwardingRule: fe.frPath, IPAddress: fe.ip, Port: fe.port,
			Listener: l.addr, Mode: l.mode, HTTPS: l.https})
	}
	s.dp.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].ForwardingRule < out[j].ForwardingRule })
	writeJSON(w, http.StatusOK, map[string]any{"listeners": out})
}

// routeRequest is the body of POST /_emu/v1/lb/route.
type routeRequest struct {
	// URLMap is the URL map's resource path or selfLink.
	URLMap string `json:"urlMap"`
	// Scheme is "http" (default) or "https"; Method defaults to GET.
	Scheme  string            `json:"scheme"`
	Method  string            `json:"method"`
	Host    string            `json:"host"`
	Path    string            `json:"path"`
	Headers map[string]string `json:"headers"`
}

// routeResult is the routing decision, as urlmap.Route makes it for the
// data plane.
type routeResult struct {
	// Match is "routeRule", "pathRule", "pathMatcherDefault" or
	// "urlMapDefault".
	Match       string                              `json:"match"`
	PathMatcher string                              `json:"pathMatcher,omitempty"`
	RouteRule   *computev1.HttpRouteRule            `json:"routeRule,omitempty"`
	PathRule    *computev1.PathRule                 `json:"pathRule,omitempty"`
	Service     string                              `json:"service,omitempty"`
	Weighted    []*computev1.WeightedBackendService `json:"weightedBackendServices,omitempty"`
	Redirect    *redirect                           `json:"redirect,omitempty"`
	Host        string                              `json:"host"`
	Path        string                              `json:"path"`
	// HeaderActions apply in order, most specific first.
	HeaderActions []*computev1.HttpHeaderAction `json:"headerActions,omitempty"`
}

// redirect is the redirect a request would get instead of a backend.
type redirect struct {
	Code     int    `json:"code"`
	Location string `json:"location"`
}

func adminError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// serveRoute is POST /_emu/v1/lb/route: the stored URL map's routing of
// one request, without sending it.
func (s *Service) serveRoute(w http.ResponseWriter, r *http.Request) {
	var req routeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		adminError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	path := relPath(req.URLMap)
	if !strings.HasPrefix(path, "projects/") || !strings.Contains(path, "/urlMaps/") {
		adminError(w, http.StatusBadRequest, "urlMap must be a URL map's resource path, e.g. projects/P/global/urlMaps/M")
		return
	}
	obj, ok := s.load(kindURLMap, path)
	if !ok {
		adminError(w, http.StatusNotFound, "URL map "+path+" not found")
		return
	}
	rt, err := urlmap.Compile(obj.(*computev1.UrlMap))
	if err != nil {
		adminError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Host == "" {
		adminError(w, http.StatusBadRequest, "host is required")
		return
	}
	p, q, _ := strings.Cut(req.Path, "?")
	if p == "" {
		p = "/"
	}
	if !strings.HasPrefix(p, "/") {
		adminError(w, http.StatusBadRequest, "path must start with /")
		return
	}
	in := &urlmap.Request{Scheme: strings.ToLower(req.Scheme), Method: strings.ToUpper(req.Method),
		Host: req.Host, Path: p, RawQuery: q, Header: http.Header{}}
	if in.Scheme == "" {
		in.Scheme = "http"
	}
	if in.Method == "" {
		in.Method = http.MethodGet
	}
	for k, v := range req.Headers {
		in.Header.Add(k, v)
	}
	// The first weighted backend service; all of them are reported.
	res := rt.Route(in, func(int64) int64 { return 0 })
	out := routeResult{PathMatcher: res.PathMatcher, RouteRule: res.RouteRule, PathRule: res.PathRule,
		Service: res.Service, Host: res.Host, Path: res.Path, HeaderActions: res.HeaderActions}
	switch {
	case res.RouteRule != nil:
		out.Match = "routeRule"
	case res.PathRule != nil:
		out.Match = "pathRule"
	case res.PathMatcher != "":
		out.Match = "pathMatcherDefault"
	default:
		out.Match = "urlMapDefault"
	}
	if res.Action != nil && len(res.Action.WeightedBackendServices) > 1 {
		out.Weighted = res.Action.WeightedBackendServices
	}
	if res.Redirect != nil {
		out.Redirect, out.Service = &redirect{Code: res.Redirect.Code, Location: res.Redirect.Location}, ""
	}
	writeJSON(w, http.StatusOK, out)
}
