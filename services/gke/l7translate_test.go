package gke

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"

	computev1 "google.golang.org/api/compute/v1"

	"github.com/linuxuser586/gcpemu/services/lb/urlmap"
)

// l7Fixture builds translator input from JSON Kubernetes objects.
type l7Fixture struct {
	t  *testing.T
	in l7Input
}

func newL7Fixture(t *testing.T) *l7Fixture {
	return &l7Fixture{t: t, in: l7Input{
		Cluster: l7Cluster{Project: "p", ID: "0123456789abcdef", Zones: []string{"us-central1-a", "us-central1-b"}},
		Classes: map[string]bool{"gke-l7-global-external-managed": true, "gke-l7-gxlb": true},
		Secret:  func(ns, name string) (string, string, bool) { return "", "", false },
	}}
}

func (f *l7Fixture) add(objs ...string) {
	f.t.Helper()
	for _, o := range objs {
		var probe struct{ Kind string }
		if err := json.Unmarshal([]byte(o), &probe); err != nil {
			f.t.Fatalf("fixture %s: %v", o, err)
		}
		var err error
		switch probe.Kind {
		case "Service":
			var v kubeService
			err = json.Unmarshal([]byte(o), &v)
			f.in.Services = append(f.in.Services, v)
		case "Ingress":
			var v kubeIngress
			err = json.Unmarshal([]byte(o), &v)
			f.in.Ingresses = append(f.in.Ingresses, v)
		case "Gateway":
			var v kubeGateway
			err = json.Unmarshal([]byte(o), &v)
			f.in.Gateways = append(f.in.Gateways, v)
		case "HTTPRoute":
			var v kubeHTTPRoute
			err = json.Unmarshal([]byte(o), &v)
			f.in.Routes = append(f.in.Routes, v)
		default:
			f.t.Fatalf("fixture kind %q", probe.Kind)
		}
		if err != nil {
			f.t.Fatal(err)
		}
	}
}

func svcJSON(ns, name string, ports ...string) string {
	return `{"kind":"Service","metadata":{"namespace":"` + ns + `","name":"` + name + `"},"spec":{"ports":[` + strings.Join(ports, ",") + `]}}`
}

// plan holds a translation and finds its resources.
type plan struct {
	t *testing.T
	l7Plan
}

func (f *l7Fixture) plan() *plan {
	return &plan{t: f.t, l7Plan: translateL7(f.in)}
}

// of returns the resources of a collection.
func (p *plan) of(coll string) []l7Resource {
	var out []l7Resource
	for _, r := range p.Resources {
		if strings.Split(r.Path, "/")[len(strings.Split(r.Path, "/"))-2] == coll {
			out = append(out, r)
		}
	}
	return out
}

func (p *plan) get(path string) any {
	p.t.Helper()
	for _, r := range p.Resources {
		if r.Path == path || strings.HasSuffix(path, "/"+r.Path) {
			return r.Body
		}
	}
	p.t.Fatalf("no resource %s in %v", path, p.paths())
	return nil
}

func (p *plan) paths() []string {
	var out []string
	for _, r := range p.Resources {
		out = append(out, r.Path)
	}
	return out
}

func (p *plan) lb(kind, ns, name string) *l7LB {
	p.t.Helper()
	for _, l := range p.LBs {
		if l.Kind == kind && l.Namespace == ns && l.Name == name {
			return l
		}
	}
	p.t.Fatalf("no %s %s/%s load balancer", kind, ns, name)
	return nil
}

// router compiles the URL map an LB's forwarding rule on port leads to.
func (p *plan) router(l *l7LB, port string) *urlmap.Router {
	p.t.Helper()
	for _, fr := range l.FRs {
		f := p.get(fr).(*computev1.ForwardingRule)
		if f.PortRange != port {
			continue
		}
		var um string
		switch px := p.get(f.Target).(type) {
		case *computev1.TargetHttpProxy:
			um = px.UrlMap
		case *computev1.TargetHttpsProxy:
			um = px.UrlMap
		}
		m := p.get(um).(*computev1.UrlMap)
		rt, err := urlmap.Compile(m)
		if err != nil {
			p.t.Fatalf("URL map %s: %v", um, err)
		}
		return rt
	}
	p.t.Fatalf("%s %s/%s has no forwarding rule on port %s", l.Kind, l.Namespace, l.Name, port)
	return nil
}

