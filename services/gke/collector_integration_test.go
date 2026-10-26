package gke_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/container/apiv1/containerpb"
	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	"google.golang.org/protobuf/proto"

	"github.com/linuxuser586/gcpemu/emutest"
)

// signalSink is an OTLP/HTTP endpoint keeping the resource attributes of
// every logs, metrics and traces request.
type signalSink struct {
	mu       sync.Mutex
	requests int
	got      map[string][]map[string]string // signal → resources' attributes
}

func (k *signalSink) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	var (
		req       proto.Message
		resources func() []*resourcepb.Resource
	)
	switch r.URL.Path {
	case "/v1/traces":
		m := &coltracepb.ExportTraceServiceRequest{}
		req, resources = m, func() (out []*resourcepb.Resource) {
			for _, rs := range m.ResourceSpans {
				out = append(out, rs.Resource)
			}
			return out
		}
	case "/v1/metrics":
		m := &colmetricspb.ExportMetricsServiceRequest{}
		req, resources = m, func() (out []*resourcepb.Resource) {
			for _, rs := range m.ResourceMetrics {
				out = append(out, rs.Resource)
			}
			return out
		}
	case "/v1/logs":
		m := &collogspb.ExportLogsServiceRequest{}
		req, resources = m, func() (out []*resourcepb.Resource) {
			for _, rs := range m.ResourceLogs {
				out = append(out, rs.Resource)
			}
			return out
		}
	}
	if req == nil || proto.Unmarshal(b, req) != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	signal := strings.TrimPrefix(r.URL.Path, "/v1/")
	k.mu.Lock()
	k.requests++
	for _, res := range resources() {
		attrs := map[string]string{}
		for _, kv := range res.GetAttributes() {
			attrs[kv.Key] = kv.Value.GetStringValue()
		}
		k.got[signal] = append(k.got[signal], attrs)
	}
	k.mu.Unlock()
	w.Header().Set("Content-Type", "application/x-protobuf")
}

// has reports whether a resource of signal has every one of attrs.
func (k *signalSink) has(signal string, attrs map[string]string) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
outer:
	for _, got := range k.got[signal] {
		for a, v := range attrs {
			if got[a] != v {
				continue outer
			}
		}
		return true
	}
	return false
}

func (k *signalSink) count() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.requests
}

