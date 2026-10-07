package gke_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
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
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"gopkg.in/yaml.v3"

	"github.com/linuxuser586/gcpemu/emutest"
)

const (
	project   = "gke-test"
	zone      = "us-central1-a"
	parent    = "projects/" + project + "/locations/" + zone
	testImage = "rancher/klipper-lb:v0.4.17" // preloaded on nodes; has busybox wget
)

// env bundles the shared test instance and cluster.
type env struct {
	t    *testing.T
	inst *emutest.Instance
	cm   *container.ClusterManagerClient
	c    *containerpb.Cluster
	ca   *x509.CertPool
	adm  *http.Client // cluster admin (client certificate)
}

func newClient(t *testing.T, inst *emutest.Instance) *container.ClusterManagerClient {
	cm, err := container.NewClusterManagerClient(context.Background(),
		option.WithEndpoint(inst.Endpoint("gateway")), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cm.Close() })
	return cm
}

func waitOp(t *testing.T, cm *container.ClusterManagerClient, loc, name string, timeout time.Duration) *containerpb.Operation {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		op, err := cm.GetOperation(context.Background(), &containerpb.GetOperationRequest{Name: "projects/" + project + "/locations/" + loc + "/operations/" + name})
		if err != nil {
			t.Fatalf("GetOperation: %v", err)
		}
		if op.Status == containerpb.Operation_DONE {
			if op.Error != nil {
				t.Fatalf("operation %s (%s) failed: %s", name, op.OperationType, op.Error.Message)
			}
			return op
		}
		if time.Now().After(deadline) {
			t.Fatalf("operation %s (%s) not done after %v", name, op.OperationType, timeout)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// gw issues a REST request to the gateway as the default principal.
func gw(t *testing.T, inst *emutest.Instance, method, path string, body any, out any) int {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, inst.GatewayURL()+path, rd)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if out != nil && resp.StatusCode/100 == 2 {
		if err := json.Unmarshal(b, out); err != nil {
			t.Fatalf("%s %s: %v: %s", method, path, err, b)
		}
	}
	if resp.StatusCode/100 != 2 {
		t.Logf("%s %s → %d %s", method, path, resp.StatusCode, b)
	}
	return resp.StatusCode
}

// kube issues a Kubernetes API request with hc (admin) or a bearer token.
func (e *env) kube(method, path, token string, body any, out any) (int, string) {
	e.t.Helper()
	var rd io.Reader
	ct := "application/json"
	if s, ok := body.(string); ok {
		rd, ct = strings.NewReader(s), "application/merge-patch+json"
	} else if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, "https://"+e.c.Endpoint+path, rd)
	req.Header.Set("Content-Type", ct)
	hc := e.adm
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
		hc = &http.Client{Timeout: 20 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: e.ca}}}
	}
	resp, err := hc.Do(req)
	if err != nil {
		e.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if out != nil && resp.StatusCode/100 == 2 {
		_ = json.Unmarshal(b, out)
	}
	return resp.StatusCode, string(b)
}

func (e *env) mustKube(method, path string, body any, out any) {
	e.t.Helper()
	if code, b := e.kube(method, path, "", body, out); code/100 != 2 {
		e.t.Fatalf("%s %s: %d %s", method, path, code, b)
	}
}

func eventually(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %v waiting for %s", timeout, what)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// adminClient builds an HTTP client from the emulator's kubeconfig endpoint.
func adminClient(t *testing.T, inst *emutest.Instance, name string) (*http.Client, *x509.CertPool) {
	t.Helper()
	resp, err := http.Get(inst.GatewayURL() + "/container/_emu/kubeconfig?cluster=" + name)
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
	if err := yaml.Unmarshal(b, &kc); err != nil || len(kc.Clusters) != 1 || len(kc.Users) != 1 {
		t.Fatalf("kubeconfig: %v\n%s", err, b)
	}
	dec := func(s string) []byte { v, _ := base64.StdEncoding.DecodeString(s); return v }
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(dec(kc.Clusters[0].Cluster["certificate-authority-data"]))
	cert, err := tls.X509KeyPair(dec(kc.Users[0].User["client-certificate-data"]), dec(kc.Users[0].User["client-key-data"]))
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, Certificates: []tls.Certificate{cert}}}}, pool
}

