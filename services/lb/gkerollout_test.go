package lb_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	container "cloud.google.com/go/container/apiv1"
	"cloud.google.com/go/container/apiv1/containerpb"
	computev1 "google.golang.org/api/compute/v1"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/linuxuser586/gcpemu/emutest"
)

const negGate = "cloud.google.com/load-balancer-neg-ready"

// rolloutPod is a pod as the rollout test reads it.
type rolloutPod struct {
	Metadata struct {
		Name              string  `json:"name"`
		DeletionTimestamp *string `json:"deletionTimestamp"`
	} `json:"metadata"`
	Spec struct {
		ReadinessGates []struct {
			ConditionType string `json:"conditionType"`
		} `json:"readinessGates"`
	} `json:"spec"`
	Status struct {
		PodIP      string `json:"podIP"`
		Conditions []struct {
			Type, Status, Reason, Message string
		} `json:"conditions"`
	} `json:"status"`
}

func (p *rolloutPod) gate() (status, reason, message string) {
	for _, c := range p.Status.Conditions {
		if c.Type == negGate {
			return c.Status, c.Reason, c.Message
		}
	}
	return "", "", ""
}

func (p *rolloutPod) gated() bool {
	for _, g := range p.Spec.ReadinessGates {
		if g.ConditionType == negGate {
			return true
		}
	}
	return false
}

// pods lists the pods labelled app.
func (k *kube) pods(app string) ([]rolloutPod, error) {
	resp, err := k.hc.Get(k.endpoint + "/api/v1/namespaces/default/pods?labelSelector=app%3D" + app)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var l struct{ Items []rolloutPod }
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("listing pods: %s", resp.Status)
	}
	err = json.NewDecoder(resp.Body).Decode(&l)
	return l.Items, err
}

