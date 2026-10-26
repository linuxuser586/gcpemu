package gke_test

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"cloud.google.com/go/container/apiv1/containerpb"

	"github.com/linuxuser586/gcpemu/emutest"
)

// promServer serves a Prometheus counter on port 8080 with busybox nc.
const promServer = `while true; do printf 'HTTP/1.0 200 OK\r\nContent-Type: text/plain\r\nConnection: close\r\n\r\n# TYPE demo_requests_total counter\ndemo_requests_total 42\n' | nc -l -p 8080 -w 1 >/dev/null; done`

// FR-GKE-020: with managed Prometheus enabled the cluster has GMP's CRDs;
// a PodMonitoring's pod and a ClusterPodMonitoring's pods in any namespace
// are scraped into the OTLP endpoint of the emulator's
// OTEL_EXPORTER_OTLP_ENDPOINT within two intervals, with namespace and pod
// attributes; disabling managed Prometheus stops the exports and keeps the
// CRDs. Not parallel: it sets the process environment.
func TestManagedPrometheusOTLP(t *testing.T) {
	emutest.RequireRuntime(t)
	sink := &otlpSink{}
	srv := httptest.NewServer(sink)
	t.Cleanup(srv.Close)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", srv.URL)
	t.Setenv("OTEL_EXPORTER_OTLP_PROTOCOL", "http/protobuf")

	emutest.HoldClusterSlot(t)
	inst := emutest.Start(t, []string{"gke"})
	cm := newClient(t, inst)
	ctx := context.Background()
	op, err := cm.CreateCluster(ctx, &containerpb.CreateClusterRequest{Parent: parent, Cluster: &containerpb.Cluster{
		Name: "prom",
		MonitoringConfig: &containerpb.MonitoringConfig{
			ManagedPrometheusConfig: &containerpb.ManagedPrometheusConfig{Enabled: true},
		},
		NodePools: []*containerpb.NodePool{{Name: "np", InitialNodeCount: 1}},
	}})
	if err != nil {
		t.Fatalf("CreateCluster: %v", err)
	}
	waitOp(t, cm, zone, op.Name)
	name := parent + "/clusters/prom"
	c, err := cm.GetCluster(ctx, &containerpb.GetClusterRequest{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	adm, pool := adminClient(t, inst, name)
	e := &env{t: t, inst: inst, cm: cm, c: c, ca: pool, adm: adm}
	crds := []string{"podmonitorings.monitoring.googleapis.com", "clusterpodmonitorings.monitoring.googleapis.com"}
	for _, crd := range crds {
		e.mustKube("GET", "/apis/apiextensions.k8s.io/v1/customresourcedefinitions/"+crd, nil, nil)
	}

	e.mustKube("POST", "/api/v1/namespaces", map[string]any{"metadata": map[string]any{"name": "other"}}, nil)
	for _, ns := range []string{"default", "other"} {
		e.mustKube("POST", "/api/v1/namespaces/"+ns+"/pods", map[string]any{
			"metadata": map[string]any{"name": "app", "labels": map[string]string{"app": "demo"}},
			"spec": map[string]any{"containers": []map[string]any{{
				"name": "srv", "image": testImage, "imagePullPolicy": "IfNotPresent",
				"command": []string{"sh", "-c", promServer},
				"ports":   []map[string]any{{"name": "metrics", "containerPort": 8080}},
			}}},
		}, nil)
	}
	for _, ns := range []string{"default", "other"} {
		eventually(t, 2*time.Minute, ns+"/app running", func() bool {
			var p struct {
				Status struct {
					Phase string `json:"phase"`
				} `json:"status"`
			}
			e.mustKube("GET", "/api/v1/namespaces/"+ns+"/pods/app", nil, &p)
			return p.Status.Phase == "Running"
		})
	}

	const interval = 10 * time.Second
	e.mustKube("POST", "/apis/monitoring.googleapis.com/v1/namespaces/default/podmonitorings", map[string]any{
		"apiVersion": "monitoring.googleapis.com/v1", "kind": "PodMonitoring",
		"metadata": map[string]any{"name": "demo"},
		"spec": map[string]any{
			"selector":  map[string]any{"matchLabels": map[string]string{"app": "demo"}},
			"endpoints": []map[string]any{{"port": "metrics", "interval": "10s", "timeout": "2s"}},
		},
	}, nil)
	counter := func(job, ns string) func() bool {
		return func() bool {
			r, ok := sink.find(map[string]string{"k8s.cluster.name": "prom", "service.name": job,
				"k8s.namespace.name": ns, "k8s.pod.name": "app"}, "demo_requests_total")
			return ok && r.metrics["demo_requests_total"] == 42
		}
	}
	eventually(t, 2*interval, "the PodMonitoring's counter", counter("demo", "default"))
	if _, ok := sink.find(map[string]string{"service.name": "demo", "k8s.namespace.name": "other"}, "demo_requests_total"); ok {
		t.Error("the PodMonitoring in default scraped a pod in other")
	}
	eventually(t, 30*time.Second, "the PodMonitoring's status", func() bool {
		var pm struct {
			Status struct {
				Conditions []struct {
					Type, Status string
				} `json:"conditions"`
			} `json:"status"`
		}
		e.mustKube("GET", "/apis/monitoring.googleapis.com/v1/namespaces/default/podmonitorings/demo", nil, &pm)
		return len(pm.Status.Conditions) == 1 && pm.Status.Conditions[0].Type == "ConfigurationCreateSuccess" && pm.Status.Conditions[0].Status == "True"
	})

	e.mustKube("POST", "/apis/monitoring.googleapis.com/v1/clusterpodmonitorings", map[string]any{
		"apiVersion": "monitoring.googleapis.com/v1", "kind": "ClusterPodMonitoring",
		"metadata": map[string]any{"name": "demo-all"},
		"spec": map[string]any{
			"selector":  map[string]any{"matchLabels": map[string]string{"app": "demo"}},
			"endpoints": []map[string]any{{"port": "metrics", "interval": "10s", "timeout": "2s"}},
		},
	}, nil)
	eventually(t, 2*interval, "the ClusterPodMonitoring's counter in other", counter("demo-all", "other"))
	eventually(t, 2*interval, "the ClusterPodMonitoring's counter in default", counter("demo-all", "default"))

	op, err = cm.UpdateCluster(ctx, &containerpb.UpdateClusterRequest{Name: name, Update: &containerpb.ClusterUpdate{
		DesiredMonitoringConfig: &containerpb.MonitoringConfig{ManagedPrometheusConfig: &containerpb.ManagedPrometheusConfig{}},
	}})
	if err != nil {
		t.Fatalf("UpdateCluster: %v", err)
	}
	waitOp(t, cm, zone, op.Name)
	time.Sleep(3 * time.Second) // a scrape in flight, which times out after 2s
	before := sink.count()
	time.Sleep(interval + 3*time.Second)
	if n := sink.count() - before; n != 0 {
		t.Errorf("%d exports after disabling managed Prometheus, want none", n)
	}
	for _, crd := range crds {
		e.mustKube("GET", "/apis/apiextensions.k8s.io/v1/customresourcedefinitions/"+crd, nil, nil)
	}
	e.mustKube("GET", "/apis/monitoring.googleapis.com/v1/namespaces/default/podmonitorings/demo", nil, nil)
}
