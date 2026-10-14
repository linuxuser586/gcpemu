package gke

import (
	"encoding/json"
	"regexp"
	"sort"
	"strconv"
	"strings"

	computev1 "google.golang.org/api/compute/v1"
)

// The Ingress and Gateway API controllers' translation (FR-GKE-010):
// Kubernetes objects in, the load balancer resources GKE would create out,
// wired forwarding rule → target proxy → URL map → backend service → NEG,
// all global:
//
//   - an Ingress of class gce (or none) is a classic external Application
//     Load Balancer (EXTERNAL), with a forwarding rule on port 80 and,
//     with spec.tls, one on 443 at the same address;
//   - a Gateway of class gke-l7-global-external-managed (EXTERNAL_MANAGED)
//     or gke-l7-gxlb (EXTERNAL) gets a forwarding rule, target proxy and
//     URL map per listener port, built from the HTTPRoutes attached to it.
//
// Every Kubernetes Service port a load balancer sends to is a backend
// service on the port's standalone NEGs in each cluster zone (container-native load
// balancing, as GKE does on VPC-native clusters), checked by an HTTP
// health check on the serving port. Requests no rule matches go to the
// cluster's default backend: a backend service without backends, which
// answers 503 where GKE's default-http-backend pod answers 404.
// translateL7 is pure; l7.go applies its plan.

// l7Cluster is what the translation needs of the cluster.
type l7Cluster struct {
	Project string
	ID      string
	Zones   []string
}

// l7Input is the cluster's Kubernetes objects.
type l7Input struct {
	Cluster   l7Cluster
	Services  []kubeService
	Ingresses []kubeIngress
	Gateways  []kubeGateway
	Routes    []kubeHTTPRoute
	// Classes are the GatewayClasses whose controllerName is GKE's.
	Classes map[string]bool
	// Secret returns a kubernetes.io/tls Secret's PEM certificate chain
	// and key.
	Secret func(ns, name string) (cert, key string, ok bool)
}

// l7Plan is the desired state.
type l7Plan struct {
	// Resources are in dependency order, each path once.
	Resources []l7Resource
	// NEGPorts are the Service ports needing NEGs, by namespace/name.
	NEGPorts map[string]map[int]bool
	// LBs are the load balancers, one per Ingress or Gateway handled.
	LBs []*l7LB
	// RouteParents is the status of each HTTPRoute (by namespace/name)
	// for its parents that are Gateways handled.
	RouteParents map[string][]gwParentStatus
}

// l7Resource is one load balancer resource.
type l7Resource struct {
	// Path is the relative name ("projects/P/global/urlMaps/M").
	Path string
	// Body is its compute v1 form (*computev1.UrlMap, ...).
	Body any
	// SameIPAs names a forwarding rule whose address this one, given none,
	// shares.
	SameIPAs string
}

// l7LB is the load balancer of one Ingress or Gateway.
type l7LB struct {
	Kind, Namespace, Name string
	// FRs are its forwarding rules' paths; the first one's address is the
	// load balancer's.
	FRs []string
	// Annotations are what GKE reports on an Ingress.
	Annotations map[string]string
	// Generation and Listeners are a Gateway's, for its status; Valid
	// reports whether any listener is served.
	Generation int64
	Listeners  []gwListenerStatus
	Valid      bool
}

var gceNameRE = regexp.MustCompile(`^[a-z](?:[-a-z0-9]{0,61}[a-z0-9])?$`)

// Ingress classes and GatewayClasses handled, by the backend services'
// load balancing scheme.
const (
	ingressClass   = "gce"
	gwController   = "networking.gke.io/gateway"
	schemeClassic  = "EXTERNAL"
	schemeManaged  = "EXTERNAL_MANAGED"
	defaultBackend = "kube-system/default-http-backend"
)

// gatewayClasses maps the GatewayClasses handled to their scheme.
var gatewayClasses = map[string]string{
	"gke-l7-global-external-managed": schemeManaged,
	"gke-l7-gxlb":                    schemeClassic,
}

// translator accumulates a plan.
type translator struct {
	in   l7Input
	svcs map[string]*kubeService
	res  map[string]l7Resource
	plan l7Plan
}