// TestGKERolloutWaitsForLB is GKE's NEG readiness gate (#1036, ADR 0071)
// against a real cluster: pods behind a NEG-annotated Service get the
// gate, and a rolling update of a 2-replica Deployment (surge 1,
// unavailable 0) behind a health-checked backend service removes no old
// pod before its replacement's endpoint is HEALTHY. Under steady load the
// load balancer is never left without an endpoint to pick: no request is
// failed_to_pick_backend. It is not a TestGKE subtest, whose timings are
// watched (#1004).
func TestGKERolloutWaitsForLB(t *testing.T) {
	emutest.RequireRuntime(t)
	emutest.HoldClusterSlot(t)
	inst := emutest.Start(t, []string{"gke", "lb", "cdn", "ar"})
	ctx := context.Background()
	const zone = "us-central1-a"
	par := "projects/" + proj + "/locations/" + zone
	cm, err := container.NewClusterManagerClient(ctx, option.WithEndpoint(inst.Endpoint("gateway")), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	defer cm.Close()
	op, err := cm.CreateCluster(ctx, &containerpb.CreateClusterRequest{Parent: par, Cluster: &containerpb.Cluster{
		Name: "roll", NodePools: []*containerpb.NodePool{{Name: "np", InitialNodeCount: 1}}}})
	if err != nil {
		t.Fatal(err)
	}
	image := pushEchoImage(t, inst)
	c, err := computev1.NewService(ctx, option.WithEndpoint(inst.GatewayURL()+"/compute/v1/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, inst: inst, c: c}
	waitCluster(t, cm, zone, op.Name)
	k := newKube(t, inst, par+"/clusters/roll")

	// The Service first, so that the Deployment's pods get the gate.
	k.apply(negService("web", 8080))
	d := deployment("web", image, 2, 8080, nil)
	d["spec"].(map[string]any)["strategy"] = map[string]any{"type": "RollingUpdate",
		"rollingUpdate": map[string]any{"maxSurge": 1, "maxUnavailable": 0}}
	k.apply(d)
	// Gated pods join the NEG once containers ready, before they are Ready.
	eventually(t, 3*time.Minute, "both pods in the NEG", func() bool {
		l, err := c.NetworkEndpointGroups.ListNetworkEndpoints(proj, zone, "web-neg", &computev1.NetworkEndpointGroupsListEndpointsRequest{}).Do()
		return err == nil && len(l.Items) == 2
	})
	e.do(c.HealthChecks.Insert(proj, &computev1.HealthCheck{Name: "hc", Type: "HTTP", CheckIntervalSec: 1, TimeoutSec: 1,
		HttpHealthCheck: &computev1.HTTPHealthCheck{PortSpecification: "USE_SERVING_PORT", RequestPath: "/healthz"}}).Do())
	e.do(c.BackendServices.Insert(proj, &computev1.BackendService{Name: "web", LoadBalancingScheme: "EXTERNAL_MANAGED",
		HealthChecks: []string{"global/healthChecks/hc"},
		Backends:     []*computev1.Backend{{Group: "zones/" + zone + "/networkEndpointGroups/web-neg", BalancingMode: "RATE", MaxRatePerEndpoint: 100}}}).Do())
	e.do(c.UrlMaps.Insert(proj, &computev1.UrlMap{Name: "map", DefaultService: "global/backendServices/web"}).Do())
	e.do(c.TargetHttpProxies.Insert(proj, &computev1.TargetHttpProxy{Name: "p", UrlMap: "global/urlMaps/map"}).Do())
	e.do(c.GlobalForwardingRules.Insert(proj, &computev1.ForwardingRule{Name: "fr", LoadBalancingScheme: "EXTERNAL_MANAGED",
		PortRange: "8080", Target: "global/targetHttpProxies/p"}).Do())
	cl := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	url := "http://" + inst.Endpoint(ep("fr")) + "/"
	fetch := func() int {
		resp, err := cl.Get(url)
		if err != nil {
			return 0
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		return resp.StatusCode
	}
	health := func() (map[string]string, error) {
		h, err := c.BackendServices.GetHealth(proj, "web", &computev1.ResourceGroupReference{Group: "zones/" + zone + "/networkEndpointGroups/web-neg"}).Do()
		if err != nil {
			return nil, err
		}
		out := map[string]string{}
		for _, hs := range h.HealthStatus {
			out[hs.IpAddress] = hs.HealthState
		}
		return out, nil
	}

	// Both pods are gated and pod Ready. They joined the NEG before any
	// backend service used it, so their gate went True without a health
	// check; the rollout's pods are the ones that wait for HEALTHY.
	old := map[string]bool{}
	eventually(t, 3*time.Minute, "two gated pods Ready behind the load balancer", func() bool {
		pods, err := k.pods("web")
		if err != nil || len(pods) != 2 {
			return false
		}
		for _, p := range pods {
			if st, _, _ := p.gate(); st != "True" {
				return false
			}
			old[p.Metadata.Name] = true
		}
		return fetch() == 200
	})
	pods, err := k.pods("web")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range pods {
		if st, reason, _ := p.gate(); !p.gated() || st != "True" {
			t.Fatalf("pod %s: gated %v, gate %s %q; want the gate, True", p.Metadata.Name, p.gated(), st, reason)
		}
	}
	var startID int64
	for _, r := range lbRequests(t, inst.GatewayURL()) {
		startID = max(startID, r.ID)
	}

	// Steady load, and the pod and getHealth timelines, through a rollout.
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var mu sync.Mutex
	codes := map[int]int{}
	var healthTL []map[string]string
	var podTL [][]rolloutPod
	picks := map[int64]string{}
	every := func(d time.Duration, f func()) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tk := time.NewTicker(d)
			defer tk.Stop()
			for {
				f()
				select {
				case <-stop:
					return
				case <-tk.C:
				}
			}
		}()
	}
	for range 4 {
		every(25*time.Millisecond, func() {
			code := fetch()
			mu.Lock()
			codes[code]++
			mu.Unlock()
		})
	}
	every(100*time.Millisecond, func() {
		if h, err := health(); err == nil {
			mu.Lock()
			healthTL = append(healthTL, h)
			mu.Unlock()
		}
	})
	every(250*time.Millisecond, func() {
		if p, err := k.pods("web"); err == nil {
			mu.Lock()
			podTL = append(podTL, p)
			mu.Unlock()
		}
	})
	// The request log keeps the last 2000 entries, many more than a second
	// of this load makes.
	every(time.Second, func() {
		reqs := lbRequests(t, inst.GatewayURL())
		mu.Lock()
		for _, r := range reqs {
			if r.ID > startID && r.LB != nil && r.LB.StatusDetails == "failed_to_pick_backend" {
				picks[r.ID] = r.LB.StatusDetails
			}
		}
		mu.Unlock()
	})
	time.Sleep(2 * time.Second)
	k.patch("/apis/apps/v1/namespaces/default/deployments/web",
		fmt.Sprintf(`{"spec":{"template":{"metadata":{"annotations":{"kubectl.kubernetes.io/restartedAt":%q}}}}}`, time.Now().Format(time.RFC3339)))
	eventually(t, 5*time.Minute, "the rollout to finish", func() bool {
		pods, err := k.pods("web")
		if err != nil || len(pods) != 2 {
			return false
		}
		for _, p := range pods {
			if st, _, _ := p.gate(); old[p.Metadata.Name] || st != "True" || p.Metadata.DeletionTimestamp != nil {
				return false
			}
		}
		return true
	})
	time.Sleep(3 * time.Second)
	close(stop)
	wg.Wait()

	t.Logf("responses by status: %v; %d getHealth and %d pod snapshots", codes, len(healthTL), len(podTL))
	// The load balancer always had a HEALTHY endpoint.
	for i, h := range healthTL {
		healthy := false
		for _, st := range h {
			healthy = healthy || st == "HEALTHY"
		}
		if !healthy {
			t.Errorf("getHealth snapshot %d of %d has no HEALTHY endpoint: %v", i, len(healthTL), h)
		}
	}
	// No old pod was terminating before a replacement had passed the gate,
	// which it does once its endpoint is HEALTHY in web.
	for i, snap := range podTL {
		var terminating []string
		replaced := false
		for _, p := range snap {
			if old[p.Metadata.Name] && p.Metadata.DeletionTimestamp != nil {
				terminating = append(terminating, p.Metadata.Name)
			}
			st, reason, msg := p.gate()
			if !old[p.Metadata.Name] && st == "True" {
				replaced = true
				if reason != "LoadBalancerNegReady" || !strings.Contains(msg, "backendServices/web") {
					t.Errorf("new pod %s: gate %s %q, want True once HEALTHY in web", p.Metadata.Name, reason, msg)
				}
			}
		}
		if len(terminating) > 0 && !replaced {
			t.Errorf("pod snapshot %d: %v terminating before any replacement passed the gate", i, terminating)
		}
	}
	if len(picks) > 0 {
		t.Errorf("%d requests were failed_to_pick_backend during the rollout", len(picks))
	}
	if codes[200] == 0 {
		t.Error("no request succeeded during the rollout")
	}

	// With the Instance out of reach, the webhook fails open: a pod behind
	// the Kubernetes Service is admitted without the gate. The API server
	// takes up the changed webhook a moment later, so dry runs are sent
	// until one goes to the unreachable URL.
	k.do("PATCH", "/apis/admissionregistration.k8s.io/v1/mutatingwebhookconfigurations/gcpemu-neg-readiness-gate", "application/json-patch+json",
		[]byte(`[{"op":"replace","path":"/webhooks/0/clientConfig/url","value":"https://127.0.0.1:1/unreachable"}]`))
	pod, _ := json.Marshal(map[string]any{"apiVersion": "v1", "kind": "Pod",
		"metadata": map[string]any{"name": "ungated", "labels": map[string]string{"app": "web"}},
		"spec":     d["spec"].(map[string]any)["template"].(map[string]any)["spec"]})
	eventually(t, 30*time.Second, "a pod admitted without the gate while the webhook is out of reach", func() bool {
		resp, err := k.hc.Post(k.endpoint+"/api/v1/namespaces/default/pods?dryRun=All", "application/json", bytes.NewReader(pod))
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		var p rolloutPod
		if resp.StatusCode != http.StatusCreated {
			b, _ := io.ReadAll(resp.Body)
			t.Fatalf("pod refused while the webhook is out of reach: %s %s", resp.Status, b)
		}
		return json.NewDecoder(resp.Body).Decode(&p) == nil && !p.gated()
	})
}

type lbRequest struct {
	ID int64
	LB *struct{ StatusDetails string }
}

// lbRequests returns the load balancer's entries in the request log, or
// none when it can't be read (logged).
func lbRequests(t *testing.T, gateway string) []lbRequest {
	resp, err := http.Get(gateway + "/_emu/v1/requests?service=lb")
	if err != nil {
		t.Logf("request log: %v", err)
		return nil
	}
	defer resp.Body.Close()
	var l struct{ Requests []lbRequest }
	if err := json.NewDecoder(resp.Body).Decode(&l); err != nil {
		t.Logf("request log: %v", err)
	}
	return l.Requests
}
