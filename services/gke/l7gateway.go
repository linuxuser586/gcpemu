package gke

import (
	"sort"
	"strconv"
	"strings"

	computev1 "google.golang.org/api/compute/v1"
)

// Gateway API translation (FR-GKE-010), part of translateL7. A Gateway
// gets a forwarding rule, target proxy and URL map per listener port; the
// HTTPRoutes attached to a port's listeners make up its URL map, one path
// matcher per hostname with a route rule per match, in Gateway API
// precedence: exact paths, then longer prefixes, then methods, more header
// matches and more query matches, then older routes. A PathPrefix match is
// a full-path rule for the prefix and a prefix rule for the prefix plus
// "/", so that it matches whole path elements. Backend references to
// other namespaces need ReferenceGrants, which the emulator doesn't serve,
// so they are refused.

const gwGroup = "gateway.networking.k8s.io"

// allowedL7Ports are the ports of external Application Load Balancer
// forwarding rules.
var allowedL7Ports = map[int]bool{80: true, 8080: true, 443: true}

func gwCond(typ string, ok bool, reason, msg string, gen int64) gwCondition {
	st := "False"
	if ok {
		st = "True"
	}
	return gwCondition{Type: typ, Status: st, Reason: reason, Message: msg, ObservedGeneration: gen}
}

// gwPort collects what a Gateway serves on one port.
type gwPort struct {
	https bool
	certs []string
	// routes lists the routes served per hostname, each once.
	routes map[string][]*kubeHTTPRoute
}

func (p *gwPort) add(r *kubeHTTPRoute, hosts []string) {
	for _, h := range hosts {
		dup := false
		for _, o := range p.routes[h] {
			dup = dup || o == r
		}
		if !dup {
			p.routes[h] = append(p.routes[h], r)
		}
	}
}

func (t *translator) gateways() {
	gws := append([]kubeGateway(nil), t.in.Gateways...)
	sort.Slice(gws, func(i, j int) bool { return objKey(gws[i].Metadata) < objKey(gws[j].Metadata) })
	routes := append([]kubeHTTPRoute(nil), t.in.Routes...)
	sort.SliceStable(routes, func(i, j int) bool { return routeOlder(&routes[i], &routes[j]) })
	for i := range gws {
		if scheme, ok := gatewayClasses[gws[i].Spec.GatewayClassName]; ok && t.in.Classes[gws[i].Spec.GatewayClassName] {
			t.gateway(&gws[i], scheme, routes)
		}
	}
}

// routeOlder orders routes by creation, then namespace/name.
func routeOlder(a, b *kubeHTTPRoute) bool {
	if a.Metadata.CreationTimestamp != b.Metadata.CreationTimestamp {
		return a.Metadata.CreationTimestamp < b.Metadata.CreationTimestamp
	}
	return objKey(a.Metadata) < objKey(b.Metadata)
}

