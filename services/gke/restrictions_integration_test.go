package gke_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/container/apiv1/containerpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/linuxuser586/gcpemu/emutest"
)

// restrictedPods are pods an Autopilot cluster refuses and a Standard one
// admits, with a phrase of GKE Warden's message for each.
var restrictedPods = map[string]struct {
	spec map[string]any
	want string
}{
	"privileged": {map[string]any{"containers": []map[string]any{{"name": "c", "image": testImage,
		"securityContext": map[string]any{"privileged": true}}}},
		`"[denied by autogke-disallow-privilege]":["container c is privileged; not allowed in Autopilot"]`},
	"hostPath": {map[string]any{
		"volumes":    []map[string]any{{"name": "root", "hostPath": map[string]any{"path": "/"}}},
		"containers": []map[string]any{{"name": "c", "image": testImage, "volumeMounts": []map[string]any{{"name": "root", "mountPath": "/host"}}}}},
		`[denied by autogke-no-write-mode-hostpath]`},
	"hostNetwork": {map[string]any{"hostNetwork": true, "containers": []map[string]any{{"name": "c", "image": testImage}}},
		`"[denied by autogke-disallow-hostnamespaces]":["enabling hostNetwork is not allowed in Autopilot."]`},
	"hostPort": {map[string]any{"containers": []map[string]any{{"name": "c", "image": testImage,
		"ports": []map[string]any{{"containerPort": 80, "hostPort": 8080}}}}},
		`"[denied by autogke-no-host-port]":["container c specifies host port 8080; disallowed in Autopilot"]`},
	"NET_ADMIN": {map[string]any{"containers": []map[string]any{{"name": "c", "image": testImage,
		"securityContext": map[string]any{"capabilities": map[string]any{"add": []string{"NET_ADMIN"}}}}}},
		`linux capability 'NET_ADMIN' on container 'c' not allowed`},
}

// podObject is a Pod named name with spec.
func podObject(name string, spec map[string]any) map[string]any {
	return map[string]any{"apiVersion": "v1", "kind": "Pod", "metadata": map[string]any{"name": name}, "spec": spec}
}

