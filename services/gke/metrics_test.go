package gke

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/container/apiv1/containerpb"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/linuxuser586/gcpemu/internal/store"
)

func TestOTLPFromEnv(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  map[string]string
		ok   bool
		want otlpSettings
	}{
		{name: "unset", env: map[string]string{}},
		{name: "endpoint", env: map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://localhost:4318"},
			ok: true, want: otlpSettings{protocol: "http/protobuf", interval: time.Minute}},
		{name: "metrics endpoint, gRPC, interval", env: map[string]string{
			"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT": "http://localhost:4317", "OTEL_EXPORTER_OTLP_PROTOCOL": "grpc",
			"OTEL_METRIC_EXPORT_INTERVAL": "5000"},
			ok: true, want: otlpSettings{protocol: "grpc", interval: 5 * time.Second}},
		{name: "metrics protocol wins", env: map[string]string{
			"OTEL_EXPORTER_OTLP_ENDPOINT": "x", "OTEL_EXPORTER_OTLP_PROTOCOL": "grpc",
			"OTEL_EXPORTER_OTLP_METRICS_PROTOCOL": "http/protobuf", "OTEL_METRIC_EXPORT_INTERVAL": "bad"},
			ok: true, want: otlpSettings{protocol: "http/protobuf", interval: time.Minute}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := otlpFromEnv(func(k string) string { return tc.env[k] })
			if ok != tc.ok || got != tc.want {
				t.Errorf("otlpFromEnv = %+v, %v; want %+v, %v", got, ok, tc.want, tc.ok)
			}
		})
	}
	if _, err := newOTLPExporter(context.Background(), "http/json"); err == nil {
		t.Error("http/json: want an error")
	}
}

type recordingExporter struct {
	sdkmetric.Exporter
	mu  sync.Mutex
	got []metricdata.ResourceMetrics
}

func (e *recordingExporter) Export(_ context.Context, rm *metricdata.ResourceMetrics) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.got = append(e.got, *rm)
	return nil
}

// metricSeries is one exported resource: its attributes and values by metric name.
type metricSeries struct {
	attrs map[string]string
	vals  map[string]float64
}

func (e *recordingExporter) series() []metricSeries {
	var out []metricSeries
	for _, rm := range e.got {
		s := metricSeries{attrs: map[string]string{}, vals: map[string]float64{}}
		for _, kv := range rm.Resource.Attributes() {
			s.attrs[string(kv.Key)] = kv.Value.Emit()
		}
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				switch d := m.Data.(type) {
				case metricdata.Gauge[float64]:
					s.vals[m.Name] = d.DataPoints[0].Value
				case metricdata.Gauge[int64]:
					s.vals[m.Name] = float64(d.DataPoints[0].Value)
				}
			}
		}
		out = append(out, s)
	}
	return out
}

func findSeries(ss []metricSeries, attrs map[string]string) *metricSeries {
outer:
	for i, s := range ss {
		for k, v := range attrs {
			if s.attrs[k] != v {
				continue outer
			}
		}
		return &ss[i]
	}
	return nil
}