func (t *translator) gateway(gw *kubeGateway, scheme string, routes []kubeHTTPRoute) {
	ns, name, gen := gw.Metadata.Namespace, gw.Metadata.Name, gw.Metadata.Generation
	lb := &l7LB{Kind: "Gateway", Namespace: ns, Name: name, Generation: gen}
	t.plan.LBs = append(t.plan.LBs, lb)
	prefix := schemePrefix(scheme)
	ports := map[int]*gwPort{}
	valid := make([]bool, len(gw.Spec.Listeners))
	for i, l := range gw.Spec.Listeners {
		st := gwListenerStatus{Name: l.Name, SupportedKinds: []gwKind{{Group: gwGroup, Kind: "HTTPRoute"}}}
		conflict := false
		ok, reason, msg := true, "Accepted", "Listener accepted."
		refsOK, refsReason, refsMsg := true, "ResolvedRefs", "References resolved."
		https := l.Protocol == "HTTPS"
		switch {
		case l.Protocol != "HTTP" && !https:
			ok, reason, msg = false, "UnsupportedProtocol", "Only HTTP and HTTPS listeners are supported."
			st.SupportedKinds = []gwKind{}
		case !allowedL7Ports[l.Port]:
			ok, reason, msg = false, "PortUnavailable", "External Application Load Balancers serve ports 80, 8080 and 443 only."
		case ports[l.Port] != nil && ports[l.Port].https != https:
			ok, reason, msg = false, "PortUnavailable", "Another listener serves this port with another protocol."
			conflict = true
		case https && l.TLS != nil && l.TLS.Mode == "Passthrough":
			ok, reason, msg = false, "UnsupportedValue", "TLS passthrough is not supported."
		}
		var certs []string
		if ok && https {
			certs, refsOK, refsReason, refsMsg = t.listenerCerts(prefix, ns, l)
		}
		if ok && (!https || len(certs) > 0) {
			if ports[l.Port] == nil {
				ports[l.Port] = &gwPort{https: https, routes: map[string][]*kubeHTTPRoute{}}
			}
			ports[l.Port].certs = append(ports[l.Port].certs, certs...)
			valid[i] = true
			lb.Valid = true
		}
		st.Conditions = []gwCondition{
			gwCond("Accepted", ok, reason, msg, gen),
			gwCond("ResolvedRefs", refsOK, refsReason, refsMsg, gen),
			gwCond("Conflicted", false, "NoConflicts", "No conflicts.", gen),
		}
		if conflict {
			st.Conditions[2] = gwCond("Conflicted", true, "ProtocolConflict", msg, gen)
		}
		lb.Listeners = append(lb.Listeners, st)
	}
	for ri := range routes {
		t.attachRoute(gw, &routes[ri], valid, lb, ports)
	}
	nums := make([]int, 0, len(ports))
	for p := range ports {
		nums = append(nums, p)
	}
	sort.Ints(nums)
	ip := ""
	if len(gw.Spec.Addresses) > 0 {
		switch a := gw.Spec.Addresses[0]; a.Type {
		case "NamedAddress":
			ip = "global/addresses/" + a.Value
		case "", "IPAddress":
			ip = a.Value
		}
	}
	def := t.defaultBackendService(scheme)
	for _, n := range nums {
		gp, p := ports[n], strconv.Itoa(n)
		um := t.put(l7Resource{Path: t.global("urlMaps", t.name(prefix, "um", ns, name, p)), Body: t.gatewayURLMap(scheme, gp, def)})
		var proxy string
		if gp.https {
			proxy = t.put(l7Resource{Path: t.global("targetHttpsProxies", t.name(prefix, "ts", ns, name, p)),
				Body: &computev1.TargetHttpsProxy{UrlMap: um, SslCertificates: gp.certs}})
		} else {
			proxy = t.put(l7Resource{Path: t.global("targetHttpProxies", t.name(prefix, "tp", ns, name, p)),
				Body: &computev1.TargetHttpProxy{UrlMap: um}})
		}
		t.forwardingRule(lb, t.global("forwardingRules", t.name(prefix, "fr", ns, name, p)), scheme, p, proxy, ip)
	}
}

// listenerCerts returns the SSL certificates of an HTTPS listener: its
// Kubernetes Secrets' and the pre-shared ones its networking.gke.io/pre-shared-certs
// option names.
func (t *translator) listenerCerts(prefix, ns string, l gwListener) (certs []string, ok bool, reason, msg string) {
	ok, reason, msg = true, "ResolvedRefs", "References resolved."
	if l.TLS == nil {
		return nil, false, "InvalidCertificateRef", "An HTTPS listener needs certificates."
	}
	for _, ref := range l.TLS.CertificateRefs {
		refNS := ns
		if ref.Namespace != "" {
			refNS = ref.Namespace
		}
		switch {
		case ref.Group != "" || ref.Kind != "" && ref.Kind != "Secret":
			ok, reason, msg = false, "InvalidCertificateRef", "Only Secrets are supported as certificates."
		case refNS != ns:
			ok, reason, msg = false, "RefNotPermitted", "Certificates in other namespaces are not permitted."
		default:
			if c := t.secretCert(prefix, ns, ref.Name); c != "" {
				certs = append(certs, c)
			} else {
				ok, reason, msg = false, "InvalidCertificateRef", "Secret "+ref.Name+" is missing or not a TLS Secret."
			}
		}
	}
	certs = append(certs, preSharedCerts(l.TLS.Options["networking.gke.io/pre-shared-certs"])...)
	if len(certs) == 0 && ok {
		ok, reason, msg = false, "InvalidCertificateRef", "An HTTPS listener needs certificates."
	}
	return certs, ok, reason, msg
}