// kubeWarned issues a request as the admin and returns its status, body
// and Warning headers.
func (e *env) kubeWarned(method, path string, body any) (int, string, []string) {
	e.t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(method, "https://"+e.c.Endpoint+path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.adm.Do(req)
	if err != nil {
		e.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(rb), resp.Header.Values("Warning")
}

// TestAutopilotWorkloadRestrictions is FR-GKE-016 on a running Autopilot
// cluster: each refused kind of pod is rejected with GKE Warden's message,
// NET_ADMIN is admitted once allowNetAdmin is set (an Istio-injected pod
// then runs), a Google identity's writes to kube-system are refused while
// the cluster's own system pods keep running, and a pod without requests
// is created with Autopilot's defaults and GKE's warning. TestGKE checks
// that a Standard cluster admits the same pods.
func TestAutopilotWorkloadRestrictions(t *testing.T) {
	emutest.RequireRuntime(t)
	t.Parallel()
	emutest.HoldClusterSlot(t)
	inst := emutest.Start(t, []string{"gke"})
	cm := newClient(t, inst)
	ctx := context.Background()
	const region = "us-central1"
	op, err := cm.CreateCluster(ctx, &containerpb.CreateClusterRequest{Parent: "projects/" + project + "/locations/" + region,
		Cluster: &containerpb.Cluster{Name: "wr", Autopilot: &containerpb.Autopilot{Enabled: true}}})
	if err != nil {
		t.Fatalf("CreateCluster: %v", err)
	}
	waitOp(t, cm, region, op.Name)
	name := "projects/" + project + "/locations/" + region + "/clusters/wr"
	c, err := cm.GetCluster(ctx, &containerpb.GetClusterRequest{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	adm, pool := adminClient(t, inst, name)
	e := &env{t: t, inst: inst, cm: cm, c: c, ca: pool, adm: adm}

	for kind, p := range restrictedPods {
		code, b := e.kube("POST", "/api/v1/namespaces/default/pods?dryRun=All", "", podObject("r", p.spec), nil)
		if code != http.StatusForbidden || !strings.Contains(b, `admission webhook \"warden-validating.common-webhooks.networking.gke.io\" denied the request: GKE Warden rejected the request`) ||
			!strings.Contains(b, strings.ReplaceAll(p.want, `"`, `\"`)) || !strings.Contains(b, `"reason":"GKE Warden constraints violations"`) {
			t.Errorf("%s pod: %d %s; want 403 with %s", kind, code, b, p.want)
		}
	}

	// A pod without requests gets Autopilot's defaults, with GKE's warning.
	code, b, warnings := e.kubeWarned("POST", "/api/v1/namespaces/default/pods", podObject("bare", map[string]any{
		"containers": []map[string]any{{"name": "c", "image": testImage, "imagePullPolicy": "IfNotPresent", "command": []string{"sleep", "3600"}}}}))
	if code != http.StatusCreated {
		t.Fatalf("bare pod: %d %s", code, b)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "autopilot-default-resources-mutator:Autopilot updated Pod default/bare: defaulted unspecified 'cpu' resource for containers [c]") {
		t.Errorf("bare pod warnings = %q", warnings)
	}
	var bare struct {
		Spec struct {
			Containers []struct {
				Resources struct {
					Requests map[string]string `json:"requests"`
				} `json:"resources"`
			} `json:"containers"`
		} `json:"spec"`
	}
	e.mustKube("GET", "/api/v1/namespaces/default/pods/bare", nil, &bare)
	if r := bare.Spec.Containers[0].Resources.Requests; r["cpu"] != "500m" || r["memory"] != "2Gi" || r["ephemeral-storage"] != "1Gi" {
		t.Errorf("bare pod requests = %v", r)
	}

	// With allowNetAdmin, an Istio-injected pod (istio-init adding
	// NET_ADMIN and NET_RAW, and the proxy as a sidecar) runs.
	yes := true
	op, err = cm.UpdateCluster(ctx, &containerpb.UpdateClusterRequest{Name: name, Update: &containerpb.ClusterUpdate{
		DesiredAutopilotWorkloadPolicyConfig: &containerpb.WorkloadPolicyConfig{AllowNetAdmin: &yes}}})
	if err != nil {
		t.Fatalf("set allowNetAdmin: %v", err)
	}
	waitOp(t, cm, region, op.Name)
	sleep := []string{"sleep", "3600"}
	e.mustKube("POST", "/api/v1/namespaces/default/pods", map[string]any{
		"apiVersion": "v1", "kind": "Pod",
		"metadata": map[string]any{"name": "istio", "labels": map[string]string{"app": "istio"}},
		"spec": map[string]any{
			"initContainers": []map[string]any{
				{"name": "istio-init", "image": testImage, "imagePullPolicy": "IfNotPresent", "command": []string{"true"},
					"securityContext": map[string]any{"capabilities": map[string]any{"add": []string{"NET_ADMIN", "NET_RAW"}, "drop": []string{"ALL"}}}},
				{"name": "istio-proxy", "image": testImage, "imagePullPolicy": "IfNotPresent", "command": sleep, "restartPolicy": "Always"},
			},
			"containers": []map[string]any{{"name": "app", "image": testImage, "imagePullPolicy": "IfNotPresent", "command": sleep}},
		},
	}, nil)
	eventually(t, 2*time.Minute, "the Istio-injected pod running", func() bool { return running(e.apPods("app%3Distio"), 1) })

	// A Google identity cannot write to kube-system; the cluster's own
	// components there keep running.
	code, b = e.kube("POST", "/api/v1/namespaces/kube-system/configmaps", "user-token",
		map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "mine"}}, nil)
	if want := `the namespace \\\"kube-system\\\" is managed and the request's verb \\\"create\\\" is denied`; code != http.StatusForbidden || !strings.Contains(b, want) {
		t.Errorf("user create in kube-system: %d %s; want 403 with %s", code, b, want)
	}
	if code, b := e.kube("POST", "/api/v1/namespaces/default/configmaps", "user-token",
		map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "mine"}}, nil); code != http.StatusCreated {
		t.Errorf("user create in default: %d %s", code, b)
	}
	var sys struct {
		Items []apPod `json:"items"`
	}
	e.mustKube("GET", "/api/v1/namespaces/kube-system/pods", nil, &sys)
	if len(sys.Items) == 0 {
		t.Error("no kube-system pods")
	}
	for _, p := range sys.Items {
		if p.Status.Phase != "Running" && p.Status.Phase != "Succeeded" {
			t.Errorf("kube-system pod %s is %s", p.Metadata.Name, p.Status.Phase)
		}
	}
}