// backend routes a request and returns the Kubernetes Service port the
// selected backend service serves ("ns/name:port"), "default" for the
// default backend, or "redirect <code> <location>".
func (p *plan) backend(rt *urlmap.Router, host, path string, hdr ...string) string {
	p.t.Helper()
	res := p.route(rt, host, path, hdr...)
	if res.Redirect != nil {
		return "redirect " + strconv.Itoa(res.Redirect.Code) + " " + res.Redirect.Location
	}
	svc := res.Service
	if svc == "" && res.Action != nil && len(res.Action.WeightedBackendServices) > 0 {
		svc = res.Action.WeightedBackendServices[0].BackendService
	}
	return p.servicePort(svc)
}

// route routes a GET (or, with hdr ":method", another method) request.
func (p *plan) route(rt *urlmap.Router, host, path string, hdr ...string) *urlmap.Result {
	h := http.Header{}
	method := "GET"
	for i := 0; i+1 < len(hdr); i += 2 {
		if hdr[i] == ":method" {
			method = hdr[i+1]
			continue
		}
		h.Set(hdr[i], hdr[i+1])
	}
	pth, q, _ := strings.Cut(path, "?")
	return rt.Route(&urlmap.Request{Scheme: "http", Method: method, Host: host, Path: pth, RawQuery: q, Header: h}, func(int64) int64 { return 0 })
}

func (p *plan) servicePort(svc string) string {
	p.t.Helper()
	be := p.get(svc).(*computev1.BackendService)
	var d struct {
		Service string `json:"kubernetes.io/service-name"`
		Port    string `json:"kubernetes.io/service-port"`
	}
	if err := json.Unmarshal([]byte(be.Description), &d); err != nil || d.Service == "" {
		p.t.Fatalf("backend service %s description %q: %v", svc, be.Description, err)
	}
	if d.Service == "kube-system/default-http-backend" {
		return "default"
	}
	return d.Service + ":" + d.Port
}

// An Ingress with only a default backend becomes a classic external
// Application Load Balancer: a forwarding rule on port 80, a target HTTP
// proxy, a URL map and a backend service on the Service port's NEGs in
// every cluster zone, with a health check; the Service port needs NEGs.
func TestL7IngressDefaultBackend(t *testing.T) {
	f := newL7Fixture(t)
	f.add(svcJSON("default", "web", `{"name":"http","port":80,"targetPort":8080}`),
		`{"kind":"Ingress","metadata":{"namespace":"default","name":"web"},"spec":{"defaultBackend":{"service":{"name":"web","port":{"number":80}}}}}`)
	p := f.plan()
	l := p.lb("Ingress", "default", "web")
	if len(l.FRs) != 1 {
		t.Fatalf("forwarding rules %v, want one", l.FRs)
	}
	fr := p.get(l.FRs[0]).(*computev1.ForwardingRule)
	if fr.LoadBalancingScheme != "EXTERNAL" || fr.PortRange != "80" || fr.IPAddress != "" {
		t.Errorf("forwarding rule %+v, want EXTERNAL on port 80 with an ephemeral address", fr)
	}
	rt := p.router(l, "80")
	if got := p.backend(rt, "any.example.com", "/x"); got != "default/web:80" {
		t.Errorf("GET /x goes to %s, want default/web:80", got)
	}
	be := p.of("backendServices")
	var web *computev1.BackendService
	for _, r := range be {
		if b := r.Body.(*computev1.BackendService); p.servicePort(r.Path) == "default/web:80" {
			web = b
		}
	}
	if web == nil {
		t.Fatalf("no backend service for default/web:80 in %v", p.paths())
	}
	neg := negName("0123456789abcdef", "default", "web", 80)
	var groups []string
	for _, b := range web.Backends {
		groups = append(groups, b.Group)
	}
	if want := "zones/us-central1-a/networkEndpointGroups/" + neg + " zones/us-central1-b/networkEndpointGroups/" + neg; strings.Join(groups, " ") != want {
		t.Errorf("backends %v, want %s", groups, want)
	}
	if web.LoadBalancingScheme != "EXTERNAL" || len(web.HealthChecks) != 1 {
		t.Errorf("backend service %+v: want EXTERNAL with a health check", web)
	}
	p.get(web.HealthChecks[0])
	if !p.NEGPorts["default/web"][80] {
		t.Errorf("NEG ports %v, want default/web:80", p.NEGPorts)
	}
	for _, r := range p.Resources {
		name := r.Path[strings.LastIndex(r.Path, "/")+1:]
		if len(name) > 63 || !gceNameRE.MatchString(name) {
			t.Errorf("resource name %q is not a valid GCE name", name)
		}
	}
}

