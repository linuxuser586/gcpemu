package lb_test

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	goruntime "runtime"
	"strings"
	"testing"
	"time"

	container "cloud.google.com/go/container/apiv1"
	"cloud.google.com/go/container/apiv1/containerpb"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	mdns "github.com/miekg/dns"
	computev1 "google.golang.org/api/compute/v1"
	dnsv1 "google.golang.org/api/dns/v1"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"gopkg.in/yaml.v3"

	"github.com/linuxuser586/gcpemu/emutest"
	"github.com/linuxuser586/gcpemu/internal/agent"
	"github.com/linuxuser586/gcpemu/internal/ca"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/services/dns"
	"github.com/linuxuser586/gcpemu/services/lb"
)

// TestGKEBackends runs the load balancer against a real GKE cluster
// (container runtime required):
//
//   - FR-INT-001: a backend service on a GKE standalone NEG routes to ready
//     pods through the lb-edge relay, and follows a Deployment scale-up
//     within 5 s of the NEG update;
//   - FR-INT-002 / FR-LB-006 (without Istio): backend mTLS to a pod that
//     requires client certificates signed by the LB's CA; the LB is
//     accepted, a direct client without the certificate is refused;
//   - FR-LB-002 / FR-INT-004: a forwarding rule on port 443 is served by
//     the lb-edge container on its real port, and emulated DNS resolves the
//     domain to it; the backend sees the client address (PROXY v2).
func TestGKEBackends(t *testing.T) {
	emutest.RequireRuntime(t)
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
		Name: "lb", NodePools: []*containerpb.NodePool{{Name: "np", InitialNodeCount: 1}}}})
	if err != nil {
		t.Fatal(err)
	}
	image := pushEchoImage(t, inst)
	c, err := computev1.NewService(ctx, option.WithEndpoint(inst.GatewayURL()+"/compute/v1/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, inst: inst, c: c}
	clientCA, _ := ca.Load(t.TempDir(), "lb-client-ca")
	meshCA, _ := ca.Load(t.TempDir(), "mesh-ca")
	auth := e.backendMTLS(clientCA, meshCA)

	waitCluster(t, cm, zone, op.Name)
	k := newKube(t, inst, par+"/clusters/lb")

	srvCert, _ := meshCA.Issue(ca.Leaf{DNSNames: []string{"gateway.mesh.test"}})
	srvChain, srvKey, _ := ca.EncodeCert(srvCert)
	k.apply(deployment("web", image, 1, 8080, nil))
	k.apply(deployment("secure", image, 1, 8443, map[string]string{
		"ECHO_PORT": "8443", "ECHO_TLS_CERT": string(srvChain), "ECHO_TLS_KEY": string(srvKey),
		"ECHO_CLIENT_CA": string(clientCA.PEM()), "ECHO_HEALTH_PORT": "8080"}))
	k.apply(negService("web", 8080))
	k.apply(negService("secure", 8443))

	negEndpoints := func(neg string) int {
		l, err := c.NetworkEndpointGroups.ListNetworkEndpoints(proj, zone, neg, &computev1.NetworkEndpointGroupsListEndpointsRequest{}).Do()
		if err != nil {
			return -1
		}
		return len(l.Items)
	}
	eventually(t, 3*time.Minute, "NEG endpoints", func() bool { return negEndpoints("web-neg") == 1 && negEndpoints("secure-neg") == 1 })

	e.do(c.HealthChecks.Insert(proj, &computev1.HealthCheck{Name: "hc", CheckIntervalSec: 1, TimeoutSec: 1,
		HttpHealthCheck: &computev1.HTTPHealthCheck{PortSpecification: "USE_SERVING_PORT", RequestPath: "/healthz"}}).Do())
	e.do(c.HealthChecks.Insert(proj, &computev1.HealthCheck{Name: "hc-secure", CheckIntervalSec: 1, TimeoutSec: 1,
		HttpHealthCheck: &computev1.HTTPHealthCheck{Port: 8080, RequestPath: "/healthz"}}).Do())
	e.do(c.BackendServices.Insert(proj, &computev1.BackendService{Name: "web", LoadBalancingScheme: "EXTERNAL_MANAGED",
		HealthChecks: []string{"global/healthChecks/hc"},
		Backends:     []*computev1.Backend{{Group: "zones/" + zone + "/networkEndpointGroups/web-neg", BalancingMode: "RATE", MaxRatePerEndpoint: 100}}}).Do())
	e.do(c.BackendServices.Insert(proj, &computev1.BackendService{Name: "secure", LoadBalancingScheme: "EXTERNAL_MANAGED", Protocol: "HTTPS",
		HealthChecks: []string{"global/healthChecks/hc-secure"},
		TlsSettings:  &computev1.BackendServiceTlsSettings{AuthenticationConfig: auth, Sni: "gateway.mesh.test"},
		Backends:     []*computev1.Backend{{Group: "zones/" + zone + "/networkEndpointGroups/secure-neg", BalancingMode: "RATE", MaxRatePerEndpoint: 100}}}).Do())
	e.do(c.UrlMaps.Insert(proj, &computev1.UrlMap{Name: "map", DefaultService: "global/backendServices/web",
		HostRules: []*computev1.HostRule{{Hosts: []string{"*"}, PathMatcher: "m"}},
		PathMatchers: []*computev1.PathMatcher{{Name: "m", DefaultService: "global/backendServices/web",
			PathRules: []*computev1.PathRule{{Paths: []string{"/api/*"}, Service: "global/backendServices/secure"}}}}}).Do())
	leaf, _ := inst.Env.CA.Issue(ca.Leaf{DNSNames: []string{"app.example.test"}})
	chain, key, _ := ca.EncodeCert(leaf)
	e.do(c.SslCertificates.Insert(proj, &computev1.SslCertificate{Name: "app", Certificate: string(chain), PrivateKey: string(key)}).Do())
	e.do(c.TargetHttpsProxies.Insert(proj, &computev1.TargetHttpsProxy{Name: "https", UrlMap: "global/urlMaps/map", SslCertificates: []string{"global/sslCertificates/app"}}).Do())
	e.do(c.GlobalAddresses.Insert(proj, &computev1.Address{Name: "ip"}).Do())
	e.do(c.GlobalForwardingRules.Insert(proj, &computev1.ForwardingRule{Name: "https", LoadBalancingScheme: "EXTERNAL_MANAGED",
		IPAddress: "global/addresses/ip", PortRange: "443", Target: "global/targetHttpsProxies/https"}).Do())
	fr, _ := c.GlobalForwardingRules.Get(proj, "https").Do()

	// DNS → LB: the domain resolves to the edge address serving port 443.
	dc, _ := dnsv1.NewService(ctx, option.WithEndpoint(inst.GatewayURL()+"/dns/"), option.WithoutAuthentication())
	if _, err := dc.ManagedZones.Create(proj, &dnsv1.ManagedZone{Name: "example", DnsName: "example.test.", Description: "x"}).Do(); err != nil {
		t.Fatal(err)
	}
	if _, err := dc.Changes.Create(proj, "example", &dnsv1.Change{Additions: []*dnsv1.ResourceRecordSet{
		{Name: "app.example.test.", Type: "A", Ttl: 60, Rrdatas: []string{fr.IPAddress}}}}).Do(); err != nil {
		t.Fatal(err)
	}
	listener := inst.Endpoint("lb:https")
	lh, lp, _ := net.SplitHostPort(listener)
	if lp != "443" {
		t.Fatalf("listener %s: want the edge on port 443", listener)
	}
	ds, _ := inst.Env.Lookup("dns")
	rrs, _ := ds.(*dns.Service).Resolve(ctx, "app.example.test", mdns.TypeA)
	if len(rrs) != 1 || rrs[0].(*mdns.A).A.String() != lh {
		t.Fatalf("app.example.test resolves to %v, want %s", rrs, lh)
	}
	cl := &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: inst.Env.CA.Pool()},
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, net.JoinHostPort(lh, "443"))
		},
		DisableKeepAlives: true,
	}}
	fetch := func(path string) (int, string) {
		resp, err := cl.Get("https://app.example.test" + path)
		if err != nil {
			return 0, err.Error()
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	// FR-INT-001: pods are reachable through the LB.
	var body string
	eventually(t, 90*time.Second, "web pod via LB", func() bool {
		code, b := fetch("/hello")
		body = b
		return code == 200 && strings.HasPrefix(b, "pod=web-")
	})
	if !strings.Contains(body, "path=/hello") || strings.Contains(body, "xff=127.0.0.1") {
		t.Errorf("web response %q", body)
	}
	if out, err := exec.Command("curl", "-sS", "--max-time", "10", "--cacert", inst.Env.CA.Path(),
		"--resolve", "app.example.test:443:"+lh, "https://app.example.test/curl").CombinedOutput(); err != nil || !strings.Contains(string(out), "path=/curl") {
		t.Errorf("curl https://app.example.test/curl: %v %s", err, out)
	}

	// Scale up: within 5 s of the NEG update both pods serve.
	k.patch("/apis/apps/v1/namespaces/default/deployments/web/scale", `{"spec":{"replicas":2}}`)
	eventually(t, 2*time.Minute, "NEG with two endpoints", func() bool { return negEndpoints("web-neg") == 2 })
	t0 := time.Now()
	pods := map[string]bool{}
	eventually(t, 5*time.Second, "both pods serving through the LB (FR-INT-001)", func() bool {
		if code, b := fetch("/"); code == 200 {
			pods[strings.Fields(b)[0]] = true
		}
		return len(pods) == 2
	})
	t.Logf("second pod served %v after the NEG update", time.Since(t0))

	// FR-INT-002 / FR-LB-006: re-encrypted mTLS to the pod.
	eventually(t, 60*time.Second, "secure pod via LB", func() bool {
		code, b := fetch("/api/health")
		body = b
		return code == 200
	})
	if !strings.Contains(body, "client=lb.client.test") {
		t.Fatalf("secure response %q", body)
	}
	// A direct client without the LB's certificate is refused.
	l, _ := c.NetworkEndpointGroups.ListNetworkEndpoints(proj, zone, "secure-neg", &computev1.NetworkEndpointGroupsListEndpointsRequest{}).Do()
	ep := l.Items[0].NetworkEndpoint
	svc, _ := inst.Env.Lookup("lb")
	raw, err := svc.(*lb.Service).DialBackend(ctx, net.JoinHostPort(ep.IpAddress, fmt.Sprint(ep.Port)))
	if err != nil {
		t.Fatalf("relay to pod: %v", err)
	}
	tc := tls.Client(raw, &tls.Config{RootCAs: meshCA.Pool(), ServerName: "gateway.mesh.test"})
	_ = tc.SetDeadline(time.Now().Add(10 * time.Second))
	_, _ = io.WriteString(tc, "GET / HTTP/1.1\r\nHost: x\r\n\r\n")
	if b, err := io.ReadAll(tc); err == nil && strings.Contains(string(b), "200 OK") {
		t.Fatal("pod accepted a client without the LB certificate")
	}
	tc.Close()

	// Health reporting.
	h, err := c.BackendServices.GetHealth(proj, "web", &computev1.ResourceGroupReference{Group: "zones/" + zone + "/networkEndpointGroups/web-neg"}).Do()
	if err != nil || len(h.HealthStatus) != 2 {
		t.Fatalf("getHealth = %+v, %v", h, err)
	}
	for _, hs := range h.HealthStatus {
		if hs.HealthState != "HEALTHY" || !strings.Contains(hs.Instance, "/instances/") {
			t.Errorf("health status %+v", hs)
		}
	}
}

func eventually(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %v waiting for %s", timeout, what)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func waitCluster(t *testing.T, cm *container.ClusterManagerClient, zone, op string) {
	t.Helper()
	eventually(t, 3*time.Minute, "cluster RUNNING", func() bool {
		o, err := cm.GetOperation(context.Background(), &containerpb.GetOperationRequest{Name: "projects/" + proj + "/locations/" + zone + "/operations/" + op})
		if err != nil {
			t.Fatal(err)
		}
		if o.Status == containerpb.Operation_DONE && o.Error != nil {
			t.Fatalf("cluster create failed: %s", o.Error.Message)
		}
		return o.Status == containerpb.Operation_DONE
	})
}

// kube is a minimal Kubernetes API client with cluster-admin credentials.
type kube struct {
	t        *testing.T
	endpoint string
	hc       *http.Client
}

func newKube(t *testing.T, inst *emutest.Instance, cluster string) *kube {
	resp, err := http.Get(inst.GatewayURL() + "/container/_emu/kubeconfig?cluster=" + cluster)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var kc struct {
		Clusters []struct {
			Cluster map[string]string `yaml:"cluster"`
		} `yaml:"clusters"`
		Users []struct {
			User map[string]string `yaml:"user"`
		} `yaml:"users"`
	}
	if err := yaml.Unmarshal(b, &kc); err != nil || len(kc.Clusters) != 1 {
		t.Fatalf("kubeconfig: %v %s", err, b)
	}
	dec := func(s string) []byte { v, _ := base64.StdEncoding.DecodeString(s); return v }
	cert, err := tls.X509KeyPair(dec(kc.Users[0].User["client-certificate-data"]), dec(kc.Users[0].User["client-key-data"]))
	if err != nil {
		t.Fatal(err)
	}
	pool := mustPool(dec(kc.Clusters[0].Cluster["certificate-authority-data"]))
	return &kube{t: t, endpoint: kc.Clusters[0].Cluster["server"], hc: &http.Client{Timeout: 30 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, Certificates: []tls.Certificate{cert}}}}}
}

