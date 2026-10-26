package gke_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"cloud.google.com/go/container/apiv1/containerpb"

	"github.com/linuxuser586/gcpemu/emutest"
	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/runtime"
	"github.com/linuxuser586/gcpemu/services/gke"
)

// TestResourceMetrics is #637: every cluster serves metrics.k8s.io from
// k3s's bundled metrics-server, as GKE's does. The cluster is private and,
// until the restart, the emulator offline, so its nodes can pull neither
// from upstream nor through the registry mirror, whose cache is new:
// metrics-server starts only from the preloaded images. `kubectl top
// nodes` and `kubectl top pods -A` read what the test checks, and a CPU
// HPA scales on it. Removing metrics-server, as a cluster created before
// #637 never had it, brings it back when the Instance next starts and
// recreates the server container.
func TestResourceMetrics(t *testing.T) {
	emutest.RequireRuntime(t)
	t.Parallel()
	emutest.HoldClusterSlot(t)
	cl, err := runtime.Detect()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, img := range gke.DefaultVersionImages() {
		if err := cl.EnsureImage(ctx, img, false); err != nil {
			t.Fatal(err)
		}
	}
	dir := t.TempDir()
	start := func() *emutest.Instance {
		t.Helper()
		ctx, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()
		inst, err := emutest.New(ctx, []string{"gke"}, emutest.WithDataDir(dir),
			emutest.WithConfig(func(c *config.Config) { c.Instance = "gke-resmetrics" }))
		if err != nil {
			t.Fatal(err)
		}
		return inst
	}
	inst := start()
	id := inst.Internal().ID
	t.Cleanup(func() {
		// The instance is not ephemeral: remove what it leaves behind.
		m := &runtime.Manager{Client: cl, InstanceID: id}
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_ = m.Cleanup(ctx, true)
	})
	stopped := false
	defer func() {
		if !stopped {
			_ = inst.Close(context.Background())
		}
	}()
	inst.Internal().Env.Config.Offline = true

	cm := newClient(t, inst)
	op, err := cm.CreateCluster(ctx, &containerpb.CreateClusterRequest{Parent: parent, Cluster: &containerpb.Cluster{
		Name:                 "top",
		PrivateClusterConfig: &containerpb.PrivateClusterConfig{EnablePrivateNodes: true, MasterIpv4CidrBlock: "172.16.0.0/28"},
		NodePools:            []*containerpb.NodePool{{Name: "np", InitialNodeCount: 2}},
	}})
	if err != nil {
		t.Fatalf("CreateCluster: %v", err)
	}
	waitOp(t, cm, zone, op.Name)
	name := parent + "/clusters/top"
	c, err := cm.GetCluster(ctx, &containerpb.GetClusterRequest{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	adm, pool := adminClient(t, inst, name)
	e := &env{t: t, inst: inst, cm: cm, c: c, ca: pool, adm: adm}

	e.nodeMetrics(2)
	e.mustKube("POST", "/apis/apps/v1/namespaces/default/deployments", map[string]any{
		"apiVersion": "apps/v1", "kind": "Deployment",
		"metadata": map[string]any{"name": "busy"},
		"spec": map[string]any{
			"replicas": 1,
			"selector": map[string]any{"matchLabels": map[string]string{"app": "busy"}},
			"template": map[string]any{
				"metadata": map[string]any{"labels": map[string]string{"app": "busy"}},
				"spec": map[string]any{"containers": []map[string]any{{
					"name": "c", "image": testImage, "imagePullPolicy": "IfNotPresent",
					"command":   []string{"sleep", "3600"},
					"resources": map[string]any{"requests": map[string]string{"cpu": "50m"}},
				}}},
			},
		},
	}, nil)
	e.mustKube("POST", "/apis/autoscaling/v2/namespaces/default/horizontalpodautoscalers", map[string]any{
		"apiVersion": "autoscaling/v2", "kind": "HorizontalPodAutoscaler",
		"metadata": map[string]any{"name": "busy"},
		"spec": map[string]any{
			"scaleTargetRef": map[string]string{"apiVersion": "apps/v1", "kind": "Deployment", "name": "busy"},
			"minReplicas":    1, "maxReplicas": 2,
			"metrics": []map[string]any{{"type": "Resource", "resource": map[string]any{
				"name": "cpu", "target": map[string]any{"type": "Utilization", "averageUtilization": 80},
			}}},
		},
	}, nil)
	e.podMetrics()
	e.hpaActive("default", "busy")

	// A cluster created before #637: the server ran with --disable
	// metrics-server, so k3s removed its Addon and objects. While the server
	// runs, k3s re-reads its manifests only when they change.
	for _, p := range []string{
		"/apis/k3s.cattle.io/v1/namespaces/kube-system/addons/metrics-server",
		"/apis/apiregistration.k8s.io/v1/apiservices/v1beta1.metrics.k8s.io",
		"/apis/apps/v1/namespaces/kube-system/deployments/metrics-server",
		"/api/v1/namespaces/kube-system/services/metrics-server",
	} {
		if code, b := e.kube("DELETE", p, "", nil, nil); code/100 != 2 && code != 404 {
			t.Fatalf("DELETE %s: %d %s", p, code, b)
		}
	}
	gone := func() bool {
		code, _ := e.kube("GET", "/apis/metrics.k8s.io/v1beta1/nodes", "", nil, nil)
		return code/100 != 2
	}
	eventually(t, time.Minute, "metrics.k8s.io gone", gone)
	// Past k3s's 15s rescan of its manifests, it is still gone.
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(2 * time.Second) {
		if !gone() {
			t.Fatal("metrics.k8s.io came back before the server restarted")
		}
	}

	cctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	if err := inst.Close(cctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	stopped = true
	inst = start()
	defer inst.Close(context.Background())
	c = waitRunning(t, inst, name)
	adm, pool = adminClient(t, inst, name)
	e = &env{t: t, inst: inst, c: c, ca: pool, adm: adm}
	e.nodeMetrics(2)
}

// nodeMetrics waits for nodes.metrics.k8s.io to report CPU and memory
// usage for each of the cluster's n nodes.
func (e *env) nodeMetrics(n int) {
	e.t.Helper()
	var got []string
	deadline := time.Now().Add(4 * time.Minute)
	for {
		var nodes, metrics struct {
			Items []struct {
				Metadata struct{ Name string }
				Usage    struct{ CPU, Memory string }
			}
		}
		e.mustKube("GET", "/api/v1/nodes", nil, &nodes)
		code, b := e.kube("GET", "/apis/metrics.k8s.io/v1beta1/nodes", "", nil, &metrics)
		has := map[string]bool{}
		for _, m := range metrics.Items {
			has[m.Metadata.Name] = m.Usage.CPU != "" && m.Usage.Memory != ""
		}
		got = got[:0]
		for _, nd := range nodes.Items {
			if has[nd.Metadata.Name] {
				got = append(got, nd.Metadata.Name)
			}
		}
		if code == 200 && len(nodes.Items) == n && len(got) == n {
			return
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("nodes.metrics.k8s.io after 4m: %d nodes, metrics for %v: %d %s\n%s",
				len(nodes.Items), got, code, b, e.metricsServerDiagnostics())
		}
		time.Sleep(2 * time.Second)
	}
}

// podMetrics waits for pods.metrics.k8s.io to report containers for every
// running pod in the cluster.
func (e *env) podMetrics() {
	e.t.Helper()
	var missing []string
	deadline := time.Now().Add(3 * time.Minute)
	for {
		var pods struct {
			Items []struct {
				Metadata struct{ Namespace, Name string }
				Status   struct{ Phase string }
			}
		}
		var metrics struct {
			Items []struct {
				Metadata   struct{ Namespace, Name string }
				Containers []struct{ Name string }
			}
		}
		e.mustKube("GET", "/api/v1/pods", nil, &pods)
		code, b := e.kube("GET", "/apis/metrics.k8s.io/v1beta1/pods", "", nil, &metrics)
		has := map[string]bool{}
		for _, m := range metrics.Items {
			has[m.Metadata.Namespace+"/"+m.Metadata.Name] = len(m.Containers) > 0
		}
		missing = missing[:0]
		for _, p := range pods.Items {
			if k := p.Metadata.Namespace + "/" + p.Metadata.Name; p.Status.Phase == "Running" && !has[k] {
				missing = append(missing, k)
			}
		}
		if code == 200 && len(missing) == 0 {
			return
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("pods.metrics.k8s.io after 3m: no metrics for running pods %v: %d %s\n%s", missing, code, b, e.metricsServerDiagnostics())
		}
		time.Sleep(2 * time.Second)
	}
}

// hpaActive waits for HPA ns/name to report its current CPU utilization
// and the condition ScalingActive=True.
func (e *env) hpaActive(ns, name string) {
	e.t.Helper()
	var last string
	deadline := time.Now().Add(3 * time.Minute)
	for {
		var hpa struct {
			Status struct {
				CurrentMetrics []struct {
					Type     string
					Resource struct {
						Name    string
						Current struct{ AverageUtilization *int }
					}
				}
				Conditions []struct{ Type, Status, Reason, Message string }
			}
		}
		_, last = e.kube("GET", "/apis/autoscaling/v2/namespaces/"+ns+"/horizontalpodautoscalers/"+name, "", nil, &hpa)
		cpu, active := false, false
		for _, m := range hpa.Status.CurrentMetrics {
			cpu = cpu || m.Type == "Resource" && m.Resource.Name == "cpu" && m.Resource.Current.AverageUtilization != nil
		}
		for _, c := range hpa.Status.Conditions {
			active = active || c.Type == "ScalingActive" && c.Status == "True"
		}
		if cpu && active {
			return
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("HPA %s/%s after 3m has no current CPU or is not ScalingActive: %s", ns, name, last)
		}
		time.Sleep(2 * time.Second)
	}
}

// metricsServerDiagnostics describes metrics-server's APIService
// conditions and its pods' container states and logs, for failures such as
// a kubelet's /metrics/resource missing node metrics (k3s-io/k3s#14554).
func (e *env) metricsServerDiagnostics() string {
	var svc struct {
		Status struct {
			Conditions []struct{ Type, Status, Reason, Message string }
		}
	}
	e.kube("GET", "/apis/apiregistration.k8s.io/v1/apiservices/v1beta1.metrics.k8s.io", "", nil, &svc)
	var pods struct {
		Items []struct {
			Metadata struct{ Name string }
			Status   struct {
				Phase             string
				ContainerStatuses []struct{ State map[string]any }
			}
		}
	}
	e.kube("GET", "/api/v1/namespaces/kube-system/pods?labelSelector=k8s-app%3Dmetrics-server", "", nil, &pods)
	out := fmt.Sprintf("APIService conditions: %+v", svc.Status.Conditions)
	for _, p := range pods.Items {
		_, l := e.kube("GET", "/api/v1/namespaces/kube-system/pods/"+p.Metadata.Name+"/log?tailLines=40", "", nil, nil)
		out += fmt.Sprintf("\n--- pod %s %s %+v, log:\n%s", p.Metadata.Name, p.Status.Phase, p.Status.ContainerStatuses, l)
	}
	return out
}