// Ingress rules route by host and path: Prefix matches whole path
// elements, Exact only the path, ImplementationSpecific is GKE's glob;
// rules without a host apply to every host; a backend whose Service is
// missing, like requests no rule matches, goes to the default backend.
func TestL7IngressRules(t *testing.T) {
	f := newL7Fixture(t)
	f.add(svcJSON("shop", "web", `{"name":"http","port":80}`),
		svcJSON("shop", "api", `{"name":"grpc","port":9000},{"name":"rest","port":8080}`),
		`{"kind":"Ingress","metadata":{"namespace":"shop","name":"site"},"spec":{"rules":[
			{"host":"shop.example.com","http":{"paths":[
				{"path":"/api","pathType":"Prefix","backend":{"service":{"name":"api","port":{"name":"rest"}}}},
				{"path":"/healthz","pathType":"Exact","backend":{"service":{"name":"api","port":{"number":9000}}}},
				{"path":"/static/*","pathType":"ImplementationSpecific","backend":{"service":{"name":"web","port":{"number":80}}}},
				{"path":"/gone","pathType":"Prefix","backend":{"service":{"name":"missing","port":{"number":80}}}}]}},
			{"http":{"paths":[{"path":"/","pathType":"Prefix","backend":{"service":{"name":"web","port":{"number":80}}}}]}}]}}`)
	p := f.plan()
	rt := p.router(p.lb("Ingress", "shop", "site"), "80")
	for _, c := range []struct{ host, path, want string }{
		{"shop.example.com", "/api", "shop/api:8080"},
		{"shop.example.com", "/api/v1/items", "shop/api:8080"},
		{"shop.example.com", "/apix", "default"},
		{"shop.example.com", "/healthz", "shop/api:9000"},
		{"shop.example.com", "/healthz/x", "default"},
		{"shop.example.com", "/static/app.js", "shop/web:80"},
		{"shop.example.com", "/gone/x", "default"},
		{"shop.example.com", "/other", "default"},
		{"other.example.com", "/anything", "shop/web:80"},
		{"other.example.com", "/api", "shop/web:80"},
	} {
		if got := p.backend(rt, c.host, c.path); got != c.want {
			t.Errorf("GET %s%s goes to %s, want %s", c.host, c.path, got, c.want)
		}
	}
	if p.NEGPorts["shop/missing"] != nil || !p.NEGPorts["shop/api"][8080] || !p.NEGPorts["shop/api"][9000] {
		t.Errorf("NEG ports %v", p.NEGPorts)
	}
}