const (
	metricsNodes = `{"items":[{"metadata":{"name":"n1"},"status":{"allocatable":{"cpu":"2","memory":"4Gi"}}}]}`
	metricsPods  = `{"items":[
	{"metadata":{"name":"web-7d9f-abc","namespace":"shop","uid":"u1","ownerReferences":[{"kind":"ReplicaSet","name":"web-7d9f"}]},
	 "spec":{"nodeName":"n1","containers":[{"name":"app","resources":{"requests":{"cpu":"250m","memory":"64Mi"},"limits":{"cpu":"1","memory":"128Mi"}}}]},
	 "status":{"containerStatuses":[{"name":"app","restartCount":3,"lastState":{"terminated":{"reason":"OOMKilled"}}}]}},
	{"metadata":{"name":"db-0","namespace":"shop","uid":"u2","ownerReferences":[{"kind":"StatefulSet","name":"db"}]},
	 "spec":{"nodeName":"n1","containers":[{"name":"pg"}]},
	 "status":{"containerStatuses":[{"name":"pg","restartCount":0}]}},
	{"metadata":{"name":"nightly-28000-xyz","namespace":"ops","uid":"u3","ownerReferences":[{"kind":"Job","name":"nightly-28000"}]},
	 "spec":{"containers":[{"name":"run"}]},"status":{"containerStatuses":[{"name":"run","restartCount":1}]}}]}`
	metricsRS   = `{"items":[{"metadata":{"name":"web-7d9f","namespace":"shop","ownerReferences":[{"kind":"Deployment","name":"web"}]}}]}`
	metricsJobs = `{"items":[{"metadata":{"name":"nightly-28000","namespace":"ops","ownerReferences":[{"kind":"CronJob","name":"nightly"}]}}]}`
	metricsSumm = `{"node":{"nodeName":"n1","cpu":{"usageNanoCores":500000000},"memory":{"workingSetBytes":1073741824}},
	"pods":[{"podRef":{"name":"web-7d9f-abc","namespace":"shop","uid":"u1"},"cpu":{"usageNanoCores":120000000},"memory":{"workingSetBytes":50000000},
	  "containers":[{"name":"app","cpu":{"usageNanoCores":100000000},"memory":{"workingSetBytes":40000000}}]}]}`
)

func metricsFixture(t *testing.T, mc *containerpb.MonitoringConfig) (*Service, string) {
	t.Helper()
	s := newUnitService(t)
	rec := &clusterRecord{Int: clusterInternal{Project: "p", Location: "us-central1-a", Name: "c0"}}
	rec.setCluster(&containerpb.Cluster{Name: "c0", MonitoringConfig: mc})
	if err := s.env.Store.Update(func(tx store.Tx) error { return putCluster(tx, rec) }); err != nil {
		t.Fatal(err)
	}
	bodies := map[string]string{
		"/api/v1/nodes":                        metricsNodes,
		"/api/v1/pods":                         metricsPods,
		"/apis/apps/v1/replicasets":            metricsRS,
		"/apis/batch/v1/jobs":                  metricsJobs,
		"/api/v1/nodes/n1/proxy/stats/summary": metricsSumm,
	}
	kube := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, ok := bodies[r.URL.Path]
		if !ok || r.Method != http.MethodGet {
			t.Errorf("unexpected kube request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = io.WriteString(w, b)
	}))
	t.Cleanup(kube.Close)
	s.rtFor(rec.key()).kube = &kubeClient{base: kube.URL, hc: kube.Client()}
	return s, rec.key()
}

func systemComponents() *containerpb.MonitoringConfig {
	return &containerpb.MonitoringConfig{ComponentConfig: &containerpb.MonitoringComponentConfig{
		EnableComponents: []containerpb.MonitoringComponentConfig_Component{containerpb.MonitoringComponentConfig_SYSTEM_COMPONENTS},
	}}
}