// attachRoute attaches a route to the Gateway's listeners its parent
// references select and records its status for them.
func (t *translator) attachRoute(gw *kubeGateway, r *kubeHTTPRoute, valid []bool, lb *l7LB, ports map[int]*gwPort) {
	rns := r.Metadata.Namespace
	for _, pr := range r.Spec.ParentRefs {
		if !refersTo(pr, rns, gw) {
			continue
		}
		accepted, reason, msg := false, "NoMatchingParent", "No listener matches the parent reference."
		for i, l := range gw.Spec.Listeners {
			if pr.SectionName != nil && *pr.SectionName != l.Name || pr.Port != nil && *pr.Port != l.Port || !valid[i] {
				continue
			}
			if !routeAllowed(l, gw.Metadata.Namespace, rns) {
				if !accepted {
					reason, msg = "NotAllowedByListeners", "The listeners don't allow routes from this namespace."
				}
				continue
			}
			hosts := intersectHosts(l.Hostname, r.Spec.Hostnames)
			if len(hosts) == 0 {
				if !accepted {
					reason, msg = "NoMatchingListenerHostname", "No listener hostname matches the route's hostnames."
				}
				continue
			}
			accepted, reason, msg = true, "Accepted", "Route accepted."
			lb.Listeners[i].AttachedRoutes++
			ports[l.Port].add(r, hosts)
		}
		refsOK, refsReason, refsMsg := t.routeRefs(r)
		gen := r.Metadata.Generation
		key := objKey(r.Metadata)
		t.plan.RouteParents[key] = append(t.plan.RouteParents[key], gwParentStatus{
			ParentRef: pr, ControllerName: gwController,
			Conditions: []gwCondition{gwCond("Accepted", accepted, reason, msg, gen), gwCond("ResolvedRefs", refsOK, refsReason, refsMsg, gen)},
		})
	}
}

// refersTo reports whether a parent reference of a route in rns names gw.
func refersTo(pr gwParentRef, rns string, gw *kubeGateway) bool {
	if pr.Group != nil && *pr.Group != gwGroup || pr.Kind != nil && *pr.Kind != "Gateway" {
		return false
	}
	ns := rns
	if pr.Namespace != nil {
		ns = *pr.Namespace
	}
	return ns == gw.Metadata.Namespace && pr.Name == gw.Metadata.Name
}

// routeAllowed applies a listener's allowedRoutes.namespaces; a label
// Selector allows every namespace.
func routeAllowed(l gwListener, gwNS, routeNS string) bool {
	from := "Same"
	if l.AllowedRoutes != nil && l.AllowedRoutes.Namespaces != nil && l.AllowedRoutes.Namespaces.From != "" {
		from = l.AllowedRoutes.Namespaces.From
	}
	return from != "Same" || gwNS == routeNS
}

// intersectHosts returns the hostnames a route serves on a listener: the
// more specific of each matching pair, "*" when neither restricts them.
func intersectHosts(listener string, route []string) []string {
	if len(route) == 0 {
		if listener == "" {
			return []string{"*"}
		}
		return []string{listener}
	}
	if listener == "" {
		return route
	}
	var out []string
	seen := map[string]bool{}
	for _, h := range route {
		m := ""
		switch {
		case h == listener || hostCovers(listener, h):
			m = h
		case hostCovers(h, listener):
			m = listener
		}
		if m != "" && !seen[m] {
			seen[m] = true
			out = append(out, m)
		}
	}
	return out
}

// hostCovers reports whether the wildcard pattern "*.suffix" matches host
// (one or more labels).
func hostCovers(pattern, host string) bool {
	suffix, ok := strings.CutPrefix(pattern, "*")
	return ok && len(host) > len(suffix) && strings.HasSuffix(host, suffix)
}