// saToken creates a service account and returns an access token for it.
func saToken(t *testing.T, inst *emutest.Instance, id string) (string, string) {
	t.Helper()
	email := id + "@" + project + ".iam.gserviceaccount.com"
	gw(t, inst, "POST", "/iam/v1/projects/"+project+"/serviceAccounts", map[string]any{"accountId": id}, nil)
	var tok struct {
		AccessToken string `json:"accessToken"`
	}
	if code := gw(t, inst, "POST", "/iamcredentials/v1/projects/-/serviceAccounts/"+email+":generateAccessToken",
		map[string]any{"scope": []string{"https://www.googleapis.com/auth/cloud-platform"}}, &tok); code != 200 || tok.AccessToken == "" {
		t.Fatalf("generateAccessToken: %d", code)
	}
	return email, tok.AccessToken
}

// TestGKE exercises one cluster end to end: create (gRPC) → RUNNING,
// REST surface, IAM-authenticated kubectl access, Workload Identity, NEG
// sync and node pool resize. A private cluster is created alongside to
// check NAT-only egress.
func TestGKE(t *testing.T) {
	emutest.RequireRuntime(t)
	t.Parallel()
	inst := emutest.Start(t, []string{"gke"}, emutest.WithIAMMode("enforce"))
	cm := newClient(t, inst)
	ctx := context.Background()

	// Private cluster (FR-GKE-007) created concurrently to save time.
	var privOp *containerpb.Operation
	var privErr error
	var privWG sync.WaitGroup
	privWG.Add(1)
	go func() {
		// Created over REST on the legacy zones/ path, as gcloud does.
		defer privWG.Done()
		body := map[string]any{"cluster": map[string]any{
			"name":                 "priv",
			"privateClusterConfig": map[string]any{"enablePrivateNodes": true, "masterIpv4CidrBlock": "172.16.0.0/28"},
			"nodePools":            []map[string]any{{"name": "np", "initialNodeCount": 1}},
		}}
		var op map[string]any
		if code := gw(t, inst, "POST", "/container/v1/projects/"+project+"/zones/"+zone+"/clusters", body, &op); code != 200 {
			privErr = fmt.Errorf("REST create: %d", code)
			return
		}
		if op["operationType"] != "CREATE_CLUSTER" || op["status"] != "RUNNING" || !strings.HasPrefix(fmt.Sprint(op["selfLink"]), "https://container.googleapis.com/v1/projects/") {
			privErr = fmt.Errorf("REST create op = %v", op)
			return
		}
		privOp = &containerpb.Operation{Name: fmt.Sprint(op["name"])}
	}()

	start := time.Now()
	op, err := cm.CreateCluster(ctx, &containerpb.CreateClusterRequest{Parent: parent, Cluster: &containerpb.Cluster{
		Name:                   "c1",
		WorkloadIdentityConfig: &containerpb.WorkloadIdentityConfig{WorkloadPool: project + ".svc.id.goog"},
		NodePools:              []*containerpb.NodePool{{Name: "pool-a", InitialNodeCount: 1, Config: &containerpb.NodeConfig{Labels: map[string]string{"team": "web"}}}},
	}})
	if err != nil {
		t.Fatalf("CreateCluster: %v", err)
	}
	if op.OperationType != containerpb.Operation_CREATE_CLUSTER || op.Status != containerpb.Operation_RUNNING {
		t.Errorf("create op = %v", op)
	}
	waitOp(t, cm, zone, op.Name, 3*time.Minute)
	took := time.Since(start)
	t.Logf("cluster create → RUNNING took %v", took)
	if took > 60*time.Second {
		t.Errorf("NFR-PERF-003: create took %v (> 60 s)", took)
	}
	c, err := cm.GetCluster(ctx, &containerpb.GetClusterRequest{Name: parent + "/clusters/c1"})
	if err != nil {
		t.Fatal(err)
	}
	if c.Status != containerpb.Cluster_RUNNING || c.Endpoint == "" || c.MasterAuth.GetClusterCaCertificate() == "" ||
		c.CurrentMasterVersion != "1.36.5-gke.100" || c.Location != zone || c.CurrentNodeCount != 1 {
		t.Fatalf("cluster = %+v", c)
	}
	adm, pool := adminClient(t, inst, parent+"/clusters/c1")
	e := &env{t: t, inst: inst, cm: cm, c: c, ca: pool, adm: adm}

	t.Run("REST", func(t *testing.T) {
		var got map[string]any
		if code := gw(t, inst, "GET", "/container/v1/projects/"+project+"/zones/"+zone+"/clusters/c1", nil, &got); code != 200 {
			t.Fatalf("legacy GET: %d", code)
		}
		if got["status"] != "RUNNING" || got["endpoint"] != c.Endpoint || got["selfLink"] != "https://container.googleapis.com/v1/projects/"+project+"/zones/"+zone+"/clusters/c1" {
			t.Errorf("REST cluster = %v", got)
		}
		var list struct{ Clusters []map[string]any }
		gw(t, inst, "GET", "/container/v1/projects/"+project+"/locations/-/clusters", nil, &list)
		if len(list.Clusters) != 2 {
			t.Errorf("list = %d clusters", len(list.Clusters))
		}
		var sc map[string]any
		gw(t, inst, "GET", "/container/v1/"+parent+"/serverConfig", nil, &sc)
		if sc["defaultClusterVersion"] != "1.36.5-gke.100" {
			t.Errorf("serverConfig = %v", sc)
		}
		if code := gw(t, inst, "GET", "/container/v1/projects/"+project+"/locations/us-central1-z/clusters", nil, nil); code != 404 {
			t.Errorf("bad location → %d", code)
		}
		if code := gw(t, inst, "POST", "/container/v1/"+parent+"/clusters", map[string]any{"cluster": map[string]any{"name": "ap", "autopilot": map[string]any{"enabled": true}}}, nil); code != 501 {
			t.Errorf("autopilot → %d", code)
		}
		var ops struct{ Operations []map[string]any }
		gw(t, inst, "GET", "/container/v1/"+parent+"/operations", nil, &ops)
		if len(ops.Operations) < 2 {
			t.Errorf("operations = %v", ops)
		}
		var nodes struct {
			Items []struct {
				Metadata struct{ Labels map[string]string }
			}
		}
		e.mustKube("GET", "/api/v1/nodes", nil, &nodes)
		if len(nodes.Items) != 1 || nodes.Items[0].Metadata.Labels["cloud.google.com/gke-nodepool"] != "pool-a" ||
			nodes.Items[0].Metadata.Labels["team"] != "web" || nodes.Items[0].Metadata.Labels["topology.kubernetes.io/zone"] != zone {
			t.Errorf("nodes = %+v", nodes)
		}
	})

	t.Run("IAMAuth", func(t *testing.T) {
		email, tok := saToken(t, inst, "kube-dev")
		if code, b := e.kube("GET", "/api/v1/namespaces/default/pods", tok, nil, nil); code != 403 {
			t.Fatalf("no role: %d %s", code, b)
		}
		if code, _ := e.kube("GET", "/api/v1/namespaces/default/pods", "not-a-token", nil, nil); code != 401 {
			t.Errorf("bad token: %d", code)
		}
		policy := map[string]any{"policy": map[string]any{"bindings": []map[string]any{{"role": "roles/container.viewer", "members": []string{"serviceAccount:" + email}}}}}
		if code := gw(t, inst, "POST", "/cloudresourcemanager/v1/projects/"+project+":setIamPolicy", policy, nil); code != 200 {
			t.Fatalf("setIamPolicy: %d", code)
		}
		eventually(t, 20*time.Second, "viewer access", func() bool {
			code, _ := e.kube("GET", "/api/v1/namespaces/default/pods", tok, nil, nil)
			return code == 200
		})
		if code, _ := e.kube("GET", "/api/v1/namespaces/default/secrets", tok, nil, nil); code != 403 {
			t.Errorf("viewer listed secrets: %d", code)
		}
	})

	t.Run("WorkloadIdentity", func(t *testing.T) { testWorkloadIdentity(t, e) })
	t.Run("NEG", func(t *testing.T) { testNEG(t, e) })

	t.Run("PrivateCluster", func(t *testing.T) {
		privWG.Wait()
		if privErr != nil {
			t.Fatalf("create private cluster: %v", privErr)
		}
		waitOp(t, cm, zone, privOp.Name, 3*time.Minute)
		testPrivateEgress(t, inst, cm)
	})

	t.Run("Resize", func(t *testing.T) {
		op, err := cm.SetNodePoolSize(ctx, &containerpb.SetNodePoolSizeRequest{Name: parent + "/clusters/c1/nodePools/pool-a", NodeCount: 2})
		if err != nil {
			t.Fatal(err)
		}
		waitOp(t, cm, zone, op.Name, 3*time.Minute)
		countReady := func() int {
			var nodes struct {
				Items []struct {
					Status struct {
						Conditions []struct{ Type, Status string }
					}
				}
			}
			e.mustKube("GET", "/api/v1/nodes", nil, &nodes)
			n := 0
			for _, it := range nodes.Items {
				for _, c := range it.Status.Conditions {
					if c.Type == "Ready" && c.Status == "True" {
						n++
					}
				}
			}
			return n
		}
		if n := countReady(); n != 2 {
			t.Fatalf("after resize to 2: %d ready nodes", n)
		}
		op, err = cm.SetNodePoolSize(ctx, &containerpb.SetNodePoolSizeRequest{Name: parent + "/clusters/c1/nodePools/pool-a", NodeCount: 1})
		if err != nil {
			t.Fatal(err)
		}
		waitOp(t, cm, zone, op.Name, 3*time.Minute)
		if n := countReady(); n != 1 {
			t.Fatalf("after resize to 1: %d nodes", n)
		}
		np, _ := cm.GetNodePool(ctx, &containerpb.GetNodePoolRequest{Name: parent + "/clusters/c1/nodePools/pool-a"})
		if np.GetInitialNodeCount() != 1 || np.Status != containerpb.NodePool_RUNNING {
			t.Errorf("pool = %+v", np)
		}
	})

	t.Run("Delete", func(t *testing.T) {
		for _, name := range []string{"c1", "priv"} {
			op, err := cm.DeleteCluster(ctx, &containerpb.DeleteClusterRequest{Name: parent + "/clusters/" + name})
			if err != nil {
				t.Fatal(err)
			}
			waitOp(t, cm, zone, op.Name, 2*time.Minute)
		}
		if _, err := cm.GetCluster(ctx, &containerpb.GetClusterRequest{Name: parent + "/clusters/c1"}); err == nil {
			t.Error("cluster still exists")
		}
	})
}

