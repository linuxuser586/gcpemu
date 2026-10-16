package gke

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"cloud.google.com/go/container/apiv1/containerpb"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// fakeKube is an in-memory Kubernetes API: list, get, create, replace,
// delete and JSON merge patch of the objects stored, by API path.
type fakeKube struct {
	t          *testing.T
	mu         sync.Mutex
	objs       map[string]map[string]any // object path → object
	gatewayAPI bool
	patches    []string
	// establish marks the CRDs created or replaced Established.
	establish bool
	// admit, when set, sees each create and replace (with the object
	// stored at path, if any) and denies it with a non-zero status.
	admit func(method, path string, old, obj map[string]any) (int, string)
}

func newFakeKube(t *testing.T) (*fakeKube, *kubeClient) {
	k := &fakeKube{t: t, objs: map[string]map[string]any{}, gatewayAPI: true}
	srv := httptest.NewServer(k)
	t.Cleanup(srv.Close)
	return k, &kubeClient{base: srv.URL, hc: srv.Client()}
}

// splitKubePath splits an API path into its base ("/api/v1" or "/apis/G/V"),
// namespace, resource, name and subresource.
func splitKubePath(p string) (base, ns, res, name, sub string) {
	segs := strings.Split(strings.Trim(p, "/"), "/")
	n := 2
	if segs[0] == "apis" {
		n = 3
	}
	base, segs = "/"+strings.Join(segs[:n], "/"), segs[n:]
	if len(segs) >= 2 && segs[0] == "namespaces" && len(segs) > 2 {
		ns, segs = segs[1], segs[2:]
	}
	res = segs[0]
	if len(segs) > 1 {
		name = segs[1]
	}
	if len(segs) > 2 {
		sub = segs[2]
	}
	return
}

func objPath(base, ns, res, name string) string {
	if ns == "" {
		return base + "/" + res + "/" + name
	}
	return base + "/namespaces/" + ns + "/" + res + "/" + name
}

// put stores a JSON object at its API path, derived from apiVersion and kind.
func (k *fakeKube) put(obj string) {
	k.t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(obj), &m); err != nil {
		k.t.Fatalf("object %s: %v", obj, err)
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	k.objs[k.pathOf(m)] = m
}

func (k *fakeKube) pathOf(m map[string]any) string {
	res := map[string]string{"Service": "services", "Ingress": "ingresses", "Gateway": "gateways", "HTTPRoute": "httproutes",
		"GatewayClass": "gatewayclasses", "Secret": "secrets", "CustomResourceDefinition": "customresourcedefinitions",
		"ValidatingAdmissionPolicy": "validatingadmissionpolicies", "ValidatingAdmissionPolicyBinding": "validatingadmissionpolicybindings"}[m["kind"].(string)]
	av, _ := m["apiVersion"].(string)
	base := "/apis/" + av
	if av == "v1" || av == "" {
		base = "/api/v1"
	}
	meta := m["metadata"].(map[string]any)
	ns, _ := meta["namespace"].(string)
	return objPath(base, ns, res, meta["name"].(string))
}

func (k *fakeKube) remove(path string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	delete(k.objs, path)
}

func (k *fakeKube) get(path string) map[string]any {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.objs[path]
}

// field digs a value out of an object ("status.loadBalancer.ingress").
func field(m map[string]any, path string) any {
	var v any = m
	for _, f := range strings.Split(path, ".") {
		mm, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = mm[f]
	}
	return v
}