func (k *kube) do(method, path, ct string, body []byte) {
	k.t.Helper()
	req, _ := http.NewRequest(method, k.endpoint+path, bytes.NewReader(body))
	req.Header.Set("Content-Type", ct)
	resp, err := k.hc.Do(req)
	if err != nil {
		k.t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(resp.Body)
		k.t.Fatalf("%s %s: %d %s", method, path, resp.StatusCode, b)
	}
}

// apply creates a namespaced object in "default".
func (k *kube) apply(obj map[string]any) {
	k.t.Helper()
	b, _ := json.Marshal(obj)
	path := "/api/v1/namespaces/default/services"
	if obj["kind"] == "Deployment" {
		path = "/apis/apps/v1/namespaces/default/deployments"
	}
	k.do("POST", path, "application/json", b)
}

func (k *kube) patch(path, merge string) {
	k.t.Helper()
	k.do("PATCH", path, "application/merge-patch+json", []byte(merge))
}

func deployment(name, image string, replicas, port int, env map[string]string) map[string]any {
	envs := []map[string]string{{"name": agent.EnvVar, "value": "lb-echo"}}
	for k, v := range env {
		envs = append(envs, map[string]string{"name": k, "value": v})
	}
	return map[string]any{
		"apiVersion": "apps/v1", "kind": "Deployment",
		"metadata": map[string]any{"name": name, "namespace": "default"},
		"spec": map[string]any{
			"replicas": replicas,
			"selector": map[string]any{"matchLabels": map[string]string{"app": name}},
			"template": map[string]any{
				"metadata": map[string]any{"labels": map[string]string{"app": name}},
				"spec": map[string]any{"containers": []map[string]any{{
					"name": "echo", "image": image, "env": envs,
					"ports":          []map[string]any{{"containerPort": port}},
					"readinessProbe": map[string]any{"periodSeconds": 1, "tcpSocket": map[string]any{"port": port}},
				}}},
			},
		},
	}
}