// An Ingress with spec.tls also gets an HTTPS proxy with the Secrets'
// certificates (and pre-shared ones) and a forwarding rule on 443 at the
// HTTP rule's address, or at the static address named; allow-http
// "false" drops port 80. GKE's resource annotations name them. Ingresses
// of other classes are left alone.
func TestL7IngressTLS(t *testing.T) {
	f := newL7Fixture(t)
	f.in.Secret = func(ns, name string) (string, string, bool) {
		if ns == "default" && name == "tls" {
			return "CERT", "KEY", true
		}
		return "", "", false
	}
	backend := `"defaultBackend":{"service":{"name":"web","port":{"number":80}}}`
	f.add(svcJSON("default", "web", `{"port":80}`),
		`{"kind":"Ingress","metadata":{"namespace":"default","name":"static","annotations":{
			"kubernetes.io/ingress.global-static-ip-name":"web-ip","ingress.gcp.kubernetes.io/pre-shared-cert":"legacy"}},
			"spec":{`+backend+`,"tls":[{"secretName":"tls"}]}}`,
		`{"kind":"Ingress","metadata":{"namespace":"default","name":"shared"},"spec":{`+backend+`,"tls":[{"secretName":"tls"}]}}`,
		`{"kind":"Ingress","metadata":{"namespace":"default","name":"httpsonly","annotations":{"kubernetes.io/ingress.allow-http":"false"}},
			"spec":{`+backend+`,"tls":[{"secretName":"tls"}]}}`,
		`{"kind":"Ingress","metadata":{"namespace":"default","name":"nginx","annotations":{"kubernetes.io/ingress.class":"nginx"}},"spec":{`+backend+`}}`,
		`{"kind":"Ingress","metadata":{"namespace":"default","name":"other"},"spec":{"ingressClassName":"traefik",`+backend+`}}`)
	p := f.plan()
	if len(p.LBs) != 3 {
		t.Errorf("%d load balancers, want 3 (not the nginx and traefik ones)", len(p.LBs))
	}
	frs := func(l *l7LB) map[string]l7Resource {
		out := map[string]l7Resource{}
		for _, fr := range l.FRs {
			for _, r := range p.Resources {
				if r.Path == fr {
					out[r.Body.(*computev1.ForwardingRule).PortRange] = r
				}
			}
		}
		return out
	}

	static := p.lb("Ingress", "default", "static")
	s := frs(static)
	if len(s) != 2 || s["80"].Body.(*computev1.ForwardingRule).IPAddress != "global/addresses/web-ip" ||
		s["443"].Body.(*computev1.ForwardingRule).IPAddress != "global/addresses/web-ip" {
		t.Errorf("static Ingress forwarding rules %v, want 80 and 443 at global/addresses/web-ip", s)
	}
	px := p.get(s["443"].Body.(*computev1.ForwardingRule).Target).(*computev1.TargetHttpsProxy)
	if len(px.SslCertificates) != 2 {
		t.Fatalf("HTTPS proxy certificates %v, want the Secret's and legacy", px.SslCertificates)
	}
	cert := p.get(px.SslCertificates[0]).(*computev1.SslCertificate)
	if cert.Certificate != "CERT" || cert.PrivateKey != "KEY" || px.SslCertificates[1] != "global/sslCertificates/legacy" {
		t.Errorf("certificates %v: %+v", px.SslCertificates, cert)
	}
	if got := p.backend(p.router(static, "443"), "x", "/"); got != "default/web:80" {
		t.Errorf("HTTPS GET / goes to %s", got)
	}
	a := static.Annotations
	name := func(path string) string { return path[strings.LastIndex(path, "/")+1:] }
	if a["ingress.kubernetes.io/forwarding-rule"] != name(s["80"].Path) || a["ingress.kubernetes.io/https-forwarding-rule"] != name(s["443"].Path) ||
		a["ingress.kubernetes.io/https-target-proxy"] != name(s["443"].Body.(*computev1.ForwardingRule).Target) ||
		a["ingress.kubernetes.io/target-proxy"] == "" || a["ingress.kubernetes.io/url-map"] != name(px.UrlMap) ||
		a["ingress.kubernetes.io/ssl-cert"] != name(px.SslCertificates[0])+",legacy" {
		t.Errorf("annotations %v", a)
	}

	sh := frs(p.lb("Ingress", "default", "shared"))
	if len(sh) != 2 || sh["80"].Body.(*computev1.ForwardingRule).IPAddress != "" || sh["443"].SameIPAs != sh["80"].Path {
		t.Errorf("shared Ingress forwarding rules %+v, want 443 at the ephemeral address of 80", sh)
	}
	for i, r := range p.Resources {
		if r.Path == sh["443"].Path {
			for _, before := range p.Resources[:i] {
				if before.Path == sh["80"].Path {
					goto ordered
				}
			}
			t.Error("the 443 forwarding rule comes before the one whose address it shares")
		ordered:
		}
	}
	if ho := frs(p.lb("Ingress", "default", "httpsonly")); len(ho) != 1 || ho["443"].Path == "" || ho["443"].SameIPAs != "" {
		t.Errorf("allow-http false: forwarding rules %v, want 443 only", ho)
	}
}

const gwGVK = `"apiVersion":"gateway.networking.k8s.io/v1"`

func cond(conds []gwCondition, typ string) gwCondition {
	for _, c := range conds {
		if c.Type == typ {
			return c
		}
	}
	return gwCondition{}
}