// gwBackend is a resolved backend reference.
type gwBackend struct {
	service string
	weight  int64
}

// routeBackend returns the backend service of a resolvable backend
// reference of a route in ns, or "".
func (t *translator) routeBackend(scheme, ns string, b gwBackendRef) string {
	if backendRefProblem(ns, b) != "" {
		return ""
	}
	return t.backendService(scheme, ns, b.Name, *b.Port, "")
}

// routeRefs reports whether every backend reference of a route resolves.
func (t *translator) routeRefs(r *kubeHTTPRoute) (bool, string, string) {
	ns := r.Metadata.Namespace
	for _, rule := range r.Spec.Rules {
		for _, b := range rule.BackendRefs {
			why := backendRefProblem(ns, b)
			if why == "" {
				if _, _, ok := t.servicePort(ns, b.Name, *b.Port, ""); !ok {
					why = "BackendNotFound"
				}
			}
			if why != "" {
				return false, why, "Backend " + b.Name + ": " + why + "."
			}
		}
	}
	return true, "ResolvedRefs", "References resolved."
}

// backendRefProblem returns the ResolvedRefs reason a backend reference
// of a route in ns can't be served for, short of a missing Kubernetes
// Service, or "".
func backendRefProblem(ns string, b gwBackendRef) string {
	switch {
	case b.Group != nil && *b.Group != "" || b.Kind != nil && *b.Kind != "Service":
		return "InvalidKind"
	case b.Namespace != nil && *b.Namespace != ns:
		return "RefNotPermitted"
	case b.Port == nil:
		return "UnsupportedValue"
	}
	return ""
}

// gwMatch is one match of a route rule.
type gwMatch struct {
	route *kubeHTTPRoute
	rule  int
	index int
	m     gwRouteMatch
}

// matchPath returns a match's path type and value, PathPrefix "/" by
// default.
func matchPath(m gwRouteMatch) (typ, v string) {
	typ, v = "PathPrefix", "/"
	if m.Path != nil {
		if m.Path.Type != "" {
			typ = m.Path.Type
		}
		if m.Path.Value != "" {
			v = m.Path.Value
		}
	}
	return typ, v
}

// pathKindRank orders path match types by precedence.
func pathKindRank(m gwRouteMatch) (int, int) {
	typ, v := matchPath(m)
	switch typ {
	case "Exact":
		return 0, len(v)
	case "PathPrefix":
		return 1, len(strings.TrimRight(v, "/"))
	}
	return 2, len(v)
}

// matchBefore orders matches by Gateway API precedence.
func matchBefore(a, b gwMatch) bool {
	ka, la := pathKindRank(a.m)
	kb, lb := pathKindRank(b.m)
	if ka != kb {
		return ka < kb
	}
	if la != lb {
		return la > lb
	}
	if (a.m.Method != "") != (b.m.Method != "") {
		return a.m.Method != ""
	}
	if len(a.m.Headers) != len(b.m.Headers) {
		return len(a.m.Headers) > len(b.m.Headers)
	}
	if len(a.m.QueryParams) != len(b.m.QueryParams) {
		return len(a.m.QueryParams) > len(b.m.QueryParams)
	}
	if a.route != b.route {
		return routeOlder(a.route, b.route)
	}
	if a.rule != b.rule {
		return a.rule < b.rule
	}
	return a.index < b.index
}