// podLogs returns the last lines of a pod's log.
func (e *env) podLogs(ns, pod string) string {
	_, b := e.kube("GET", "/api/v1/namespaces/"+ns+"/pods/"+pod+"/log?tailLines=1", "", nil, nil)
	return b
}

func (e *env) runPod(ns, name, sa, script string) {
	pod := map[string]any{
		"apiVersion": "v1", "kind": "Pod",
		"metadata": map[string]any{"name": name, "namespace": ns},
		"spec": map[string]any{
			"serviceAccountName": sa,
			"containers": []map[string]any{{
				"name": "c", "image": testImage, "imagePullPolicy": "IfNotPresent",
				"command": []string{"sh", "-c", script},
			}},
		},
	}
	e.mustKube("POST", "/api/v1/namespaces/"+ns+"/pods", pod, nil)
}

func testWorkloadIdentity(t *testing.T, e *env) {
	gsa := "wi-app@" + project + ".iam.gserviceaccount.com"
	gw(t, e.inst, "POST", "/iam/v1/projects/"+project+"/serviceAccounts", map[string]any{"accountId": "wi-app"}, nil)
	e.mustKube("POST", "/api/v1/namespaces/default/serviceaccounts", map[string]any{
		"metadata": map[string]any{"name": "app", "annotations": map[string]string{"iam.gke.io/gcp-service-account": gsa}},
	}, nil)
	script := `while true; do
e=$(wget -q -O - --header 'Metadata-Flavor: Google' http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/email 2>/dev/null)
if t=$(wget -q -O - --header 'Metadata-Flavor: Google' http://169.254.169.254/computeMetadata/v1/instance/service-accounts/default/token 2>/dev/null); then echo "R $e OK $t"; else echo "R $e DENIED"; fi
sleep 1; done`
	e.runPod("default", "wi", "app", script)
	e.runPod("default", "wi-plain", "default", script)
	eventually(t, 60*time.Second, "denied token without binding", func() bool {
		return strings.Contains(e.podLogs("default", "wi"), "R "+gsa+" DENIED")
	})
	eventually(t, 30*time.Second, "federated token for an unannotated KSA", func() bool {
		return strings.Contains(e.podLogs("default", "wi-plain"), "R "+project+".svc.id.goog OK {")
	})
	member := "serviceAccount:" + project + ".svc.id.goog[default/app]"
	pol := map[string]any{"policy": map[string]any{"bindings": []map[string]any{{"role": "roles/iam.workloadIdentityUser", "members": []string{member}}}}}
	if code := gw(t, e.inst, "POST", "/iam/v1/projects/"+project+"/serviceAccounts/"+gsa+":setIamPolicy", pol, nil); code != 200 {
		t.Fatalf("setIamPolicy on GSA: %d", code)
	}
	var line string
	eventually(t, 30*time.Second, "GSA token with binding", func() bool {
		line = e.podLogs("default", "wi")
		return strings.Contains(line, "R "+gsa+" OK {")
	})
	// The token belongs to the GSA.
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	_ = json.Unmarshal([]byte(line[strings.Index(line, "{"):]), &tok)
	resp, err := http.Get(e.inst.GatewayURL() + "/oauth2/v3/tokeninfo?access_token=" + tok.AccessToken)
	if err == nil {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if !strings.Contains(string(b), gsa) {
			t.Errorf("tokeninfo = %s", b)
		}
	}
}