func translateL7(in l7Input) l7Plan {
	t := &translator{in: in, svcs: map[string]*kubeService{}, res: map[string]l7Resource{}}
	t.plan.NEGPorts = map[string]map[int]bool{}
	t.plan.RouteParents = map[string][]gwParentStatus{}
	for i := range in.Services {
		s := &in.Services[i]
		t.svcs[s.Metadata.Namespace+"/"+s.Metadata.Name] = s
	}
	ings := append([]kubeIngress(nil), in.Ingresses...)
	sort.Slice(ings, func(i, j int) bool { return objKey(ings[i].Metadata) < objKey(ings[j].Metadata) })
	for i := range ings {
		if ingressHandled(&ings[i]) {
			t.ingress(&ings[i])
		}
	}
	t.gateways()
	for _, r := range t.res {
		t.plan.Resources = append(t.plan.Resources, r)
	}
	sort.Slice(t.plan.Resources, func(i, j int) bool {
		a, b := t.plan.Resources[i], t.plan.Resources[j]
		if ra, rb := collRank(a.Path), collRank(b.Path); ra != rb {
			return ra < rb
		}
		// A forwarding rule sharing another's address comes after it.
		if (a.SameIPAs == "") != (b.SameIPAs == "") {
			return a.SameIPAs == ""
		}
		return a.Path < b.Path
	})
	return t.plan
}

func objKey(m objectMeta) string { return m.Namespace + "/" + m.Name }

// l7Colls lists the collections in dependency order.
var l7Colls = []string{"healthChecks", "sslCertificates", "backendServices", "urlMaps", "targetHttpProxies", "targetHttpsProxies", "forwardingRules"}

func isForwardingRule(path string) bool { return collRank(path) == len(l7Colls)-1 }

// collRank orders a resource path by its collection's dependencies.
func collRank(path string) int {
	segs := strings.Split(path, "/")
	if len(segs) < 2 {
		return len(l7Colls)
	}
	for i, c := range l7Colls {
		if segs[len(segs)-2] == c {
			return i
		}
	}
	return len(l7Colls)
}

func (t *translator) global(coll, name string) string {
	return "projects/" + t.in.Cluster.Project + "/global/" + coll + "/" + name
}

func (t *translator) put(r l7Resource) string {
	t.res[r.Path] = r
	return r.Path
}

// name builds a resource name like GKE's controllers do:
// PREFIX-KIND-CLUSTER-NAMESPACE-NAME-HASH, at most 63 characters.
func (t *translator) name(prefix, kind, ns, name string, extra ...string) string {
	id := t.in.Cluster.ID
	if len(id) > 8 {
		id = id[:8]
	}
	h := shortHash(strings.Join(append([]string{t.in.Cluster.ID, prefix, kind, ns, name}, extra...), "/"))
	return strings.ToLower(prefix + "-" + kind + "-" + id + "-" + gceSafe(ns, 10) + "-" + gceSafe(name, 15) + "-" + h)
}

// gceSafe truncates a Kubernetes name to n characters valid in a GCE name.
func gceSafe(s string, n int) string {
	s = strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' {
			return r
		}
		if r >= 'A' && r <= 'Z' {
			return r + 'a' - 'A'
		}
		return '-'
	}, s)
	return trunc(s, n)
}

// schemePrefix is the name prefix of the resources of a scheme.
func schemePrefix(scheme string) string {
	if scheme == schemeManaged {
		return "gkegw1"
	}
	return "k8s2"
}

// backendDesc is the description GKE gives a backend service.
func backendDesc(svc, port string) string {
	b, _ := json.Marshal(map[string]string{"kubernetes.io/service-name": svc, "kubernetes.io/service-port": port})
	return string(b)
}

// defaultBackendService returns the cluster's default backend of scheme.
func (t *translator) defaultBackendService(scheme string) string {
	path := t.global("backendServices", t.name(schemePrefix(scheme), "be", "kube-system", "default-http-backend"))
	return t.put(l7Resource{Path: path, Body: &computev1.BackendService{
		LoadBalancingScheme: scheme, Protocol: "HTTP", Description: backendDesc(defaultBackend, "http"),
	}})
}

// servicePort resolves a Service port by number or name; ok is false when
// the Service or the port is missing.
func (t *translator) servicePort(ns, name string, number int, portName string) (*kubeService, kubeServicePort, bool) {
	s := t.svcs[ns+"/"+name]
	if s == nil {
		return nil, kubeServicePort{}, false
	}
	for _, p := range s.Spec.Ports {
		if number != 0 && p.Port == number || number == 0 && portName != "" && p.Name == portName {
			return s, p, true
		}
	}
	return s, kubeServicePort{}, false
}