// TestAutopilotCompatibilityCheck is FR-GKE-017 on a running Standard
// cluster: a cluster whose workloads pass has no issues; a privileged pod,
// a Deployment whose pods set no requests and a pod adding NET_ADMIN each
// give their issue, against the pod or its Deployment, while pods in
// kube-system and finished pods never appear; and the cluster takes
// compatibility auditing but not allowNetAdmin.
func TestAutopilotCompatibilityCheck(t *testing.T) {
	emutest.RequireRuntime(t)
	t.Parallel()
	emutest.HoldClusterSlot(t)
	inst := emutest.Start(t, []string{"gke"})
	cm := newClient(t, inst)
	ctx := context.Background()
	op, err := cm.CreateCluster(ctx, &containerpb.CreateClusterRequest{Parent: parent, Cluster: &containerpb.Cluster{
		Name: "cc", NodePools: []*containerpb.NodePool{{Name: "np", InitialNodeCount: 1}}}})
	if err != nil {
		t.Fatalf("CreateCluster: %v", err)
	}
	waitOp(t, cm, zone, op.Name)
	name := parent + "/clusters/cc"
	c, err := cm.GetCluster(ctx, &containerpb.GetClusterRequest{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	adm, pool := adminClient(t, inst, name)
	e := &env{t: t, inst: inst, cm: cm, c: c, ca: pool, adm: adm}
	check := func() []*containerpb.AutopilotCompatibilityIssue {
		t.Helper()
		resp, err := cm.CheckAutopilotCompatibility(ctx, &containerpb.CheckAutopilotCompatibilityRequest{Name: name})
		if err != nil {
			t.Fatalf("CheckAutopilotCompatibility: %v", err)
		}
		if resp.Summary != "" {
			t.Errorf("summary = %q", resp.Summary)
		}
		return resp.Issues
	}

	// Requests Autopilot keeps as they are.
	requests := map[string]any{"requests": map[string]string{"cpu": "250m", "memory": "512Mi", "ephemeral-storage": "1Gi"}}
	sleep := []string{"sleep", "3600"}
	ctr := func(extra map[string]any) map[string]any {
		c := map[string]any{"name": "c", "image": testImage, "imagePullPolicy": "IfNotPresent", "command": sleep, "resources": requests}
		for k, v := range extra {
			c[k] = v
		}
		return c
	}
	privileged := map[string]any{"securityContext": map[string]any{"privileged": true}}
	e.mustKube("POST", "/api/v1/namespaces/default/pods", podObject("ok", map[string]any{"containers": []map[string]any{ctr(nil)}}), nil)
	if issues := check(); len(issues) != 0 {
		t.Errorf("issues with every workload passing = %v", issues)
	}

	e.mustKube("POST", "/api/v1/namespaces/default/pods", podObject("priv", map[string]any{"containers": []map[string]any{ctr(privileged)}}), nil)
	e.mustKube("POST", "/api/v1/namespaces/default/pods", podObject("netadmin", map[string]any{"containers": []map[string]any{
		ctr(map[string]any{"securityContext": map[string]any{"capabilities": map[string]any{"add": []string{"NET_ADMIN"}}}})}}), nil)
	e.mustKube("POST", "/api/v1/namespaces/kube-system/pods", podObject("sys", map[string]any{"containers": []map[string]any{
		{"name": "c", "image": testImage, "imagePullPolicy": "IfNotPresent", "command": sleep, "securityContext": map[string]any{"privileged": true}}}}), nil)
	e.mustKube("POST", "/api/v1/namespaces/default/pods", podObject("done", map[string]any{"restartPolicy": "Never", "containers": []map[string]any{
		{"name": "c", "image": testImage, "imagePullPolicy": "IfNotPresent", "command": []string{"true"}, "securityContext": map[string]any{"privileged": true}}}}), nil)
	e.mustKube("POST", "/apis/apps/v1/namespaces/default/deployments", map[string]any{
		"apiVersion": "apps/v1", "kind": "Deployment", "metadata": map[string]any{"name": "web"},
		"spec": map[string]any{
			"replicas": 1, "selector": map[string]any{"matchLabels": map[string]string{"app": "web"}},
			"template": map[string]any{"metadata": map[string]any{"labels": map[string]string{"app": "web"}},
				"spec": map[string]any{"containers": []map[string]any{{"name": "c", "image": testImage, "imagePullPolicy": "IfNotPresent", "command": sleep}}}},
		},
	}, nil)
	eventually(t, 2*time.Minute, "the finished pod and the Deployment's pod", func() bool {
		var done apPod
		e.mustKube("GET", "/api/v1/namespaces/default/pods/done", nil, &done)
		return done.Status.Phase == "Succeeded" && len(e.apPods("app%3Dweb")) == 1
	})

	type issue struct {
		constraint string
		typ        containerpb.AutopilotCompatibilityIssue_IssueType
		subjects   string
	}
	var got []issue
	for _, i := range check() {
		got = append(got, issue{i.ConstraintType, i.IncompatibilityType, strings.Join(i.Subjects, ",")})
		if i.LastObservation.AsTime().IsZero() || i.Description == "" || !strings.HasPrefix(i.DocumentationUrl, "https://cloud.google.com/kubernetes-engine/docs/") {
			t.Errorf("issue %v", i)
		}
	}
	want := []issue{
		{"autogke-default-linux-capabilities", containerpb.AutopilotCompatibilityIssue_ADDITIONAL_CONFIG_REQUIRED, "default/netadmin"},
		{"autogke-disallow-privilege", containerpb.AutopilotCompatibilityIssue_INCOMPATIBILITY, "default/priv"},
		{"autogke-pod-resources", containerpb.AutopilotCompatibilityIssue_PASSED_WITH_OPTIONAL_CONFIG, "default/web"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("issues = %v; want %v", got, want)
	}

	// Compatibility auditing is the one workload policy a Standard
	// cluster takes.
	yes := true
	op, err = cm.UpdateCluster(ctx, &containerpb.UpdateClusterRequest{Name: name, Update: &containerpb.ClusterUpdate{
		DesiredAutopilotWorkloadPolicyConfig: &containerpb.WorkloadPolicyConfig{AutopilotCompatibilityAuditingEnabled: &yes}}})
	if err != nil {
		t.Fatalf("enable auditing: %v", err)
	}
	waitOp(t, cm, zone, op.Name)
	if c, err := cm.GetCluster(ctx, &containerpb.GetClusterRequest{Name: name}); err != nil || !c.GetAutopilot().GetWorkloadPolicyConfig().GetAutopilotCompatibilityAuditingEnabled() {
		t.Errorf("after enabling auditing: %v, %v", c.GetAutopilot(), err)
	}
	if _, err := cm.UpdateCluster(ctx, &containerpb.UpdateClusterRequest{Name: name, Update: &containerpb.ClusterUpdate{
		DesiredAutopilotWorkloadPolicyConfig: &containerpb.WorkloadPolicyConfig{AllowNetAdmin: &yes}}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("allowNetAdmin on a Standard cluster: %v; want InvalidArgument", err)
	}
	if n := len(check()); n != len(want) {
		t.Errorf("issues with auditing on: %d; want %d", n, len(want))
	}
}