// A Gateway of class gke-l7-global-external-managed is a global external
// Application Load Balancer (EXTERNAL_MANAGED) built from its HTTPRoutes:
// PathPrefix matches whole path elements, a rule without matches takes
// the rest of the route's hostnames, and other hosts get the default
// backend. Listener and route status report the attachment. Gateways of
// other classes are left alone.
func TestL7Gateway(t *testing.T) {
	f := newL7Fixture(t)
	f.add(svcJSON("store", "v1", `{"port":8080}`), svcJSON("store", "v2", `{"port":8080}`),
		`{"kind":"Gateway",`+gwGVK+`,"metadata":{"namespace":"store","name":"ext","generation":3},"spec":{
			"gatewayClassName":"gke-l7-global-external-managed","listeners":[{"name":"http","port":80,"protocol":"HTTP"}]}}`,
		`{"kind":"Gateway",`+gwGVK+`,"metadata":{"namespace":"store","name":"istio"},"spec":{
			"gatewayClassName":"istio","listeners":[{"name":"http","port":80,"protocol":"HTTP"}]}}`,
		`{"kind":"HTTPRoute",`+gwGVK+`,"metadata":{"namespace":"store","name":"store","generation":2},"spec":{
			"parentRefs":[{"name":"ext"}],"hostnames":["store.example.com"],"rules":[
			{"matches":[{"path":{"type":"PathPrefix","value":"/v2"}}],"backendRefs":[{"name":"v2","port":8080}]},
			{"backendRefs":[{"name":"v1","port":8080}]}]}}`)
	p := f.plan()
	if len(p.LBs) != 1 {
		t.Fatalf("%d load balancers, want only the gke-l7 Gateway's", len(p.LBs))
	}
	l := p.lb("Gateway", "store", "ext")
	fr := p.get(l.FRs[0]).(*computev1.ForwardingRule)
	if fr.LoadBalancingScheme != "EXTERNAL_MANAGED" || fr.PortRange != "80" {
		t.Errorf("forwarding rule %+v", fr)
	}
	rt := p.router(l, "80")
	for _, c := range []struct{ host, path, want string }{
		{"store.example.com", "/v2", "store/v2:8080"},
		{"store.example.com", "/v2/cart", "store/v2:8080"},
		{"store.example.com", "/v2x", "store/v1:8080"},
		{"store.example.com", "/", "store/v1:8080"},
		{"other.example.com", "/v2", "default"},
	} {
		if got := p.backend(rt, c.host, c.path); got != c.want {
			t.Errorf("GET %s%s goes to %s, want %s", c.host, c.path, got, c.want)
		}
	}
	for _, r := range p.of("backendServices") {
		if s := r.Body.(*computev1.BackendService).LoadBalancingScheme; s != "EXTERNAL_MANAGED" {
			t.Errorf("backend service %s scheme %s", r.Path, s)
		}
	}
	if len(l.Listeners) != 1 || l.Listeners[0].Name != "http" || l.Listeners[0].AttachedRoutes != 1 ||
		cond(l.Listeners[0].Conditions, "Accepted").Status != "True" || cond(l.Listeners[0].Conditions, "ResolvedRefs").Status != "True" ||
		cond(l.Listeners[0].Conditions, "Accepted").ObservedGeneration != 3 {
		t.Errorf("listener status %+v", l.Listeners)
	}
	ps := p.RouteParents["store/store"]
	if len(ps) != 1 || ps[0].ParentRef.Name != "ext" || ps[0].ControllerName != "networking.gke.io/gateway" ||
		cond(ps[0].Conditions, "Accepted").Status != "True" || cond(ps[0].Conditions, "ResolvedRefs").Status != "True" ||
		cond(ps[0].Conditions, "Accepted").ObservedGeneration != 2 {
		t.Errorf("route parents %+v", ps)
	}
}