// backendService returns the backend service of a Service port, on its
// NEGs, or "" when the Service or port is missing.
func (t *translator) backendService(scheme, ns, name string, number int, portName string) string {
	svc, sp, ok := t.servicePort(ns, name, number, portName)
	if !ok {
		return ""
	}
	key := ns + "/" + name
	if t.plan.NEGPorts[key] == nil {
		t.plan.NEGPorts[key] = map[int]bool{}
	}
	t.plan.NEGPorts[key][sp.Port] = true
	prefix := schemePrefix(scheme)
	bname := t.name(prefix, "be", ns, name, strconv.Itoa(sp.Port))
	hc := t.put(l7Resource{Path: t.global("healthChecks", bname), Body: &computev1.HealthCheck{
		Type:            "HTTP",
		HttpHealthCheck: &computev1.HTTPHealthCheck{PortSpecification: "USE_SERVING_PORT", RequestPath: "/"},
	}})
	neg := negNameFor(t.in.Cluster.ID, svc, sp.Port)
	be := &computev1.BackendService{
		LoadBalancingScheme: scheme, Protocol: backendProtocol(sp), HealthChecks: []string{hc},
		Description: backendDesc(key, strconv.Itoa(sp.Port)),
	}
	for _, z := range t.in.Cluster.Zones {
		be.Backends = append(be.Backends, &computev1.Backend{
			Group: "zones/" + z + "/networkEndpointGroups/" + neg, BalancingMode: "RATE", MaxRatePerEndpoint: 100,
		})
	}
	return t.put(l7Resource{Path: t.global("backendServices", bname), Body: be})
}

// backendProtocol follows the Service port's appProtocol.
func backendProtocol(sp kubeServicePort) string {
	switch strings.ToLower(sp.AppProtocol) {
	case "https":
		return "HTTPS"
	case "http2":
		return "HTTP2"
	case "kubernetes.io/h2c":
		return "H2C"
	}
	return "HTTP"
}

// ---- Ingress ----

// ingressHandled reports whether the Ingress is of class gce.
func ingressHandled(ing *kubeIngress) bool {
	if c, ok := ing.Metadata.Annotations["kubernetes.io/ingress.class"]; ok {
		return c == ingressClass
	}
	return ing.Spec.IngressClassName == nil || *ing.Spec.IngressClassName == ingressClass
}

// ingressBackendService returns the backend service of an Ingress backend.
func (t *translator) ingressBackendService(ns string, b *ingressBackend) string {
	if b == nil || b.Service == nil {
		return ""
	}
	return t.backendService(schemeClassic, ns, b.Service.Name, b.Service.Port.Number, b.Service.Port.Name)
}

func (t *translator) ingress(ing *kubeIngress) {
	ns, name := ing.Metadata.Namespace, ing.Metadata.Name
	lb := &l7LB{Kind: "Ingress", Namespace: ns, Name: name, Annotations: map[string]string{}}
	t.plan.LBs = append(t.plan.LBs, lb)
	def := t.ingressBackendService(ns, ing.Spec.DefaultBackend)
	if def == "" {
		def = t.defaultBackendService(schemeClassic)
	}
	um := &computev1.UrlMap{DefaultService: def}
	t.ingressRules(ing, um)
	umPath := t.put(l7Resource{Path: t.global("urlMaps", t.name("k8s2", "um", ns, name)), Body: um})
	lb.Annotations["ingress.kubernetes.io/url-map"] = lastSeg(umPath)
	ann := ing.Metadata.Annotations
	ip := ""
	if n := ann["kubernetes.io/ingress.global-static-ip-name"]; n != "" {
		ip = "global/addresses/" + n
	}
	addForwardingRule := func(kind, port, proxy, annoPrefix string) {
		fr := t.forwardingRule(lb, t.global("forwardingRules", t.name("k8s2", kind, ns, name)), schemeClassic, port, proxy, ip)
		lb.Annotations["ingress.kubernetes.io/"+annoPrefix+"forwarding-rule"] = lastSeg(fr)
		lb.Annotations["ingress.kubernetes.io/"+annoPrefix+"target-proxy"] = lastSeg(proxy)
	}
	if ann["kubernetes.io/ingress.allow-http"] != "false" {
		tp := t.put(l7Resource{Path: t.global("targetHttpProxies", t.name("k8s2", "tp", ns, name)), Body: &computev1.TargetHttpProxy{UrlMap: umPath}})
		addForwardingRule("fr", "80", tp, "")
	}
	var certs, certNames []string
	for _, tls := range ing.Spec.TLS {
		if tls.SecretName == "" {
			continue
		}
		if c := t.secretCert("k8s2", ns, tls.SecretName); c != "" {
			certs = append(certs, c)
		}
	}
	certs = append(certs, preSharedCerts(ann["ingress.gcp.kubernetes.io/pre-shared-cert"])...)
	if len(certs) > 0 {
		for _, c := range certs {
			certNames = append(certNames, lastSeg(c))
		}
		lb.Annotations["ingress.kubernetes.io/ssl-cert"] = strings.Join(certNames, ",")
		ts := t.put(l7Resource{Path: t.global("targetHttpsProxies", t.name("k8s2", "ts", ns, name)), Body: &computev1.TargetHttpsProxy{UrlMap: umPath, SslCertificates: certs}})
		addForwardingRule("fs", "443", ts, "https-")
	}
}