// gatewayURLMap builds the URL map of one port.
func (t *translator) gatewayURLMap(scheme string, gp *gwPort, def string) *computev1.UrlMap {
	um := &computev1.UrlMap{DefaultService: def}
	hosts := make([]string, 0, len(gp.routes))
	for h := range gp.routes {
		hosts = append(hosts, h)
	}
	sort.Strings(hosts)
	for i, h := range hosts {
		var ms []gwMatch
		for _, r := range gp.routes[h] {
			for ri, rule := range r.Spec.Rules {
				matches := rule.Matches
				if len(matches) == 0 {
					matches = []gwRouteMatch{{}}
				}
				for mi, m := range matches {
					ms = append(ms, gwMatch{route: r, rule: ri, index: mi, m: m})
				}
			}
		}
		sort.SliceStable(ms, func(a, b int) bool { return matchBefore(ms[a], ms[b]) })
		pm := &computev1.PathMatcher{Name: "host" + strconv.Itoa(i), DefaultService: def}
		for _, m := range ms {
			for _, v := range pathVariants(m.m) {
				rr := t.routeRule(scheme, m, v, def)
				rr.Priority = int64(len(pm.RouteRules))
				pm.RouteRules = append(pm.RouteRules, rr)
			}
		}
		um.PathMatchers = append(um.PathMatchers, pm)
		um.HostRules = append(um.HostRules, &computev1.HostRule{Hosts: []string{h}, PathMatcher: pm.Name})
	}
	return um
}

// pathVariant is one URL map path match of a route match.
type pathVariant struct {
	full, prefix, regex string
	// base is a PathPrefix match's prefix without a trailing "/".
	base string
}

// pathVariants expands a route match's path: PathPrefix "/a" is the full
// path "/a" and the prefix "/a/".
func pathVariants(m gwRouteMatch) []pathVariant {
	typ, v := matchPath(m)
	switch typ {
	case "Exact":
		return []pathVariant{{full: v}}
	case "RegularExpression":
		return []pathVariant{{regex: v}}
	}
	base := strings.TrimRight(v, "/")
	if base == "" {
		return []pathVariant{{prefix: "/"}}
	}
	return []pathVariant{{full: base, base: base}, {prefix: base + "/", base: base}}
}

