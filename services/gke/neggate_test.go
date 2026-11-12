package gke

import (
	"context"
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/container/apiv1/containerpb"

	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// Each case of the gate rule sets the condition with GKE's reason, and a
// pod that meets none stays not ready until GKE's timeout.
func TestNEGGateCondition(t *testing.T) {
	created := time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)
	soon := created.Add(time.Minute)
	hc := func(states ...string) []emu.NEGBackendHealth {
		var out []emu.NEGBackendHealth
		for i, s := range states {
			out = append(out, emu.NEGBackendHealth{BackendService: "projects/p/global/backendServices/bs" + string(rune('a'+i)), State: s})
		}
		return out
	}
	for _, tc := range []struct {
		name   string
		in     negGateInput
		ready  bool
		reason string
		msg    string
	}{
		{"HEALTHY in one of two backend services",
			negGateInput{ContainersReady: true, Created: created, Now: soon,
				NEGs: []negGateNEG{{Name: "web-neg", HasEndpoint: true, Health: hc("UNKNOWN", "HEALTHY")}}},
			true, "LoadBalancerNegReady", `Pod has become Healthy in NEG "web-neg" attached to BackendService "projects/p/global/backendServices/bsb"`},
		{"not yet HEALTHY in any",
			negGateInput{ContainersReady: true, Created: created, Now: soon,
				NEGs: []negGateNEG{{Name: "web-neg", HasEndpoint: true, Health: hc("UNKNOWN", "UNHEALTHY")}}},
			false, "", ""},
		{"in a NEG no health-checked backend service uses",
			negGateInput{ContainersReady: true, Created: created, Now: soon,
				NEGs: []negGateNEG{{Name: "web-neg", HasEndpoint: true}}},
			true, "LoadBalancerNegWithoutHealthCheck", `Pod is in NEG "web-neg". NEG is not attached to any BackendService with health checking.`},
		{"covered but not yet in a NEG",
			negGateInput{Created: created, Now: soon, NEGs: []negGateNEG{{Name: "web-neg"}}},
			false, "", ""},
		{"covered by no NEG, containers ready",
			negGateInput{ContainersReady: true, Created: created, Now: soon},
			true, "LoadBalancerNegReady", "Pod does not belong to any NEG."},
		{"covered by no NEG, containers not ready",
			negGateInput{Created: created, Now: soon},
			false, "", ""},
		{"timed out",
			negGateInput{ContainersReady: true, Created: created, Now: created.Add(10*time.Minute + time.Second),
				NEGs: []negGateNEG{{Name: "web-neg", HasEndpoint: true, Health: hc("UNHEALTHY")}}},
			true, "LoadBalancerNegTimeout", `Timeout waiting for pod to become healthy in at least one of the NEG(s): [web-neg]`},
		{"just before the timeout",
			negGateInput{ContainersReady: true, Created: created, Now: created.Add(10 * time.Minute),
				NEGs: []negGateNEG{{Name: "web-neg", HasEndpoint: true, Health: hc("UNHEALTHY")}}},
			false, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := negGateCondition(tc.in)
			if got.Ready != tc.ready || got.Reason != tc.reason || !strings.Contains(got.Message, tc.msg) {
				t.Errorf("got %+v, want ready %v, reason %q, message containing %q", got, tc.ready, tc.reason, tc.msg)
			}
			if got.Ready && !strings.HasSuffix(got.Message, `Marking condition "cloud.google.com/load-balancer-neg-ready" to True.`) {
				t.Errorf("message %q", got.Message)
			}
		})
	}
}