// FR-GKE-019: on a cluster with the Managed OpenTelemetry scope, a pod
// sends a trace, a metric and a log to GKE's in-cluster collector address
// over OTLP/HTTP and over OTLP/gRPC, and the Instance's OTLP endpoint
// receives each with the cluster, namespace and pod set. No collector pods
// run; with no OTLP endpoint the exports still succeed; scope NONE removes
// the Service and keeps the Instrumentation CRD. Not parallel: it sets the
// process environment.
func TestManagedCollector(t *testing.T) {
	emutest.RequireRuntime(t)
	sink := &signalSink{got: map[string][]map[string]string{}}
	srv := httptest.NewServer(sink)
	t.Cleanup(srv.Close)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", srv.URL)
	t.Setenv("OTEL_EXPORTER_OTLP_PROTOCOL", "http/protobuf")

	emutest.HoldClusterSlot(t)
	inst := emutest.Start(t, []string{"gke"})
	cm := newClient(t, inst)
	ctx := context.Background()
	scope := containerpb.ManagedOpenTelemetryConfig_COLLECTION_AND_INSTRUMENTATION_COMPONENTS
	op, err := cm.CreateCluster(ctx, &containerpb.CreateClusterRequest{Parent: parent, Cluster: &containerpb.Cluster{
		Name:                       "otel",
		ManagedOpentelemetryConfig: &containerpb.ManagedOpenTelemetryConfig{Scope: &scope},
		NodePools:                  []*containerpb.NodePool{{Name: "np", InitialNodeCount: 1}},
	}})
	if err != nil {
		t.Fatalf("CreateCluster: %v", err)
	}
	image := buildTestApp(t, inst, project, "otlpapp") // while the cluster comes up
	waitOp(t, cm, zone, op.Name)
	name := parent + "/clusters/otel"
	c, err := cm.GetCluster(ctx, &containerpb.GetClusterRequest{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	adm, pool := adminClient(t, inst, name)
	e := &env{t: t, inst: inst, cm: cm, c: c, ca: pool, adm: adm}

	const (
		crd = "/apis/apiextensions.k8s.io/v1/customresourcedefinitions/instrumentations.telemetry.googleapis.com"
		svc = "/api/v1/namespaces/gke-managed-otel/services/opentelemetry-collector"
	)
	if code, body := e.kube("GET", crd, "", nil, nil); code != http.StatusOK {
		t.Fatalf("GET Instrumentation CRD = %d: %s", code, body)
	}
	if code, body := e.kube("GET", svc, "", nil, nil); code != http.StatusOK {
		t.Fatalf("GET collector Service = %d: %s", code, body)
	}

	// send runs otlpapp as pod over protocol and fails unless it succeeds.
	send := func(pod, protocol string) {
		t.Helper()
		e.mustKube("POST", "/api/v1/namespaces/default/pods", map[string]any{
			"apiVersion": "v1", "kind": "Pod",
			"metadata": map[string]any{"name": pod, "namespace": "default"},
			"spec": map[string]any{
				"restartPolicy": "Never",
				"containers": []map[string]any{{
					"name": "app", "image": image,
					"env": []map[string]string{{"name": "PROTOCOL", "value": protocol}},
				}},
			},
		}, nil)
		var phase string
		deadline := time.Now().Add(3 * time.Minute)
		for phase != "Succeeded" && phase != "Failed" && time.Now().Before(deadline) {
			time.Sleep(time.Second)
			var p struct{ Status struct{ Phase string } }
			e.mustKube("GET", "/api/v1/namespaces/default/pods/"+pod, nil, &p)
			phase = p.Status.Phase
		}
		_, logs := e.kube("GET", "/api/v1/namespaces/default/pods/"+pod+"/log?limitBytes=8192", "", nil, nil)
		if phase != "Succeeded" || !strings.Contains(logs, "OTLPAPP OK") {
			var p map[string]any
			e.kube("GET", "/api/v1/namespaces/default/pods/"+pod, "", nil, &p)
			t.Fatalf("pod %s: phase %q\nlogs:\n%s\nstatus: %v", pod, phase, logs, p["status"])
		}
	}

	for _, protocol := range []string{"http", "grpc"} {
		t.Run(strings.ToUpper(protocol), func(t *testing.T) {
			pod := "otlp-" + protocol
			send(pod, protocol)
			for _, signal := range []string{"traces", "metrics", "logs"} {
				want := map[string]string{"service.name": "otlpapp-" + protocol, "k8s.cluster.name": "otel",
					"k8s.namespace.name": "default", "k8s.pod.name": pod, "k8s.container.name": "app"}
				if !sink.has(signal, want) {
					sink.mu.Lock()
					t.Errorf("%s: no resource with %v; got %v", signal, want, sink.got[signal])
					sink.mu.Unlock()
				}
			}
		})
	}

	var pods struct{ Items []any }
	e.mustKube("GET", "/api/v1/namespaces/gke-managed-otel/pods", nil, &pods)
	if len(pods.Items) != 0 {
		t.Errorf("%d pods in gke-managed-otel, want none", len(pods.Items))
	}

	t.Run("NoEndpoint", func(t *testing.T) {
		os.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "") // t.Setenv restores it
		before := sink.count()
		send("otlp-dropped", "http")
		if n := sink.count() - before; n != 0 {
			t.Errorf("%d requests reached the endpoint, want none", n)
		}
	})

	none := containerpb.ManagedOpenTelemetryConfig_NONE
	op, err = cm.UpdateCluster(ctx, &containerpb.UpdateClusterRequest{Name: name, Update: &containerpb.ClusterUpdate{
		DesiredManagedOpentelemetryConfig: &containerpb.ManagedOpenTelemetryConfig{Scope: &none},
	}})
	if err != nil {
		t.Fatalf("UpdateCluster: %v", err)
	}
	waitOp(t, cm, zone, op.Name)
	if code, _ := e.kube("GET", svc, "", nil, nil); code != http.StatusNotFound {
		t.Errorf("collector Service after NONE: GET = %d, want 404", code)
	}
	if code, body := e.kube("GET", crd, "", nil, nil); code != http.StatusOK {
		t.Errorf("Instrumentation CRD after NONE: GET = %d: %s", code, body)
	}
}