func testNEG(t *testing.T, e *env) {
	deploy := map[string]any{
		"apiVersion": "apps/v1", "kind": "Deployment",
		"metadata": map[string]any{"name": "web", "namespace": "default"},
		"spec": map[string]any{
			"replicas": 1,
			"selector": map[string]any{"matchLabels": map[string]string{"app": "web"}},
			"template": map[string]any{
				"metadata": map[string]any{"labels": map[string]string{"app": "web"}},
				"spec": map[string]any{"containers": []map[string]any{{
					"name": "web", "image": testImage, "imagePullPolicy": "IfNotPresent",
					"command": []string{"sh", "-c", "while true; do echo -e 'HTTP/1.1 200 OK\\r\\n\\r\\nok' | nc -l -p 8080; done"},
					"ports":   []map[string]any{{"containerPort": 8080}},
				}}},
			},
		},
	}
	e.mustKube("POST", "/apis/apps/v1/namespaces/default/deployments", deploy, nil)
	svc := map[string]any{
		"apiVersion": "v1", "kind": "Service",
		"metadata": map[string]any{"name": "web", "namespace": "default",
			"annotations": map[string]string{"cloud.google.com/neg": `{"exposed_ports":{"80":{"name":"web-neg"}}}`}},
		"spec": map[string]any{"selector": map[string]string{"app": "web"}, "ports": []map[string]any{{"port": 80, "targetPort": 8080}}},
	}
	e.mustKube("POST", "/api/v1/namespaces/default/services", svc, nil)
	negEndpoints := func() []map[string]any {
		var out struct {
			Items []map[string]any `json:"items"`
		}
		code := gw(t, e.inst, "POST", "/compute/v1/projects/"+project+"/zones/"+zone+"/networkEndpointGroups/web-neg/listNetworkEndpoints", map[string]any{}, &out)
		if code != 200 {
			return nil
		}
		return out.Items
	}
	eventually(t, 60*time.Second, "NEG with one endpoint", func() bool { return len(negEndpoints()) == 1 })
	var neg map[string]any
	gw(t, e.inst, "GET", "/compute/v1/projects/"+project+"/zones/"+zone+"/networkEndpointGroups/web-neg", nil, &neg)
	if neg["networkEndpointType"] != "GCE_VM_IP_PORT" {
		t.Errorf("NEG = %v", neg)
	}
	var s struct {
		Metadata struct{ Annotations map[string]string }
	}
	eventually(t, 10*time.Second, "neg-status annotation", func() bool {
		e.mustKube("GET", "/api/v1/namespaces/default/services/web", nil, &s)
		return strings.Contains(s.Metadata.Annotations["cloud.google.com/neg-status"], `"80":"web-neg"`)
	})

	// Scale to 2: once the second pod is ready the NEG follows within 5 s.
	e.mustKube("PATCH", "/apis/apps/v1/namespaces/default/deployments/web/scale", `{"spec":{"replicas":2}}`, nil)
	eventually(t, 60*time.Second, "two ready pods", func() bool {
		var d struct{ Status struct{ ReadyReplicas int } }
		e.mustKube("GET", "/apis/apps/v1/namespaces/default/deployments/web", nil, &d)
		return d.Status.ReadyReplicas == 2
	})
	eventually(t, 5*time.Second, "NEG with two endpoints (FR-INT-001)", func() bool { return len(negEndpoints()) == 2 })
	ep := negEndpoints()[0]["networkEndpoint"].(map[string]any)
	if fmt.Sprint(ep["port"]) != "8080" || ep["instance"] == "" {
		t.Errorf("endpoint = %v", ep)
	}

	// LoadBalancer Services get an address from the in-cluster L4
	// allocator (FR-GKE-009).
	e.mustKube("POST", "/api/v1/namespaces/default/services", map[string]any{
		"apiVersion": "v1", "kind": "Service", "metadata": map[string]any{"name": "web-lb"},
		"spec": map[string]any{"type": "LoadBalancer", "selector": map[string]string{"app": "web"}, "ports": []map[string]any{{"port": 8081, "targetPort": 8080}}},
	}, nil)
	eventually(t, 60*time.Second, "LoadBalancer ingress", func() bool {
		var lb struct {
			Status struct {
				LoadBalancer struct{ Ingress []struct{ IP string } }
			}
		}
		e.mustKube("GET", "/api/v1/namespaces/default/services/web-lb", nil, &lb)
		return len(lb.Status.LoadBalancer.Ingress) > 0 && lb.Status.LoadBalancer.Ingress[0].IP != ""
	})

	// Removing the annotation deletes the NEG.
	e.mustKube("PATCH", "/api/v1/namespaces/default/services/web", `{"metadata":{"annotations":{"cloud.google.com/neg":null}}}`, nil)
	eventually(t, 10*time.Second, "NEG deleted", func() bool {
		return gw(t, e.inst, "GET", "/compute/v1/projects/"+project+"/zones/"+zone+"/networkEndpointGroups/web-neg", nil, nil) == 404
	})
}