// HTTPRoute matches follow Gateway API precedence across routes (exact
// paths, longer prefixes, methods, header and query matches, then older
// routes), and its filters and weights carry over: header modifiers,
// redirects, prefix rewrites and weighted backends.
func TestL7RouteMatching(t *testing.T) {
	f := newL7Fixture(t)
	for _, n := range []string{"api", "canary", "health", "debug", "a", "b", "other", "form"} {
		f.add(svcJSON("app", n, `{"port":80}`))
	}
	be := func(n string) string { return `"backendRefs":[{"name":"` + n + `","port":80}]` }
	prefix := func(v string) string { return `"path":{"type":"PathPrefix","value":"` + v + `"}` }
	f.add(`{"kind":"Gateway",`+gwGVK+`,"metadata":{"namespace":"app","name":"gw"},"spec":{
			"gatewayClassName":"gke-l7-gxlb","listeners":[{"name":"http","port":80,"protocol":"HTTP","hostname":"app.example.com"}]}}`,
		`{"kind":"HTTPRoute",`+gwGVK+`,"metadata":{"namespace":"app","name":"newer","creationTimestamp":"2026-02-01T00:00:00Z"},"spec":{
			"parentRefs":[{"name":"gw"}],"rules":[{"matches":[{`+prefix("/api")+`}],`+be("other")+`}]}}`,
		`{"kind":"HTTPRoute",`+gwGVK+`,"metadata":{"namespace":"app","name":"older","creationTimestamp":"2026-01-01T00:00:00Z"},"spec":{
			"parentRefs":[{"name":"gw"}],"rules":[
			{"matches":[{`+prefix("/api")+`}],"filters":[{"type":"URLRewrite","urlRewrite":{"path":{"type":"ReplacePrefixMatch","replacePrefixMatch":"/v1"}}}],`+be("api")+`},
			{"matches":[{`+prefix("/api")+`,"headers":[{"name":"x-canary","value":"1"}]}],`+be("canary")+`},
			{"matches":[{"path":{"type":"Exact","value":"/api/health"}}],`+be("health")+`},
			{"matches":[{`+prefix("/old")+`}],"filters":[{"type":"RequestRedirect","requestRedirect":{"path":{"type":"ReplacePrefixMatch","replacePrefixMatch":"/new"},"statusCode":301}}]},
			{"matches":[{`+prefix("/submit")+`,"method":"POST"}],`+be("form")+`},
			{"matches":[{"queryParams":[{"name":"debug","value":"true"}]}],`+be("debug")+`},
			{"matches":[{`+prefix("/split")+`}],"backendRefs":[{"name":"a","port":80,"weight":90},{"name":"b","port":80,"weight":10},{"name":"other","port":80,"weight":0}]},
			{"matches":[{`+prefix("/hdr")+`}],"filters":[{"type":"RequestHeaderModifier","requestHeaderModifier":{"set":[{"name":"x-env","value":"test"}],"remove":["x-drop"]}},
				{"type":"ResponseHeaderModifier","responseHeaderModifier":{"add":[{"name":"x-served-by","value":"gw"}]}}],`+be("api")+`}]}}`)
	p := f.plan()
	l := p.lb("Gateway", "app", "gw")
	if fr := p.get(l.FRs[0]).(*computev1.ForwardingRule); fr.LoadBalancingScheme != "EXTERNAL" {
		t.Errorf("gke-l7-gxlb scheme %s, want EXTERNAL", fr.LoadBalancingScheme)
	}
	rt := p.router(l, "80")
	const h = "app.example.com"
	for _, c := range []struct {
		path string
		hdr  []string
		want string
	}{
		{"/api/health", nil, "app/health:80"},
		{"/api/x", []string{"x-canary", "1"}, "app/canary:80"},
		{"/api/x", nil, "app/api:80"},
		{"/old/page", nil, "redirect 301 http://app.example.com/new/page"},
		{"/submit", []string{":method", "POST"}, "app/form:80"},
		{"/submit", nil, "default"},
		{"/?debug=true", nil, "app/debug:80"},
		{"/", nil, "default"},
		{"/split", nil, "app/a:80"},
	} {
		if got := p.backend(rt, h, c.path, c.hdr...); got != c.want {
			t.Errorf("%v %s goes to %s, want %s", c.hdr, c.path, got, c.want)
		}
	}
	for path, want := range map[string]string{"/api/x": "/v1/x", "/api": "/v1"} {
		if res := p.route(rt, h, path); res.Path != want {
			t.Errorf("%s rewritten to %s, want %s", path, res.Path, want)
		}
	}
	split := p.route(rt, h, "/split")
	if a := split.Action; a == nil || len(a.WeightedBackendServices) != 2 || a.WeightedBackendServices[0].Weight != 90 || a.WeightedBackendServices[1].Weight != 10 {
		t.Errorf("/split action %+v, want weights 90 and 10", split.Action)
	}
	hdr := p.route(rt, h, "/hdr")
	var set, drop, resp bool
	for _, ha := range hdr.HeaderActions {
		for _, o := range ha.RequestHeadersToAdd {
			set = set || o.HeaderName == "x-env" && o.HeaderValue == "test" && o.Replace
		}
		for _, n := range ha.RequestHeadersToRemove {
			drop = drop || n == "x-drop"
		}
		for _, o := range ha.ResponseHeadersToAdd {
			resp = resp || o.HeaderName == "x-served-by" && o.HeaderValue == "gw" && !o.Replace
		}
	}
	if !set || !drop || !resp {
		t.Errorf("/hdr header actions %+v: set %v, remove %v, response %v", hdr.HeaderActions, set, drop, resp)
	}
}