// With SYSTEM_COMPONENTS enabled, one export carries node, pod and
// container CPU and memory usage, requests, limits, allocatable and
// restarts, on resources naming the cluster and the owning workload.
func TestExportSystemMetrics(t *testing.T) {
	s, key := metricsFixture(t, systemComponents())
	exp := &recordingExporter{}
	if err := s.exportSystemMetrics(context.Background(), key, exp); err != nil {
		t.Fatal(err)
	}
	ss := exp.series()
	cluster := map[string]string{"k8s.cluster.name": "c0", "cloud.provider": "gcp", "cloud.platform": "gcp_kubernetes_engine",
		"cloud.account.id": "p", "cloud.availability_zone": "us-central1-a"}

	check := func(what string, attrs map[string]string, want map[string]float64, absent ...string) {
		t.Helper()
		for k, v := range cluster {
			if _, set := attrs[k]; !set {
				attrs[k] = v
			}
		}
		s := findSeries(ss, attrs)
		if s == nil {
			t.Errorf("%s: no resource with %v; got %+v", what, attrs, ss)
			return
		}
		for name, v := range want {
			if got, ok := s.vals[name]; !ok || got != v {
				t.Errorf("%s: %s = %v (%v), want %v", what, name, got, ok, v)
			}
		}
		for _, name := range absent {
			if _, ok := s.vals[name]; ok {
				t.Errorf("%s: %s exported, want none", what, name)
			}
		}
	}
	check("node", map[string]string{"k8s.node.name": "n1"}, map[string]float64{
		"k8s.node.cpu.usage": 0.5, "k8s.node.memory.working_set": 1 << 30,
		"k8s.node.allocatable_cpu": 2, "k8s.node.allocatable_memory": 4 << 30,
	})
	check("pod", map[string]string{"k8s.pod.name": "web-7d9f-abc", "k8s.namespace.name": "shop", "k8s.pod.uid": "u1",
		"k8s.node.name": "n1", "k8s.replicaset.name": "web-7d9f", "k8s.deployment.name": "web"}, map[string]float64{
		"k8s.pod.cpu.usage": 0.12, "k8s.pod.memory.working_set": 50000000,
	})
	check("container", map[string]string{"k8s.pod.name": "web-7d9f-abc", "k8s.container.name": "app", "k8s.deployment.name": "web",
		"k8s.container.status.last_terminated_reason": "OOMKilled"}, map[string]float64{
		"container.cpu.usage": 0.1, "container.memory.working_set": 40000000,
		"k8s.container.cpu_request": 0.25, "k8s.container.cpu_limit": 1,
		"k8s.container.memory_request": 64 << 20, "k8s.container.memory_limit": 128 << 20,
		"k8s.container.restarts": 3,
	})
	check("container without stats or resources", map[string]string{"k8s.pod.name": "db-0", "k8s.container.name": "pg",
		"k8s.statefulset.name": "db"}, map[string]float64{"k8s.container.restarts": 0},
		"container.cpu.usage", "k8s.container.cpu_request", "k8s.container.memory_limit")
	check("job pod", map[string]string{"k8s.pod.name": "nightly-28000-xyz", "k8s.container.name": "run",
		"k8s.job.name": "nightly-28000", "k8s.cronjob.name": "nightly"}, map[string]float64{"k8s.container.restarts": 1})
}

// Without SYSTEM_COMPONENTS (monitoring config unset, or other components
// only) nothing is exported and the cluster is not read.
func TestExportSystemMetricsDisabled(t *testing.T) {
	for name, mc := range map[string]*containerpb.MonitoringConfig{
		"unset": nil,
		"apiserver only": {ComponentConfig: &containerpb.MonitoringComponentConfig{
			EnableComponents: []containerpb.MonitoringComponentConfig_Component{containerpb.MonitoringComponentConfig_APISERVER},
		}},
	} {
		t.Run(name, func(t *testing.T) {
			s, key := metricsFixture(t, mc)
			s.rtFor(key).kube = &kubeClient{base: "http://127.0.0.1:1", hc: http.DefaultClient}
			exp := &recordingExporter{}
			if err := s.exportSystemMetrics(context.Background(), key, exp); err != nil {
				t.Fatal(err)
			}
			if len(exp.got) != 0 {
				t.Errorf("exported %d resources, want none", len(exp.got))
			}
		})
	}
}

func TestParseQuantity(t *testing.T) {
	for in, want := range map[string]float64{
		"1": 1, "250m": 0.25, "1.5": 1.5, "100u": 0.0001, "500n": 0.0000005,
		"64Mi": 64 << 20, "1Gi": 1 << 30, "2Ki": 2048, "1G": 1e9, "512k": 512000, "1e3": 1000, "1.5E2": 150,
	} {
		got, err := parseQuantity(in)
		if err != nil || got != want {
			t.Errorf("parseQuantity(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"", "abc", "1Xi", "m"} {
		if _, err := parseQuantity(in); err == nil {
			t.Errorf("parseQuantity(%q): want an error", in)
		}
	}
}