func testPrivateEgress(t *testing.T, inst *emutest.Instance, cm *container.ClusterManagerClient) {
	c, err := cm.GetCluster(context.Background(), &containerpb.GetClusterRequest{Name: parent + "/clusters/priv"})
	if err != nil {
		t.Fatal(err)
	}
	if c.PrivateClusterConfig.GetPrivateEndpoint() == "" || c.PrivateClusterConfig.GetPublicEndpoint() == "" {
		t.Errorf("private cluster endpoints = %+v", c.PrivateClusterConfig)
	}
	adm, pool := adminClient(t, inst, parent+"/clusters/priv")
	e := &env{t: t, inst: inst, cm: cm, c: c, ca: pool, adm: adm}
	e.runPod("default", "egress", "default", `while true; do if wget -q -T 3 -O /dev/null http://example.com/ 2>/dev/null; then echo EGRESS-OK; else echo EGRESS-BLOCKED; fi; sleep 1; done`)
	eventually(t, 60*time.Second, "egress blocked without NAT", func() bool {
		return strings.Contains(e.podLogs("default", "egress"), "EGRESS-BLOCKED")
	})
	// Configure Cloud NAT for the region (FR-NAT-001/002, FR-INT-009).
	router := map[string]any{
		"name": "nat-router", "network": "projects/" + project + "/global/networks/default",
		"nats": []map[string]any{{"name": "nat", "natIpAllocateOption": "AUTO_ONLY", "sourceSubnetworkIpRangesToNat": "ALL_SUBNETWORKS_ALL_IP_RANGES"}},
	}
	if code := gw(t, inst, "POST", "/compute/v1/projects/"+project+"/regions/us-central1/routers", router, nil); code != 200 {
		t.Fatalf("create router: %d", code)
	}
	eventually(t, 60*time.Second, "egress through NAT", func() bool {
		return strings.Contains(e.podLogs("default", "egress"), "EGRESS-OK")
	})
}