// Gateway listeners and route attachment: an HTTPS listener serves its
// Secret's certificate on its own port at the Gateway's named address;
// a missing Secret, an unsupported protocol, a namespace the listener
// doesn't allow, a hostname no listener serves and a missing backend
// show in status, and requests for the missing backend get the default.
func TestL7GatewayStatus(t *testing.T) {
	f := newL7Fixture(t)
	f.in.Secret = func(ns, name string) (string, string, bool) {
		return "CERT", "KEY", ns == "infra" && name == "site-tls"
	}
	f.add(svcJSON("infra", "site", `{"port":80}`), svcJSON("team", "app", `{"port":80}`),
		`{"kind":"Gateway",`+gwGVK+`,"metadata":{"namespace":"infra","name":"gw"},"spec":{
			"gatewayClassName":"gke-l7-global-external-managed","addresses":[{"type":"NamedAddress","value":"site-ip"}],"listeners":[
			{"name":"http","port":80,"protocol":"HTTP","hostname":"*.example.com","allowedRoutes":{"namespaces":{"from":"All"}}},
			{"name":"https","port":443,"protocol":"HTTPS","hostname":"site.example.com","tls":{"certificateRefs":[{"name":"site-tls"}]}},
			{"name":"broken","port":8080,"protocol":"HTTPS","tls":{"certificateRefs":[{"name":"missing"}]}},
			{"name":"tcp","port":80,"protocol":"TCP"}]}}`,
		`{"kind":"HTTPRoute",`+gwGVK+`,"metadata":{"namespace":"infra","name":"site"},"spec":{
			"parentRefs":[{"name":"gw"}],"hostnames":["site.example.com"],"rules":[
			{"matches":[{"path":{"type":"PathPrefix","value":"/gone"}}],"backendRefs":[{"name":"nope","port":80}]},
			{"backendRefs":[{"name":"site","port":80}]}]}}`,
		`{"kind":"HTTPRoute",`+gwGVK+`,"metadata":{"namespace":"team","name":"app"},"spec":{
			"parentRefs":[{"name":"gw","namespace":"infra"}],"hostnames":["app.example.com"],"rules":[{"backendRefs":[{"name":"app","port":80}]}]}}`,
		`{"kind":"HTTPRoute",`+gwGVK+`,"metadata":{"namespace":"team","name":"private"},"spec":{
			"parentRefs":[{"name":"gw","namespace":"infra","sectionName":"https"}],"rules":[{"backendRefs":[{"name":"app","port":80}]}]}}`,
		`{"kind":"HTTPRoute",`+gwGVK+`,"metadata":{"namespace":"infra","name":"elsewhere"},"spec":{
			"parentRefs":[{"name":"gw"}],"hostnames":["other.test"],"rules":[{"backendRefs":[{"name":"site","port":80}]}]}}`)
	p := f.plan()
	l := p.lb("Gateway", "infra", "gw")
	if len(l.FRs) != 2 {
		t.Fatalf("forwarding rules %v, want ports 80 and 443 (8080 has no certificate)", l.FRs)
	}
	for _, fr := range l.FRs {
		if ip := p.get(fr).(*computev1.ForwardingRule).IPAddress; ip != "global/addresses/site-ip" {
			t.Errorf("%s address %q, want global/addresses/site-ip", fr, ip)
		}
	}
	http, https := p.router(l, "80"), p.router(l, "443")
	for _, c := range []struct {
		rt         *urlmap.Router
		host, path string
		want       string
	}{
		{http, "site.example.com", "/", "infra/site:80"},
		{http, "site.example.com", "/gone/x", "default"},
		{http, "app.example.com", "/", "team/app:80"},
		{https, "site.example.com", "/", "infra/site:80"},
		{https, "app.example.com", "/", "default"},
		{http, "other.test", "/", "default"},
	} {
		if got := p.backend(c.rt, c.host, c.path); got != c.want {
			t.Errorf("%s%s goes to %s, want %s", c.host, c.path, got, c.want)
		}
	}
	ls := map[string]gwListenerStatus{}
	for _, s := range l.Listeners {
		ls[s.Name] = s
	}
	if s := ls["http"]; s.AttachedRoutes != 2 || cond(s.Conditions, "Accepted").Status != "True" {
		t.Errorf("http listener %+v, want 2 routes", s)
	}
	if s := ls["https"]; s.AttachedRoutes != 1 || cond(s.Conditions, "ResolvedRefs").Status != "True" {
		t.Errorf("https listener %+v, want 1 route", s)
	}
	if c := cond(ls["broken"].Conditions, "ResolvedRefs"); c.Status != "False" || c.Reason != "InvalidCertificateRef" {
		t.Errorf("broken listener ResolvedRefs %+v", c)
	}
	if c := cond(ls["tcp"].Conditions, "Accepted"); c.Status != "False" || c.Reason != "UnsupportedProtocol" {
		t.Errorf("tcp listener Accepted %+v", c)
	}
	parent := func(route, typ string) gwCondition {
		ps := p.RouteParents[route]
		if len(ps) != 1 {
			t.Fatalf("%s parents %+v, want one", route, ps)
		}
		return cond(ps[0].Conditions, typ)
	}
	if c := parent("infra/site", "ResolvedRefs"); c.Status != "False" || c.Reason != "BackendNotFound" {
		t.Errorf("infra/site ResolvedRefs %+v", c)
	}
	if c := parent("infra/site", "Accepted"); c.Status != "True" {
		t.Errorf("infra/site Accepted %+v", c)
	}
	if c := parent("team/app", "Accepted"); c.Status != "True" {
		t.Errorf("team/app Accepted %+v", c)
	}
	if c := parent("team/private", "Accepted"); c.Status != "False" || c.Reason != "NotAllowedByListeners" {
		t.Errorf("team/private Accepted %+v", c)
	}
	if c := parent("infra/elsewhere", "Accepted"); c.Status != "False" || c.Reason != "NoMatchingListenerHostname" {
		t.Errorf("infra/elsewhere Accepted %+v", c)
	}
}

