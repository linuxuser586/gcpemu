package lb_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
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

// TestGKEIngressAndGateway runs GKE's Ingress and Gateway API controllers
// against a real cluster (FR-GKE-010, container runtime required): an
// Ingress of class gce and a Gateway of class
// gke-l7-global-external-managed with an HTTPRoute each become a load
// balancer on the Service's NEGs that serves the pod, their addresses are
// reported in status, and deleting the Ingress removes its load balancer.
func TestGKEIngressAndGateway(t *testing.T) {
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
		Name:          "l7",
		NetworkConfig: &containerpb.NetworkConfig{GatewayApiConfig: &containerpb.GatewayAPIConfig{Channel: containerpb.GatewayAPIConfig_CHANNEL_STANDARD}},
		NodePools:     []*containerpb.NodePool{{Name: "np", InitialNodeCount: 1}}}})
	if err != nil {
		t.Fatal(err)
	}
	image := pushEchoImage(t, inst)
	c, err := computev1.NewService(ctx, option.WithEndpoint(inst.GatewayURL()+"/compute/v1/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	waitCluster(t, cm, zone, op.Name)
	k := newKube(t, inst, par+"/clusters/l7")

	k.apply(deployment("web", image, 1, 8080, nil))
	k.apply(map[string]any{
		"apiVersion": "v1", "kind": "Service",
		"metadata": map[string]any{"name": "web", "namespace": "default"},
		"spec":     map[string]any{"selector": map[string]string{"app": "web"}, "ports": []map[string]any{{"port": 80, "targetPort": 8080}}},
	})
	k.create("/apis/networking.k8s.io/v1/namespaces/default/ingresses", map[string]any{
		"apiVersion": "networking.k8s.io/v1", "kind": "Ingress",
		"metadata": map[string]any{"name": "web", "namespace": "default"},
		"spec": map[string]any{"rules": []map[string]any{{"http": map[string]any{"paths": []map[string]any{{
			"path": "/", "pathType": "Prefix",
			"backend": map[string]any{"service": map[string]any{"name": "web", "port": map[string]any{"number": 80}}}}}}}}},
	})

	// The GatewayClass appears once the Gateway API CRDs are installed.
	const gwAPI = "/apis/gateway.networking.k8s.io/v1"
	eventually(t, 2*time.Minute, "GatewayClass accepted", func() bool {
		var gc struct {
			Status struct {
				Conditions []struct{ Type, Status string }
			}
		}
		if !k.getJSON(gwAPI+"/gatewayclasses/gke-l7-global-external-managed", &gc) {
			return false
		}
		for _, c := range gc.Status.Conditions {
			if c.Type == "Accepted" && c.Status == "True" {
				return true
			}
		}
		return false
	})
	k.create(gwAPI+"/namespaces/default/gateways", map[string]any{
		"apiVersion": "gateway.networking.k8s.io/v1", "kind": "Gateway",
		"metadata": map[string]any{"name": "ext", "namespace": "default"},
		"spec": map[string]any{"gatewayClassName": "gke-l7-global-external-managed",
			"listeners": []map[string]any{{"name": "http", "port": 80, "protocol": "HTTP"}}},
	})
	k.create(gwAPI+"/namespaces/default/httproutes", map[string]any{
		"apiVersion": "gateway.networking.k8s.io/v1", "kind": "HTTPRoute",
		"metadata": map[string]any{"name": "web", "namespace": "default"},
		"spec": map[string]any{"parentRefs": []map[string]any{{"name": "ext"}}, "rules": []map[string]any{{
			"matches":     []map[string]any{{"path": map[string]any{"type": "PathPrefix", "value": "/app"}}},
			"filters":     []map[string]any{{"type": "URLRewrite", "urlRewrite": map[string]any{"path": map[string]any{"type": "ReplacePrefixMatch", "replacePrefixMatch": "/"}}}},
			"backendRefs": []map[string]any{{"name": "web", "port": 80}}}}},
	})

	// listener returns the host listener of the forwarding rule at ip.
	listener := func(ip string) string {
		l, err := c.GlobalForwardingRules.List(proj).Do()
		if err != nil {
			t.Fatal(err)
		}
		for _, fr := range l.Items {
			if fr.IPAddress == ip {
				return inst.Endpoint("lb:" + fr.Name)
			}
		}
		return ""
	}
	cl := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	fetch := func(addr, path string) string {
		if addr == "" {
			return ""
		}
		resp, err := cl.Get("http://" + addr + path)
		if err != nil {
			return err.Error()
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.Status + " " + string(b)
	}

	// Ingress: status.loadBalancer.ingress, then the pod through the LB.
	var ingIP, ingFR string
	eventually(t, 3*time.Minute, "Ingress address", func() bool {
		var ing struct {
			Metadata struct{ Annotations map[string]string }
			Status   struct {
				LoadBalancer struct{ Ingress []struct{ IP string } }
			}
		}
		if !k.getJSON("/apis/networking.k8s.io/v1/namespaces/default/ingresses/web", &ing) || len(ing.Status.LoadBalancer.Ingress) == 0 {
			return false
		}
		ingIP, ingFR = ing.Status.LoadBalancer.Ingress[0].IP, ing.Metadata.Annotations["ingress.kubernetes.io/forwarding-rule"]
		return ingIP != "" && ingFR != ""
	})
	if fr, err := c.GlobalForwardingRules.Get(proj, ingFR).Do(); err != nil || fr.IPAddress != ingIP || fr.LoadBalancingScheme != "EXTERNAL" {
		t.Fatalf("Ingress forwarding rule %s: %v %+v, want EXTERNAL at %s", ingFR, err, fr, ingIP)
	}
	var body string
	eventually(t, 2*time.Minute, "web pod via the Ingress", func() bool {
		body = fetch(listener(ingIP), "/hello")
		return strings.HasPrefix(body, "200 ") && strings.Contains(body, "pod=web-")
	})
	if !strings.Contains(body, "path=/hello") {
		t.Errorf("Ingress response %q", body)
	}

	// Gateway: status.addresses and Programmed, then the pod with the
	// route's rewrite.
	var gwIP string
	eventually(t, 2*time.Minute, "Gateway programmed", func() bool {
		var gw struct {
			Status struct {
				Addresses  []struct{ Value string }
				Conditions []struct{ Type, Status string }
			}
		}
		if !k.getJSON(gwAPI+"/namespaces/default/gateways/ext", &gw) || len(gw.Status.Addresses) == 0 {
			return false
		}
		for _, c := range gw.Status.Conditions {
			if c.Type == "Programmed" && c.Status == "True" {
				gwIP = gw.Status.Addresses[0].Value
			}
		}
		return gwIP != ""
	})
	eventually(t, 2*time.Minute, "web pod via the Gateway", func() bool {
		body = fetch(listener(gwIP), "/app/hello")
		return strings.HasPrefix(body, "200 ") && strings.Contains(body, "path=/hello")
	})
	if got := fetch(listener(gwIP), "/other"); strings.HasPrefix(got, "200 ") {
		t.Errorf("GET /other on the Gateway: %q, want the default backend's error", got)
	}
	var route struct {
		Status struct {
			Parents []struct {
				ControllerName string
				Conditions     []struct{ Type, Status string }
			}
		}
	}
	k.getJSON(gwAPI+"/namespaces/default/httproutes/web", &route)
	if len(route.Status.Parents) != 1 || route.Status.Parents[0].ControllerName != "networking.gke.io/gateway" {
		t.Errorf("HTTPRoute status %+v", route.Status)
	}

	// Deleting the Ingress removes its load balancer.
	k.do("DELETE", "/apis/networking.k8s.io/v1/namespaces/default/ingresses/web", "application/json", nil)
	eventually(t, time.Minute, "Ingress forwarding rule removed", func() bool {
		_, err := c.GlobalForwardingRules.Get(proj, ingFR).Do()
		return err != nil
	})
}

// create posts obj to a collection path.
func (k *kube) create(path string, obj map[string]any) {
	k.t.Helper()
	b, _ := json.Marshal(obj)
	k.do("POST", path, "application/json", b)
}

// getJSON reads an object, reporting whether it exists.
func (k *kube) getJSON(path string, out any) bool {
	k.t.Helper()
	req, _ := http.NewRequest("GET", k.endpoint+path, bytes.NewReader(nil))
	resp, err := k.hc.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	return json.NewDecoder(resp.Body).Decode(out) == nil
}