// forwardingRule adds a forwarding rule of lb at ip, or with none at its
// first forwarding rule's address, and returns its path.
func (t *translator) forwardingRule(lb *l7LB, path, scheme, port, proxy, ip string) string {
	fr := l7Resource{Path: path, Body: &computev1.ForwardingRule{
		LoadBalancingScheme: scheme, IPProtocol: "TCP", PortRange: port, Target: proxy, IPAddress: ip,
	}}
	if ip == "" && len(lb.FRs) > 0 {
		fr.SameIPAs = lb.FRs[0]
	}
	lb.FRs = append(lb.FRs, t.put(fr))
	return fr.Path
}

// preSharedCerts returns the SSL certificates a comma-separated list names.
func preSharedCerts(names string) []string {
	var out []string
	for _, n := range strings.Split(names, ",") {
		if n = strings.TrimSpace(n); n != "" {
			out = append(out, "global/sslCertificates/"+n)
		}
	}
	return out
}

// secretCert returns a self-managed SSL certificate holding a Kubernetes
// TLS Secret,
// named after its content so that a renewed certificate is a new one, or
// "" when the Secret is missing.
func (t *translator) secretCert(prefix, ns, secret string) string {
	cert, key, ok := t.in.Secret(ns, secret)
	if !ok {
		return ""
	}
	return t.put(l7Resource{Path: t.global("sslCertificates", t.name(prefix, "cr", ns, secret, shortHash(cert+key))),
		Body: &computev1.SslCertificate{Certificate: cert, PrivateKey: key}})
}

// ingressRules adds a host rule and path matcher per host of the Ingress
// rules to um; rules without a host apply to every host ("*").
func (t *translator) ingressRules(ing *kubeIngress, um *computev1.UrlMap) {
	ns := ing.Metadata.Namespace
	matchers := map[string]*computev1.PathMatcher{}
	var hosts []string
	for _, r := range ing.Spec.Rules {
		if r.HTTP == nil {
			continue
		}
		host := r.Host
		if host == "" {
			host = "*"
		}
		pm := matchers[host]
		if pm == nil {
			pm = &computev1.PathMatcher{DefaultService: um.DefaultService}
			matchers[host] = pm
			hosts = append(hosts, host)
		}
		seen := map[string]bool{}
		for _, pr := range pm.PathRules {
			for _, p := range pr.Paths {
				seen[p] = true
			}
		}
		for _, p := range r.HTTP.Paths {
			be := t.ingressBackendService(ns, &p.Backend)
			if be == "" {
				be = t.defaultBackendService(schemeClassic)
			}
			var paths []string
			for _, gp := range ingressPaths(p.Path, p.PathType) {
				if !seen[gp] {
					seen[gp] = true
					paths = append(paths, gp)
				}
			}
			if len(paths) > 0 {
				pm.PathRules = append(pm.PathRules, &computev1.PathRule{Paths: paths, Service: be})
			}
		}
	}
	sort.Strings(hosts)
	for i, h := range hosts {
		pm := matchers[h]
		pm.Name = "host" + strconv.Itoa(i)
		um.PathMatchers = append(um.PathMatchers, pm)
		um.HostRules = append(um.HostRules, &computev1.HostRule{Hosts: []string{h}, PathMatcher: pm.Name})
	}
}

// ingressPaths returns the URL map paths of an Ingress path: Prefix
// matches whole path elements, Exact the path alone, and
// ImplementationSpecific is GKE's glob, the path as given.
func ingressPaths(path, typ string) []string {
	if path == "" {
		path = "/"
	}
	switch typ {
	case "Exact":
		return []string{path}
	case "Prefix":
		p := strings.TrimRight(path, "/")
		if p == "" {
			return []string{"/*"}
		}
		return []string{p, p + "/*"}
	}
	if path == "/" {
		return []string{"/*"}
	}
	return []string{path}
}