// A Gateway is handled only when its GatewayClass is GKE's; a listener on
// another listener's port with another protocol is a conflict, and a
// Gateway without a listener it can serve is not valid.
func TestL7GatewayClassAndConflicts(t *testing.T) {
	f := newL7Fixture(t)
	f.in.Classes = map[string]bool{"gke-l7-global-external-managed": true}
	f.add(`{"kind":"Gateway",`+gwGVK+`,"metadata":{"namespace":"a","name":"foreign"},"spec":{
			"gatewayClassName":"gke-l7-gxlb","listeners":[{"name":"http","port":80,"protocol":"HTTP"}]}}`,
		`{"kind":"Gateway",`+gwGVK+`,"metadata":{"namespace":"a","name":"conflict"},"spec":{
			"gatewayClassName":"gke-l7-global-external-managed","listeners":[
			{"name":"web","port":80,"protocol":"HTTP"},{"name":"tls","port":80,"protocol":"HTTPS","tls":{"options":{"networking.gke.io/pre-shared-certs":"c"}}}]}}`,
		`{"kind":"Gateway",`+gwGVK+`,"metadata":{"namespace":"a","name":"none"},"spec":{
			"gatewayClassName":"gke-l7-global-external-managed","listeners":[{"name":"udp","port":53,"protocol":"UDP"}]}}`)
	p := f.plan()
	if len(p.LBs) != 2 {
		t.Fatalf("%d load balancers, want 2 (not the Gateway of a foreign class)", len(p.LBs))
	}
	c := p.lb("Gateway", "a", "conflict")
	if !c.Valid || cond(c.Listeners[1].Conditions, "Conflicted").Status != "True" || cond(c.Listeners[1].Conditions, "Conflicted").Reason != "ProtocolConflict" ||
		cond(c.Listeners[0].Conditions, "Conflicted").Status != "False" {
		t.Errorf("conflicting listeners %+v", c.Listeners)
	}
	if n := p.lb("Gateway", "a", "none"); n.Valid || len(n.FRs) != 0 {
		t.Errorf("Gateway without a servable listener: valid %v, forwarding rules %v", n.Valid, n.FRs)
	}
}