func negService(name string, port int) map[string]any {
	return map[string]any{
		"apiVersion": "v1", "kind": "Service",
		"metadata": map[string]any{"name": name, "namespace": "default",
			"annotations": map[string]string{"cloud.google.com/neg": fmt.Sprintf(`{"exposed_ports":{"%d":{"name":"%s-neg"}}}`, port, name)}},
		"spec": map[string]any{"selector": map[string]string{"app": name}, "ports": []map[string]any{{"port": port, "targetPort": port}}},
	}
}

// pushEchoImage wraps the static gcpemu agent binary as an image (agent
// "lb-echo"), pushes it to the emulated Artifact Registry and returns its
// LOCATION-docker.pkg.dev reference.
func pushEchoImage(t *testing.T, inst *emutest.Instance) string {
	t.Helper()
	bin, err := agent.Binary()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	var tb bytes.Buffer
	tw := tar.NewWriter(&tb)
	_ = tw.WriteHeader(&tar.Header{Name: "gcpemu", Mode: 0o755, Size: int64(len(data)), Typeflag: tar.TypeReg})
	_, _ = tw.Write(data)
	_ = tw.Close()
	layer, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(tb.Bytes())), nil })
	if err != nil {
		t.Fatal(err)
	}
	img, _ := mutate.AppendLayers(empty.Image, layer)
	cf, _ := img.ConfigFile()
	cf = cf.DeepCopy()
	cf.OS, cf.Architecture = "linux", goruntime.GOARCH
	cf.Config = v1.Config{Entrypoint: []string{"/gcpemu"}}
	if img, err = mutate.ConfigFile(img, cf); err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, inst: inst}
	e.rest("POST", "/artifactregistry/v1/projects/"+proj+"/locations/us-central1/repositories?repositoryId=lb", map[string]any{"format": "DOCKER"})
	keys, _ := inst.Env.Lookup("iam")
	tok, _, err := keys.(emu.ServiceAccountKeys).AccessToken(context.Background(), emu.Principal(inst.Config.DefaultPrincipal))
	if err != nil {
		t.Fatal(err)
	}
	image := "us-central1-docker.pkg.dev/" + proj + "/lb/echo:v1"
	ref, err := name.ParseReference(inst.Endpoint("ar")+"/"+image, name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(ref, img, remote.WithAuth(&authn.Basic{Username: "oauth2accesstoken", Password: tok})); err != nil {
		t.Fatalf("push: %v", err)
	}
	return image
}

