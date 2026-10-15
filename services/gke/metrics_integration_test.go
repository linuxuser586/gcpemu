package gke_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/container/apiv1/containerpb"
	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	"google.golang.org/protobuf/proto"

	"github.com/linuxuser586/gcpemu/emutest"
)

// otlpSink is an OTLP/HTTP metrics receiver that keeps, per resource
// (as its attributes), the metric names last received.
type otlpSink struct {
	mu       sync.Mutex
	requests int
	got      []otlpResource
}

type otlpResource struct {
	attrs   map[string]string
	metrics map[string]float64
}

func (k *otlpSink) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	var req colmetricspb.ExportMetricsServiceRequest
	if r.URL.Path != "/v1/metrics" || proto.Unmarshal(b, &req) != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	k.mu.Lock()
	k.requests++
	for _, rm := range req.ResourceMetrics {
		res := otlpResource{attrs: map[string]string{}, metrics: map[string]float64{}}
		for _, kv := range rm.Resource.GetAttributes() {
			res.attrs[kv.Key] = kv.Value.GetStringValue()
		}
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				for _, dp := range m.GetGauge().GetDataPoints() {
					res.metrics[m.Name] = dp.GetAsDouble() + float64(dp.GetAsInt())
				}
			}
		}
		k.got = append(k.got, res)
	}
	k.mu.Unlock()
	out, _ := proto.Marshal(&colmetricspb.ExportMetricsServiceResponse{})
	w.Header().Set("Content-Type", "application/x-protobuf")
	_, _ = w.Write(out)
}

// find returns the last resource received with attrs and metric.
func (k *otlpSink) find(attrs map[string]string, metric string) (otlpResource, bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
outer:
	for i := len(k.got) - 1; i >= 0; i-- {
		r := k.got[i]
		for a, v := range attrs {
			if r.attrs[a] != v {
				continue outer
			}
		}
		if _, ok := r.metrics[metric]; ok {
			return r, true
		}
	}
	return otlpResource{}, false
}

func (k *otlpSink) count() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.requests
}

// FR-GKE-014: a cluster whose monitoringConfig enables SYSTEM_COMPONENTS exports its
// node, pod and container usage over OTLP to the endpoint of the
// emulator's OTEL_EXPORTER_OTLP_ENDPOINT; a container killed for
// exceeding its memory limit shows as a restart with reason OOMKilled, and
// disabling the component stops the export. Not parallel: it sets the
// process environment.
func TestSystemMetricsOTLP(t *testing.T) {
	emutest.RequireRuntime(t)
	sink := &otlpSink{}
	srv := httptest.NewServer(sink)
	t.Cleanup(srv.Close)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", srv.URL)
	t.Setenv("OTEL_EXPORTER_OTLP_PROTOCOL", "http/protobuf")
	t.Setenv("OTEL_METRIC_EXPORT_INTERVAL", "1000")

	inst := emutest.Start(t, []string{"gke"})
	cm := newClient(t, inst)
	ctx := context.Background()
	op, err := cm.CreateCluster(ctx, &containerpb.CreateClusterRequest{Parent: parent, Cluster: &containerpb.Cluster{
		Name: "metrics",
		MonitoringConfig: &containerpb.MonitoringConfig{ComponentConfig: &containerpb.MonitoringComponentConfig{
			EnableComponents: []containerpb.MonitoringComponentConfig_Component{containerpb.MonitoringComponentConfig_SYSTEM_COMPONENTS},
		}},
		NodePools: []*containerpb.NodePool{{Name: "np", InitialNodeCount: 1}},
	}})
	if err != nil {
		t.Fatalf("CreateCluster: %v", err)
	}
	waitOp(t, cm, zone, op.Name, 3*time.Minute)
	name := parent + "/clusters/metrics"
	c, err := cm.GetCluster(ctx, &containerpb.GetClusterRequest{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	adm, pool := adminClient(t, inst, name)
	e := &env{t: t, inst: inst, cm: cm, c: c, ca: pool, adm: adm}

	eventually(t, 2*time.Minute, "node CPU usage", func() bool {
		_, ok := sink.find(map[string]string{"k8s.cluster.name": "metrics"}, "k8s.node.cpu.usage")
		return ok
	})

	// tail buffers /dev/zero, a single endless line, until the limit kills it.
	e.mustKube("POST", "/api/v1/namespaces/default/pods", map[string]any{
		"apiVersion": "v1", "kind": "Pod",
		"metadata": map[string]any{"name": "oom"},
		"spec": map[string]any{"containers": []map[string]any{{
			"name": "hog", "image": testImage, "imagePullPolicy": "IfNotPresent",
			"command":   []string{"sh", "-c", "sleep 2; tail /dev/zero"},
			"resources": map[string]any{"requests": map[string]string{"memory": "16Mi", "cpu": "50m"}, "limits": map[string]string{"memory": "32Mi"}},
		}}},
	}, nil)
	hog := map[string]string{"k8s.cluster.name": "metrics", "k8s.namespace.name": "default", "k8s.pod.name": "oom", "k8s.container.name": "hog"}
	eventually(t, 3*time.Minute, "an OOMKilled restart", func() bool {
		r, ok := sink.find(hog, "k8s.container.restarts")
		return ok && r.metrics["k8s.container.restarts"] >= 1 && r.attrs["k8s.container.status.last_terminated_reason"] == "OOMKilled"
	})
	r, _ := sink.find(hog, "k8s.container.restarts")
	if r.metrics["k8s.container.memory_limit"] != 32<<20 || r.metrics["k8s.container.cpu_request"] != 0.05 {
		t.Errorf("hog limits and requests: %v", r.metrics)
	}

	op, err = cm.UpdateCluster(ctx, &containerpb.UpdateClusterRequest{Name: name, Update: &containerpb.ClusterUpdate{
		DesiredMonitoringConfig: &containerpb.MonitoringConfig{ComponentConfig: &containerpb.MonitoringComponentConfig{}},
	}})
	if err != nil {
		t.Fatalf("UpdateCluster: %v", err)
	}
	waitOp(t, cm, zone, op.Name, time.Minute)
	time.Sleep(3 * time.Second) // an export in flight
	before := sink.count()
	time.Sleep(4 * time.Second)
	if n := sink.count() - before; n != 0 {
		t.Errorf("%d exports after disabling SYSTEM_COMPONENTS, want none", n)
	}
}