func (k *fakeKube) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	k.mu.Lock()
	defer k.mu.Unlock()
	base, ns, res, name, sub := splitKubePath(r.URL.Path)
	if strings.HasPrefix(base, "/apis/gateway.networking.k8s.io") && !k.gatewayAPI || res == "endpointslices" && r.Method == http.MethodGet && name == "" {
		if res == "endpointslices" {
			_, _ = io.WriteString(w, `{"items":[]}`)
			return
		}
		http.NotFound(w, r)
		return
	}
	switch {
	case r.Method == http.MethodGet && name == "":
		var keys []string
		for p := range k.objs {
			b, ons, ores, _, _ := splitKubePath(p)
			if b == base && ores == res && (ns == "" || ns == ons) {
				keys = append(keys, p)
			}
		}
		sort.Strings(keys)
		items := []any{}
		// Label selectors of the form key!=value only.
		lk, lv, _ := strings.Cut(r.URL.Query().Get("labelSelector"), "!=")
		for _, p := range keys {
			if lk != "" {
				if l, _ := field(k.objs[p], "metadata.labels").(map[string]any); l[lk] == lv {
					continue
				}
			}
			items = append(items, k.objs[p])
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"items": items})
	case r.Method == http.MethodGet:
		o := k.objs[objPath(base, ns, res, name)]
		if o == nil {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(o)
	case r.Method == http.MethodPost && name == "":
		var m map[string]any
		_ = json.NewDecoder(r.Body).Decode(&m)
		meta := m["metadata"].(map[string]any)
		p := objPath(base, ns, res, meta["name"].(string))
		if k.objs[p] != nil {
			w.WriteHeader(http.StatusConflict)
			return
		}
		if k.denied(w, r.Method, p, nil, m) {
			return
		}
		meta["generation"] = 1
		k.store(p, m)
		_ = json.NewEncoder(w).Encode(m)
	case r.Method == http.MethodPut:
		var m map[string]any
		_ = json.NewDecoder(r.Body).Decode(&m)
		p := objPath(base, ns, res, name)
		old := k.objs[p]
		if old == nil {
			http.NotFound(w, r)
			return
		}
		if k.denied(w, r.Method, p, old, m) {
			return
		}
		k.store(p, m)
		_ = json.NewEncoder(w).Encode(m)
	case r.Method == http.MethodDelete:
		p := objPath(base, ns, res, name)
		if k.objs[p] == nil {
			http.NotFound(w, r)
			return
		}
		delete(k.objs, p)
		_, _ = io.WriteString(w, `{}`)
	case r.Method == http.MethodPatch:
		p := objPath(base, ns, res, name)
		o := k.objs[p]
		if o == nil {
			http.NotFound(w, r)
			return
		}
		var patch map[string]any
		_ = json.NewDecoder(r.Body).Decode(&patch)
		if sub == "status" {
			patch = map[string]any{"status": patch["status"]}
		} else {
			delete(patch, "status")
		}
		mergeJSON(o, patch)
		k.patches = append(k.patches, r.URL.Path)
		_ = json.NewEncoder(w).Encode(o)
	default:
		k.t.Errorf("unexpected kube request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// denied answers a create or replace that admit denies.
func (k *fakeKube) denied(w http.ResponseWriter, method, path string, old, obj map[string]any) bool {
	if k.admit == nil {
		return false
	}
	code, msg := k.admit(method, path, old, obj)
	if code == 0 {
		return false
	}
	w.WriteHeader(code)
	_, _ = io.WriteString(w, msg)
	return true
}

// store saves obj at path, Established when it is a CRD and k.establish.
func (k *fakeKube) store(path string, obj map[string]any) {
	if k.establish && obj["kind"] == "CustomResourceDefinition" {
		obj["status"] = map[string]any{"conditions": []any{map[string]any{"type": "Established", "status": "True"}}}
	}
	k.objs[path] = obj
}

// mergeJSON applies a JSON merge patch (RFC 7386) to dst.
func mergeJSON(dst, patch map[string]any) {
	for k, v := range patch {
		pm, ok := v.(map[string]any)
		switch {
		case v == nil:
			delete(dst, k)
		case ok:
			dm, _ := dst[k].(map[string]any)
			if dm == nil {
				dm = map[string]any{}
				dst[k] = dm
			}
			mergeJSON(dm, pm)
		default:
			dst[k] = v
		}
	}
}

func (k *fakeKube) takePatches() []string {
	k.mu.Lock()
	defer k.mu.Unlock()
	p := k.patches
	k.patches = nil
	return p
}

// fakeLB is an in-memory emu.LoadBalancerResources; forwarding rules get
// an address when they have none.
type fakeLB struct {
	mu   sync.Mutex
	res  map[string]map[string]any
	puts int
	next int
}

func (f *fakeLB) PutLBResource(ctx context.Context, path string, body []byte) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, err
	}
	if strings.Contains(path, "/forwardingRules/") {
		ip, _ := m["IPAddress"].(string)
		switch {
		case strings.HasPrefix(ip, "global/addresses/"):
			m["IPAddress"] = "34.0.0.99"
		case ip == "" && f.res[path] != nil:
			m["IPAddress"] = f.res[path]["IPAddress"]
		case ip == "":
			f.next++
			m["IPAddress"] = "34.120.0." + strconv.Itoa(f.next)
		}
	}
	m["name"] = path[strings.LastIndex(path, "/")+1:]
	f.res[path] = m
	f.puts++
	return json.Marshal(m)
}