func mustPool(pemBytes []byte) *x509.CertPool {
	p := x509.NewCertPool()
	p.AppendCertsFromPEM(pemBytes)
	return p
}

// TestEdgeRetired: the lb-edge container started for a privileged port is
// removed once its last forwarding rule is deleted, so tearing down a load
// balancer leaves no containers (SRS 11.2 step 10).
func TestEdgeRetired(t *testing.T) {
	emutest.RequireRuntime(t)
	e := start(t)
	c := e.c
	e.do(c.BackendServices.Insert(proj, &computev1.BackendService{Name: "be", LoadBalancingScheme: "EXTERNAL_MANAGED"}).Do())
	e.do(c.UrlMaps.Insert(proj, &computev1.UrlMap{Name: "map", DefaultService: "global/backendServices/be"}).Do())
	e.do(c.TargetHttpProxies.Insert(proj, &computev1.TargetHttpProxy{Name: "p", UrlMap: "global/urlMaps/map"}).Do())
	e.do(c.GlobalForwardingRules.Insert(proj, &computev1.ForwardingRule{Name: "fr", LoadBalancingScheme: "EXTERNAL_MANAGED", PortRange: "80", Target: "global/targetHttpProxies/p"}).Do())
	if _, port, _ := net.SplitHostPort(e.inst.Endpoint("lb:fr")); port != "80" {
		t.Fatalf("listener %s: want the edge on port 80", e.inst.Endpoint("lb:fr"))
	}
	edges := func() int {
		cts, err := e.inst.Containers(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, ct := range cts {
			if ct.Service == "lb" {
				n++
			}
		}
		return n
	}
	if edges() != 1 {
		t.Fatalf("edge containers = %d, want 1", edges())
	}
	e.do(c.GlobalForwardingRules.Delete(proj, "fr").Do())
	eventually(t, 30*time.Second, "edge container removed", func() bool { return edges() == 0 })
}