// A new pod gets the gate when a Service of its namespace selects it and
// either exposes NEGs by annotation or is a load balancer's NEG Service;
// kube-system, and a pod that has the gate, are left alone.
func TestNEGGatePatch(t *testing.T) {
	svc := func(name, neg string, selector map[string]string) kubeService {
		var s kubeService
		s.Metadata = objectMeta{Name: name, Namespace: "default"}
		if neg != "" {
			s.Metadata.Annotations = map[string]string{annoNEG: neg}
		}
		s.Spec.Selector = selector
		return s
	}
	exposed := `{"exposed_ports":{"80":{"name":"web-neg"}}}`
	web := map[string]string{"app": "web"}
	pod := func(gates ...string) *gatedPod {
		p := &gatedPod{}
		p.Metadata.Labels = map[string]string{"app": "web", "tier": "front"}
		for _, g := range gates {
			p.Spec.ReadinessGates = append(p.Spec.ReadinessGates, readinessGate{ConditionType: g})
		}
		return p
	}
	add := `[{"op":"add","path":"/spec/readinessGates","value":[{"conditionType":"cloud.google.com/load-balancer-neg-ready"}]}]`
	for _, tc := range []struct {
		name  string
		ns    string
		pod   *gatedPod
		svcs  []kubeService
		lb    map[string]bool
		patch string
	}{
		{"NEG annotation with exposed ports", "default", pod(), []kubeService{svc("web", exposed, web)}, nil, add},
		{"a load balancer's NEG Service", "default", pod(), []kubeService{svc("web", "", web)}, map[string]bool{"default/web": true}, add},
		{"a load balancer's NEG Service in another namespace", "default", pod(), []kubeService{svc("web", "", web)}, map[string]bool{"other/web": true}, "null"},
		{"NEG annotation without exposed ports", "default", pod(), []kubeService{svc("web", `{"ingress":true}`, web)}, nil, "null"},
		{"Service selecting other pods", "default", pod(), []kubeService{svc("api", exposed, map[string]string{"app": "api"})}, nil, "null"},
		{"Service without a selector", "default", pod(), []kubeService{svc("web", exposed, nil)}, nil, "null"},
		{"plain Service", "default", pod(), []kubeService{svc("web", "", web)}, nil, "null"},
		{"kube-system", "kube-system", pod(), []kubeService{svc("web", exposed, web)}, nil, "null"},
		{"already gated", "default", pod(negReadinessGate), []kubeService{svc("web", exposed, web)}, nil, "null"},
		{"other gates kept", "default", pod("example.com/ready"), []kubeService{svc("web", exposed, web)}, nil,
			`[{"op":"add","path":"/spec/readinessGates/-","value":{"conditionType":"cloud.google.com/load-balancer-neg-ready"}}]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, _ := json.Marshal(negGatePatch(tc.pod, tc.ns, tc.svcs, tc.lb))
			if string(b) != tc.patch {
				t.Errorf("patch %s, want %s", b, tc.patch)
			}
		})
	}
}

// healthLB is a fakeLB that reports NEG endpoint health, by
// "zone/neg/ip:port".
type healthLB struct {
	*fakeLB
	hmu    sync.Mutex
	health map[string][]emu.NEGBackendHealth
}

func (h *healthLB) NEGEndpointHealth(ctx context.Context, project, zone, neg, ip string, port int) []emu.NEGBackendHealth {
	h.hmu.Lock()
	defer h.hmu.Unlock()
	return h.health[zone+"/"+neg+"/"+ip+":"+strconv.Itoa(port)]
}

func (h *healthLB) set(k string, hs ...emu.NEGBackendHealth) {
	h.hmu.Lock()
	defer h.hmu.Unlock()
	h.health[k] = hs
}

// The NEG sync takes a gated pod's endpoint once its containers are ready
// (its EndpointSlice says not ready until the gate holds), and sets the
// gate's condition: on HEALTHY behind a health check, at once without
// one, once containers ready for a pod no NEG covers, and never back.
func TestNEGGateSync(t *testing.T) {
	s := newUnitService(t)
	lb := &healthLB{fakeLB: &fakeLB{res: map[string]map[string]any{}}, health: map[string][]emu.NEGBackendHealth{}}
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
	sync := func() {
		t.Helper()
		if err := s.syncLoadBalancers(ctx, key); err != nil {
			t.Fatal(err)
		}
	}
	created := time.Now().UTC().Format(time.RFC3339)
	pod := func(name, app, ip string, gated bool, containersReady string) {
		gates := `[]`
		if gated {
			gates = `[{"conditionType":"` + negReadinessGate + `"}]`
		}
		kube.put(`{"apiVersion":"v1","kind":"Pod","metadata":{"namespace":"default","name":"` + name + `","labels":{"app":"` + app + `"},
			"creationTimestamp":"` + created + `"},"spec":{"readinessGates":` + gates + `},
			"status":{"podIP":"` + ip + `","conditions":[{"type":"ContainersReady","status":"` + containersReady + `"}]}}`)
	}
	service := func(name string) {
		kube.put(`{"apiVersion":"v1","kind":"Service","metadata":{"namespace":"default","name":"` + name + `",
			"annotations":{"cloud.google.com/neg":"{\"exposed_ports\":{\"80\":{\"name\":\"` + name + `-neg\"}}}"}},
			"spec":{"selector":{"app":"` + name + `"},"ports":[{"port":80,"targetPort":8080}]}}`)
	}
	type ep struct {
		pod, ip string
		ready   bool
	}
	slice := func(svc string, eps ...ep) {
		var items []string
		for _, e := range eps {
			items = append(items, `{"addresses":["`+e.ip+`"],"conditions":{"ready":`+strconv.FormatBool(e.ready)+`},"nodeName":"n1",
				"targetRef":{"kind":"Pod","namespace":"default","name":"`+e.pod+`"}}`)
		}
		kube.put(`{"apiVersion":"discovery.k8s.io/v1","kind":"EndpointSlice","metadata":{"namespace":"default","name":"` + svc + `-x",
			"labels":{"kubernetes.io/service-name":"` + svc + `"}},"addressType":"IPv4","ports":[{"name":"","port":8080}],
			"endpoints":[` + strings.Join(items, ",") + `]}`)
	}
	gate := func(name string) podCondition {
		t.Helper()
		var p gatedPod
		b, _ := json.Marshal(kube.get("/api/v1/namespaces/default/pods/" + name))
		_ = json.Unmarshal(b, &p)
		for _, c := range p.Status.Conditions {
			if c.Type == negReadinessGate {
				return c
			}
		}
		return podCondition{}
	}
	negIPs := func(neg string) []string {
		vpc.mu.Lock()
		defer vpc.mu.Unlock()
		var ips []string
		for _, e := range vpc.negs["us-central1-a/"+neg] {
			ips = append(ips, e.IP)
		}
		return ips
	}

	service("web")
	service("api")
	pod("web-old", "web", "10.0.0.1", false, "True")
	pod("web-new", "web", "10.0.0.2", true, "True")
	pod("web-starting", "web", "10.0.0.3", true, "False")
	pod("api-1", "api", "10.0.1.1", true, "True")
	pod("lonely", "other", "10.0.2.1", true, "True")
	pod("lonely-starting", "other", "10.0.2.2", true, "False")
	slice("web", ep{"web-old", "10.0.0.1", true}, ep{"web-new", "10.0.0.2", false}, ep{"web-starting", "10.0.0.3", false})
	slice("api", ep{"api-1", "10.0.1.1", false})
	bs := "projects/p/global/backendServices/web"
	lb.set("us-central1-a/web-neg/10.0.0.2:8080", emu.NEGBackendHealth{BackendService: bs, State: "UNKNOWN"})
	sync()

	if got := negIPs("web-neg"); !slices.Equal(got, []string{"10.0.0.1", "10.0.0.2"}) {
		t.Errorf("web-neg endpoints %v, want the ready pod's and the gated containers-ready pod's", got)
	}
	if c := gate("web-new"); c.Status != "" {
		t.Errorf("web-new before HEALTHY: condition %+v, want none", c)
	}
	if c := gate("api-1"); c.Status != "True" || c.Reason != "LoadBalancerNegWithoutHealthCheck" || c.LastTransitionTime == "" {
		t.Errorf("api-1 in a NEG without a health check: condition %+v", c)
	}
	if c := gate("lonely"); c.Status != "True" || c.Reason != "LoadBalancerNegReady" || !strings.Contains(c.Message, "does not belong to any NEG") {
		t.Errorf("lonely, covered by no NEG: condition %+v", c)
	}
	if c := gate("lonely-starting"); c.Status != "" {
		t.Errorf("lonely-starting, not containers ready: condition %+v, want none", c)
	}
	if c := gate("web-old"); c.Status != "" {
		t.Errorf("web-old, not gated: condition %+v, want none", c)
	}
	conds := func(name string) int {
		n, _ := field(kube.get("/api/v1/namespaces/default/pods/"+name), "status.conditions").([]any)
		return len(n)
	}
	if n := conds("api-1"); n != 2 {
		t.Errorf("api-1 has %d conditions, want ContainersReady kept beside the gate's", n)
	}

	lb.set("us-central1-a/web-neg/10.0.0.2:8080", emu.NEGBackendHealth{BackendService: bs, State: "HEALTHY"})
	sync()
	if c := gate("web-new"); c.Status != "True" || c.Reason != "LoadBalancerNegReady" || !strings.Contains(c.Message, bs) {
		t.Errorf("web-new once HEALTHY: condition %+v", c)
	}

	// The condition never goes back, and nothing is written again.
	lb.set("us-central1-a/web-neg/10.0.0.2:8080", emu.NEGBackendHealth{BackendService: bs, State: "UNHEALTHY"})
	kube.takePatches()
	sync()
	for _, p := range kube.takePatches() {
		if strings.HasSuffix(p, "/status") {
			t.Errorf("sync with every condition True patched %s", p)
		}
	}
	if c := gate("web-new"); c.Status != "True" {
		t.Errorf("web-new after UNHEALTHY: condition %+v, want True kept", c)
	}

	// A terminating gated pod leaves the NEG.
	field(kube.get("/api/v1/namespaces/default/pods/web-new"), "metadata").(map[string]any)["deletionTimestamp"] = created
	sync()
	if got := negIPs("web-neg"); !slices.Equal(got, []string{"10.0.0.1"}) {
		t.Errorf("web-neg endpoints %v after web-new began terminating, want only web-old's", got)
	}
}