// routeRule translates one path variant of a route match.
func (t *translator) routeRule(scheme string, m gwMatch, v pathVariant, def string) *computev1.HttpRouteRule {
	mr := &computev1.HttpRouteRuleMatch{FullPathMatch: v.full, PrefixMatch: v.prefix, RegexMatch: v.regex}
	for _, h := range m.m.Headers {
		hm := &computev1.HttpHeaderMatch{HeaderName: h.Name}
		if h.Type == "RegularExpression" {
			hm.RegexMatch = h.Value
		} else {
			hm.ExactMatch = h.Value
		}
		mr.HeaderMatches = append(mr.HeaderMatches, hm)
	}
	if m.m.Method != "" {
		mr.HeaderMatches = append(mr.HeaderMatches, &computev1.HttpHeaderMatch{HeaderName: ":method", ExactMatch: m.m.Method})
	}
	for _, q := range m.m.QueryParams {
		qm := &computev1.HttpQueryParameterMatch{Name: q.Name}
		if q.Type == "RegularExpression" {
			qm.RegexMatch = q.Value
		} else {
			qm.ExactMatch = q.Value
		}
		mr.QueryParameterMatches = append(mr.QueryParameterMatches, qm)
	}
	rr := &computev1.HttpRouteRule{MatchRules: []*computev1.HttpRouteRuleMatch{mr}}
	rule := m.route.Spec.Rules[m.rule]
	var rewrite *computev1.UrlRewrite
	for _, f := range rule.Filters {
		switch {
		case f.Type == "RequestHeaderModifier" && f.RequestHeaderModifier != nil:
			ha := headerAction(rr)
			ha.RequestHeadersToAdd = append(ha.RequestHeadersToAdd, headerOptions(f.RequestHeaderModifier)...)
			ha.RequestHeadersToRemove = append(ha.RequestHeadersToRemove, f.RequestHeaderModifier.Remove...)
		case f.Type == "ResponseHeaderModifier" && f.ResponseHeaderModifier != nil:
			ha := headerAction(rr)
			ha.ResponseHeadersToAdd = append(ha.ResponseHeadersToAdd, headerOptions(f.ResponseHeaderModifier)...)
			ha.ResponseHeadersToRemove = append(ha.ResponseHeadersToRemove, f.ResponseHeaderModifier.Remove...)
		case f.Type == "RequestRedirect" && f.RequestRedirect != nil:
			rd := f.RequestRedirect
			ur := &computev1.HttpRedirectAction{RedirectResponseCode: "FOUND"}
			if rd.Scheme != nil && *rd.Scheme == "https" {
				ur.HttpsRedirect = true
			}
			if rd.Hostname != nil {
				ur.HostRedirect = *rd.Hostname
			}
			if rd.StatusCode != nil {
				ur.RedirectResponseCode = map[int]string{301: "MOVED_PERMANENTLY_DEFAULT", 302: "FOUND", 303: "SEE_OTHER", 307: "TEMPORARY_REDIRECT", 308: "PERMANENT_REDIRECT"}[*rd.StatusCode]
			}
			if pm := rd.Path; pm != nil {
				switch pm.Type {
				case "ReplaceFullPath":
					ur.PathRedirect = pm.ReplaceFullPath
				case "ReplacePrefixMatch":
					ur.PrefixRedirect = prefixReplacement(v, pm.ReplacePrefixMatch)
				}
			}
			rr.UrlRedirect = ur
		case f.Type == "URLRewrite" && f.URLRewrite != nil:
			rewrite = &computev1.UrlRewrite{}
			if f.URLRewrite.Hostname != nil {
				rewrite.HostRewrite = *f.URLRewrite.Hostname
			}
			if pm := f.URLRewrite.Path; pm != nil && v.regex == "" {
				switch {
				case pm.Type == "ReplacePrefixMatch":
					rewrite.PathPrefixRewrite = prefixReplacement(v, pm.ReplacePrefixMatch)
				case pm.Type == "ReplaceFullPath" && v.full != "":
					rewrite.PathPrefixRewrite = pm.ReplaceFullPath
				case pm.Type == "ReplaceFullPath":
					// Replace the whole of any path under the prefix.
					mr.PrefixMatch, mr.PathTemplateMatch = "", v.base+"/**"
					rewrite.PathTemplateRewrite = pm.ReplaceFullPath
				}
			}
		}
	}
	if rr.UrlRedirect != nil {
		return rr
	}
	var bes []gwBackend
	for _, b := range rule.BackendRefs {
		w := int64(1)
		if b.Weight != nil {
			w = int64(*b.Weight)
		}
		if w <= 0 {
			continue
		}
		if be := t.routeBackend(scheme, m.route.Metadata.Namespace, b); be != "" {
			bes = append(bes, gwBackend{be, min(w, 1000)})
		}
	}
	switch {
	case len(bes) == 0:
		rr.Service = def
	case len(bes) == 1:
		rr.Service = bes[0].service
	default:
		rr.RouteAction = &computev1.HttpRouteAction{}
		for _, b := range bes {
			rr.RouteAction.WeightedBackendServices = append(rr.RouteAction.WeightedBackendServices,
				&computev1.WeightedBackendService{BackendService: b.service, Weight: b.weight})
		}
	}
	if rewrite != nil {
		if rr.RouteAction == nil {
			rr.RouteAction = &computev1.HttpRouteAction{}
		}
		rr.RouteAction.UrlRewrite = rewrite
	}
	return rr
}

func headerAction(rr *computev1.HttpRouteRule) *computev1.HttpHeaderAction {
	if rr.HeaderAction == nil {
		rr.HeaderAction = &computev1.HttpHeaderAction{}
	}
	return rr.HeaderAction
}

// headerOptions translates a header modifier's set (replacing) and add.
func headerOptions(m *gwHeaderModifier) []*computev1.HttpHeaderOption {
	var out []*computev1.HttpHeaderOption
	for _, h := range m.Set {
		out = append(out, &computev1.HttpHeaderOption{HeaderName: h.Name, HeaderValue: h.Value, Replace: true})
	}
	for _, h := range m.Add {
		out = append(out, &computev1.HttpHeaderOption{HeaderName: h.Name, HeaderValue: h.Value})
	}
	return out
}

// prefixReplacement is the URL map prefix replacing a path variant's
// matched prefix with repl: the full path for the bare prefix, repl plus
// "/" for the prefix plus "/".
func prefixReplacement(v pathVariant, repl string) string {
	if v.full != "" {
		if repl == "" {
			return "/"
		}
		return repl
	}
	return strings.TrimRight(repl, "/") + "/"
}