func (f *fakeLB) LBResource(ctx context.Context, path string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if m := f.res[path]; m != nil {
		return json.Marshal(m)
	}
	return nil, apierr.NotFound("The resource '%s' was not found", path)
}

func (f *fakeLB) DeleteLBResource(ctx context.Context, path string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.res, path)
	return nil
}

func (f *fakeLB) paths(coll string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for p := range f.res {
		if strings.Contains(p, "/"+coll+"/") {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

// fakeVPC records NEGs.
type fakeVPC struct {
	emu.VPC
	mu   sync.Mutex
	negs map[string][]emu.NEGEndpoint // zone/name
}

func (v *fakeVPC) UpsertNEG(ctx context.Context, project, zone, name, network, subnetwork string, defaultPort int, description string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if _, ok := v.negs[zone+"/"+name]; !ok {
		v.negs[zone+"/"+name] = nil
	}
	return nil
}

func (v *fakeVPC) SetNEGEndpoints(ctx context.Context, project, zone, name string, eps []emu.NEGEndpoint) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.negs[zone+"/"+name] = eps
	return nil
}

func (v *fakeVPC) DeleteNEG(ctx context.Context, project, zone, name string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	delete(v.negs, zone+"/"+name)
	return nil
}

func (v *fakeVPC) has(zone, name string) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	_, ok := v.negs[zone+"/"+name]
	return ok
}

// The controller turns a cluster's Ingresses and Gateways into load
// balancer resources and NEGs, reports their addresses and status (and
// installs the gke-l7 GatewayClasses), changes nothing while nothing
// changes, removes what goes away, and everything with the cluster.
func TestL7Controller(t *testing.T) {
	s := newUnitService(t)
	lb := &fakeLB{res: map[string]map[string]any{}}
	vpc := &fakeVPC{negs: map[string][]emu.NEGEndpoint{}}
	s.testLB, s.testVPC = lb, vpc
	rec := &clusterRecord{Int: clusterInternal{Project: "p", Location: "us-central1-a", Name: "c1",
		Nodes: []nodeRecord{{Name: "n1", Zone: "us-central1-a"}}}}
	rec.setCluster(&containerpb.Cluster{Name: "c1", Id: "0123456789abcdef", Locations: []string{"us-central1-a"}})
	if err := s.env.Store.Update(func(tx store.Tx) error { return putCluster(tx, rec) }); err != nil {
		t.Fatal(err)
	}
	key := rec.key()
	kube, kc := newFakeKube(t)
	s.rtFor(key).kube = kc
	ctx := context.Background()

	const gw = `"apiVersion":"gateway.networking.k8s.io/v1"`
	kube.put(`{"apiVersion":"v1","kind":"Service","metadata":{"namespace":"default","name":"web"},"spec":{"ports":[{"port":80}]}}`)
	kube.put(`{"apiVersion":"networking.k8s.io/v1","kind":"Ingress","metadata":{"namespace":"default","name":"web"},
		"spec":{"defaultBackend":{"service":{"name":"web","port":{"number":80}}}}}`)
	kube.put(`{` + gw + `,"kind":"Gateway","metadata":{"namespace":"default","name":"ext","generation":1},"spec":{
		"gatewayClassName":"gke-l7-global-external-managed","listeners":[{"name":"http","port":80,"protocol":"HTTP"}]}}`)
	kube.put(`{` + gw + `,"kind":"HTTPRoute","metadata":{"namespace":"default","name":"web","generation":1},"spec":{
		"parentRefs":[{"name":"ext"}],"rules":[{"backendRefs":[{"name":"web","port":80}]}]},
		"status":{"parents":[{"parentRef":{"name":"mesh"},"controllerName":"example.com/mesh","conditions":[]}]}}`)

	if err := s.syncLoadBalancers(ctx, key); err != nil {
		t.Fatal(err)
	}
	frs := lb.paths("forwardingRules")
	if len(frs) != 2 || len(lb.paths("targetHttpProxies")) != 2 || len(lb.paths("urlMaps")) != 2 {
		t.Fatalf("load balancer resources: forwarding rules %v, proxies %v, URL maps %v", frs, lb.paths("targetHttpProxies"), lb.paths("urlMaps"))
	}
	neg := negName("0123456789abcdef", "default", "web", 80)
	if !vpc.has("us-central1-a", neg) {
		t.Errorf("NEGs %v, want %s", vpc.negs, neg)
	}
	if st, _ := field(kube.get("/api/v1/namespaces/default/services/web"), "metadata.annotations").(map[string]any)[annoNEGStatus].(string); !strings.Contains(st, neg) {
		t.Errorf("Service neg-status %q, want %s", st, neg)
	}

	ing := kube.get("/apis/networking.k8s.io/v1/namespaces/default/ingresses/web")
	ingIP, _ := field(ing, "status.loadBalancer.ingress").([]any)
	if len(ingIP) != 1 || !strings.HasPrefix(ingIP[0].(map[string]any)["ip"].(string), "34.120.0.") {
		t.Errorf("Ingress status %v, want its forwarding rule's address", field(ing, "status"))
	}
	if a, _ := field(ing, "metadata.annotations").(map[string]any); a["ingress.kubernetes.io/forwarding-rule"] == nil || a["ingress.kubernetes.io/url-map"] == nil {
		t.Errorf("Ingress annotations %v", a)
	}

	g := kube.get("/apis/gateway.networking.k8s.io/v1/namespaces/default/gateways/ext")
	var gst gwStatus
	b, _ := json.Marshal(field(g, "status"))
	_ = json.Unmarshal(b, &gst)
	if len(gst.Addresses) != 1 || !strings.HasPrefix(gst.Addresses[0].Value, "34.120.0.") || gst.Addresses[0].Type != "IPAddress" ||
		cond(gst.Conditions, "Accepted").Status != "True" || cond(gst.Conditions, "Programmed").Status != "True" ||
		len(gst.Listeners) != 1 || gst.Listeners[0].AttachedRoutes != 1 || cond(gst.Listeners[0].Conditions, "Programmed").Status != "True" ||
		cond(gst.Conditions, "Programmed").LastTransitionTime == "" {
		t.Errorf("Gateway status %s", b)
	}
	var rst struct {
		Parents []gwParentStatus `json:"parents"`
	}
	b, _ = json.Marshal(field(kube.get("/apis/gateway.networking.k8s.io/v1/namespaces/default/httproutes/web"), "status"))
	_ = json.Unmarshal(b, &rst)
	if len(rst.Parents) != 2 || rst.Parents[0].ControllerName != "example.com/mesh" || rst.Parents[1].ControllerName != gwController ||
		cond(rst.Parents[1].Conditions, "Accepted").Status != "True" {
		t.Errorf("HTTPRoute status %s, want the mesh's parent kept and ours added", b)
	}
	for name := range gatewayClasses {
		gc := kube.get("/apis/gateway.networking.k8s.io/v1/gatewayclasses/" + name)
		if field(gc, "spec.controllerName") != gwController {
			t.Errorf("GatewayClass %s: %v", name, gc)
		}
		if l, _ := field(gc, "metadata.labels").(map[string]any); l[addonLabel] != "gateway-api" {
			t.Errorf("GatewayClass %s labels %v, want the gateway-api add-on's", name, l)
		}
		var cs []gwCondition
		b, _ := json.Marshal(field(gc, "status.conditions"))
		_ = json.Unmarshal(b, &cs)
		if cond(cs, "Accepted").Status != "True" {
			t.Errorf("GatewayClass %s conditions %s", name, b)
		}
	}

	// Nothing changes while nothing changes.
	kube.takePatches()
	puts := lb.puts
	if err := s.syncLoadBalancers(ctx, key); err != nil {
		t.Fatal(err)
	}
	if lb.puts != puts {
		t.Errorf("second sync wrote %d load balancer resources, want none", lb.puts-puts)
	}
	if p := kube.takePatches(); len(p) != 0 {
		t.Errorf("second sync patched %v, want nothing", p)
	}

	// What goes away is removed.
	kube.remove("/apis/networking.k8s.io/v1/namespaces/default/ingresses/web")
	if err := s.syncLoadBalancers(ctx, key); err != nil {
		t.Fatal(err)
	}
	if frs := lb.paths("forwardingRules"); len(frs) != 1 || !strings.Contains(frs[0], "gkegw1") {
		t.Errorf("after the Ingress went: forwarding rules %v, want the Gateway's", frs)
	}
	kube.remove("/apis/gateway.networking.k8s.io/v1/namespaces/default/gateways/ext")
	if err := s.syncLoadBalancers(ctx, key); err != nil {
		t.Fatal(err)
	}
	b, _ = json.Marshal(field(kube.get("/apis/gateway.networking.k8s.io/v1/namespaces/default/httproutes/web"), "status"))
	if strings.Contains(string(b), gwController) {
		t.Errorf("HTTPRoute status %s still has the removed Gateway's parent", b)
	}
	if len(lb.res) != 0 || vpc.has("us-central1-a", neg) {
		t.Errorf("after everything went: load balancer resources %v, NEG kept %v", lb.paths(""), vpc.has("us-central1-a", neg))
	}

	// Deleting the cluster removes its load balancers.
	kube.put(`{"apiVersion":"networking.k8s.io/v1","kind":"Ingress","metadata":{"namespace":"default","name":"web"},
		"spec":{"defaultBackend":{"service":{"name":"web","port":{"number":80}}}}}`)
	if err := s.syncLoadBalancers(ctx, key); err != nil || len(lb.res) == 0 {
		t.Fatalf("sync: %v, %d resources", err, len(lb.res))
	}
	s.deleteL7(ctx, key)
	if len(lb.res) != 0 {
		t.Errorf("after the cluster went: load balancer resources %v", lb.paths(""))
	}

	// Without the Gateway API CRDs only Ingresses are served.
	kube.gatewayAPI = false
	if err := s.syncLoadBalancers(ctx, key); err != nil {
		t.Fatalf("sync without the Gateway API: %v", err)
	}
	if frs := lb.paths("forwardingRules"); len(frs) != 1 {
		t.Errorf("without the Gateway API: forwarding rules %v, want the Ingress's", frs)
	}
}

// Disabling the Gateway API removes its CRDs although the add-on's own
// GatewayClasses exist, and keeps them while a user's object does.
func TestGatewayCRDsRemovedWithClasses(t *testing.T) {
	s := newUnitService(t)
	kube, kc := newFakeKube(t)
	ctx := context.Background()
	crd := func(plural string) string {
		return `{"apiVersion":"apiextensions.k8s.io/v1","kind":"CustomResourceDefinition","metadata":{"name":"` + plural +
			`.gateway.networking.k8s.io","labels":{"gcpemu.dev/addon":"gateway-api"}},"spec":{"group":"gateway.networking.k8s.io",
			"names":{"plural":"` + plural + `"},"versions":[{"name":"v1","served":true}]}}`
	}
	var objs []map[string]any
	for _, c := range []string{crd("gatewayclasses"), crd("gateways")} {
		kube.put(c)
		var m map[string]any
		_ = json.Unmarshal([]byte(c), &m)
		objs = append(objs, m)
	}
	if err := s.ensureGatewayClasses(ctx, kc); err != nil {
		t.Fatal(err)
	}
	user := `{"apiVersion":"gateway.networking.k8s.io/v1","kind":"Gateway","metadata":{"namespace":"default","name":"mine"},"spec":{}}`
	kube.put(user)
	if err := s.removeGatewayCRDs(ctx, kc, objs); err != nil {
		t.Fatal(err)
	}
	if kube.get("/apis/apiextensions.k8s.io/v1/customresourcedefinitions/gateways.gateway.networking.k8s.io") == nil {
		t.Fatal("CRDs removed while a user's Gateway exists")
	}
	kube.remove("/apis/gateway.networking.k8s.io/v1/namespaces/default/gateways/mine")
	if err := s.removeGatewayCRDs(ctx, kc, objs); err != nil {
		t.Fatal(err)
	}
	if kube.get("/apis/apiextensions.k8s.io/v1/customresourcedefinitions/gatewayclasses.gateway.networking.k8s.io") != nil {
		t.Error("CRDs kept although only the add-on's GatewayClasses exist")
	}
}

// Status follows what is served: dropping TLS removes the HTTPS
// annotations, an Ingress without forwarding rules loses its address, and
// a Gateway with no servable listener is not accepted and has none.
func TestL7StatusCleared(t *testing.T) {
	s := newUnitService(t)
	lb := &fakeLB{res: map[string]map[string]any{}}
	s.testLB, s.testVPC = lb, &fakeVPC{negs: map[string][]emu.NEGEndpoint{}}
	rec := &clusterRecord{Int: clusterInternal{Project: "p", Location: "us-central1-a", Name: "c1"}}
	rec.setCluster(&containerpb.Cluster{Name: "c1", Id: "0123456789abcdef", Locations: []string{"us-central1-a"}})
	if err := s.env.Store.Update(func(tx store.Tx) error { return putCluster(tx, rec) }); err != nil {
		t.Fatal(err)
	}
	key := rec.key()
	kube, kc := newFakeKube(t)
	s.rtFor(key).kube = kc
	ctx := context.Background()
	sync := func() {
		t.Helper()
		if err := s.syncLoadBalancers(ctx, key); err != nil {
			t.Fatal(err)
		}
	}
	const ingPath = "/apis/networking.k8s.io/v1/namespaces/default/ingresses/web"
	const gwPath = "/apis/gateway.networking.k8s.io/v1/namespaces/default/gateways/ext"
	ingress := func(spec string, annos string) {
		kube.put(`{"apiVersion":"networking.k8s.io/v1","kind":"Ingress","metadata":{"namespace":"default","name":"web","annotations":{` + annos + `}},"spec":{` + spec + `}}`)
	}
	kube.put(`{"apiVersion":"v1","kind":"Service","metadata":{"namespace":"default","name":"web"},"spec":{"ports":[{"port":80}]}}`)
	kube.put(`{"apiVersion":"v1","kind":"Secret","metadata":{"namespace":"default","name":"tls"},"type":"kubernetes.io/tls","data":{"tls.crt":"Q0VSVA==","tls.key":"S0VZ"}}`)
	backend := `"defaultBackend":{"service":{"name":"web","port":{"number":80}}}`
	ingress(backend+`,"tls":[{"secretName":"tls"}]`, "")
	kube.put(`{"apiVersion":"gateway.networking.k8s.io/v1","kind":"Gateway","metadata":{"namespace":"default","name":"ext","generation":1},"spec":{
		"gatewayClassName":"gke-l7-global-external-managed","listeners":[{"name":"http","port":80,"protocol":"HTTP"}]}}`)
	sync()
	sync() // the Gateway's class exists from the first
	annos := func() map[string]any {
		a, _ := field(kube.get(ingPath), "metadata.annotations").(map[string]any)
		return a
	}
	if annos()["ingress.kubernetes.io/https-forwarding-rule"] == nil {
		t.Fatalf("Ingress annotations %v, want the HTTPS forwarding rule", annos())
	}
	if a, _ := field(kube.get(gwPath), "status.addresses").([]any); len(a) != 1 {
		t.Fatalf("Gateway addresses %v, want one", a)
	}

	// Keep the status and annotations written, change the spec.
	cur := kube.get(ingPath)
	cur["spec"] = map[string]any{"defaultBackend": map[string]any{"service": map[string]any{"name": "web", "port": map[string]any{"number": 80}}}}
	sync()
	if a := annos(); a["ingress.kubernetes.io/https-forwarding-rule"] != nil || a["ingress.kubernetes.io/ssl-cert"] != nil || a["ingress.kubernetes.io/forwarding-rule"] == nil {
		t.Errorf("after dropping TLS: annotations %v", a)
	}
	field(kube.get(ingPath), "metadata.annotations").(map[string]any)["kubernetes.io/ingress.allow-http"] = "false"
	sync()
	if ing, _ := field(kube.get(ingPath), "status.loadBalancer.ingress").([]any); len(ing) != 0 {
		t.Errorf("Ingress without forwarding rules: status %v, want no address", ing)
	}

	field(kube.get(gwPath), "spec").(map[string]any)["listeners"] = []any{map[string]any{"name": "udp", "port": 53, "protocol": "UDP"}}
	sync()
	var gst gwStatus
	b, _ := json.Marshal(field(kube.get(gwPath), "status"))
	_ = json.Unmarshal(b, &gst)
	if len(gst.Addresses) != 0 || cond(gst.Conditions, "Accepted").Status != "False" || cond(gst.Conditions, "Programmed").Reason != "Invalid" {
		t.Errorf("Gateway without a servable listener: status %s", b)
	}
}
