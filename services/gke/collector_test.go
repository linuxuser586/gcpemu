package gke

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/container/apiv1/containerpb"
	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	grpcstatus "google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/linuxuser586/gcpemu/internal/store"
)

// The managed collector forwards each signal where the standard OTel env
// vars say, a signal's own variables winning over the general ones.
func TestOTLPTargetFromEnv(t *testing.T) {
	for _, tc := range []struct {
		name   string
		signal string
		env    map[string]string
		ok     bool
		want   otlpTarget
	}{
		{name: "unset", signal: "traces", env: map[string]string{}},
		{name: "empty endpoint", signal: "traces", env: map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": ""}},
		{name: "general endpoint gets the signal path", signal: "traces",
			env: map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://otel:4318/"},
			ok:  true, want: otlpTarget{protocol: "http/protobuf", endpoint: "http://otel:4318/v1/traces", timeout: 10 * time.Second}},
		{name: "signal endpoint is used as is", signal: "logs", env: map[string]string{
			"OTEL_EXPORTER_OTLP_ENDPOINT": "http://otel:4318", "OTEL_EXPORTER_OTLP_LOGS_ENDPOINT": "http://logs:9000/ingest"},
			ok: true, want: otlpTarget{protocol: "http/protobuf", endpoint: "http://logs:9000/ingest", timeout: 10 * time.Second}},
		{name: "another signal's endpoint does not count", signal: "metrics",
			env: map[string]string{"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": "http://otel:4318/v1/traces"}},
		{name: "gRPC over plain HTTP", signal: "metrics", env: map[string]string{
			"OTEL_EXPORTER_OTLP_ENDPOINT": "http://otel:4317", "OTEL_EXPORTER_OTLP_PROTOCOL": "grpc"},
			ok: true, want: otlpTarget{protocol: "grpc", endpoint: "otel:4317", insecure: true, timeout: 10 * time.Second}},
		{name: "gRPC over TLS, signal protocol wins", signal: "traces", env: map[string]string{
			"OTEL_EXPORTER_OTLP_ENDPOINT": "https://otel:4317", "OTEL_EXPORTER_OTLP_PROTOCOL": "http/protobuf",
			"OTEL_EXPORTER_OTLP_TRACES_PROTOCOL": "grpc"},
			ok: true, want: otlpTarget{protocol: "grpc", endpoint: "otel:4317", timeout: 10 * time.Second}},
		{name: "gRPC without a scheme follows INSECURE", signal: "logs", env: map[string]string{
			"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT": "otel:4317", "OTEL_EXPORTER_OTLP_PROTOCOL": "grpc",
			"OTEL_EXPORTER_OTLP_INSECURE": "false", "OTEL_EXPORTER_OTLP_LOGS_INSECURE": "true"},
			ok: true, want: otlpTarget{protocol: "grpc", endpoint: "otel:4317", insecure: true, timeout: 10 * time.Second}},
		{name: "headers, timeout, compression and certificate", signal: "metrics", env: map[string]string{
			"OTEL_EXPORTER_OTLP_ENDPOINT":         "http://otel:4318",
			"OTEL_EXPORTER_OTLP_HEADERS":          "x-general=1",
			"OTEL_EXPORTER_OTLP_METRICS_HEADERS":  "api-key=a%20b, x-tenant = t1",
			"OTEL_EXPORTER_OTLP_TIMEOUT":          "2500",
			"OTEL_EXPORTER_OTLP_COMPRESSION":      "gzip",
			"OTEL_EXPORTER_OTLP_CERTIFICATE":      "/etc/ca.pem",
			"OTEL_EXPORTER_OTLP_METRICS_TIMEOUT":  "bad",
			"OTEL_EXPORTER_OTLP_LOGS_COMPRESSION": "none"},
			ok: true, want: otlpTarget{protocol: "http/protobuf", endpoint: "http://otel:4318/v1/metrics",
				headers: map[string]string{"api-key": "a b", "x-tenant": "t1"}, timeout: 2500 * time.Millisecond,
				compression: "gzip", certificate: "/etc/ca.pem"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := otlpTargetFromEnv(func(k string) string { return tc.env[k] }, tc.signal)
			if ok != tc.ok || !reflect.DeepEqual(got, tc.want) {
				t.Errorf("otlpTargetFromEnv(%s) = %+v, %v; want %+v, %v", tc.signal, got, ok, tc.want, tc.ok)
			}
		})
	}
}

// otlpHTTPSink is an OTLP/HTTP backend that keeps each request body by
// path.
type otlpHTTPSink struct {
	mu     sync.Mutex
	bodies map[string][][]byte
	header http.Header // of the last request
	status int         // to answer with, 200 if 0
}

func newOTLPHTTPSink(t *testing.T) (*otlpHTTPSink, string) {
	k := &otlpHTTPSink{bodies: map[string][][]byte{}}
	srv := httptest.NewServer(k)
	t.Cleanup(srv.Close)
	return k, srv.URL
}

func (k *otlpHTTPSink) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	k.mu.Lock()
	defer k.mu.Unlock()
	k.bodies[r.URL.Path] = append(k.bodies[r.URL.Path], b)
	k.header = r.Header.Clone()
	if k.status != 0 {
		w.WriteHeader(k.status)
		return
	}
	w.Header().Set("Content-Type", "application/x-protobuf")
}

// last decodes the last body received at path into m, false if none.
func (k *otlpHTTPSink) last(t *testing.T, path string, m proto.Message) bool {
	t.Helper()
	k.mu.Lock()
	defer k.mu.Unlock()
	bs := k.bodies[path]
	if len(bs) == 0 {
		return false
	}
	if err := proto.Unmarshal(bs[len(bs)-1], m); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return true
}

func (k *otlpHTTPSink) count() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	n := 0
	for _, bs := range k.bodies {
		n += len(bs)
	}
	return n
}

// collectorFixture is a cluster c0 with the managed collector's scope
// (scope), one pod web-7d9f-abc at 10.4.0.7 owned by Deployment web, and
// the URL base of the receiver for that pod.
func collectorFixture(t *testing.T, scope containerpb.ManagedOpenTelemetryConfig_Scope) (http.Handler, string) {
	t.Helper()
	h, base, _ := collectorFixtureKube(t, scope)
	return h, base
}

// collectorFixtureKube is collectorFixture with the cluster's API server.
func collectorFixtureKube(t *testing.T, scope containerpb.ManagedOpenTelemetryConfig_Scope) (http.Handler, string, *fakeKube) {
	t.Helper()
	s := newUnitService(t)
	rec := &clusterRecord{Int: clusterInternal{Project: "p", Location: "us-central1-a", Name: "c0", Secret: "s3cret", PodCIDR: "10.4.0.0/14"}}
	rec.setCluster(&containerpb.Cluster{Name: "c0", Id: "cid", ManagedOpentelemetryConfig: &containerpb.ManagedOpenTelemetryConfig{Scope: &scope}})
	if err := s.env.Store.Update(func(tx store.Tx) error { return putCluster(tx, rec) }); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.byID["cid"] = rec.key()
	s.mu.Unlock()
	k, kc := newFakeKube(t)
	k.put(`{"apiVersion":"v1","kind":"Pod","metadata":{"name":"other","namespace":"shop","uid":"u0"},
		"spec":{"nodeName":"n1","containers":[{"name":"x"}]},"status":{"phase":"Running","podIP":"10.4.0.9"}}`)
	k.put(`{"apiVersion":"v1","kind":"Pod","metadata":{"name":"web-7d9f-abc","namespace":"shop","uid":"u1",
		"ownerReferences":[{"kind":"ReplicaSet","name":"web-7d9f"}]},
		"spec":{"nodeName":"n1","containers":[{"name":"app"}]},"status":{"phase":"Running","podIP":"10.4.0.7"}}`)
	k.put(`{"apiVersion":"apps/v1","kind":"ReplicaSet","metadata":{"name":"web-7d9f","namespace":"shop",
		"ownerReferences":[{"kind":"Deployment","name":"web"}]}}`)
	s.rtFor(rec.key()).kube = kc
	return s.internalHandler(), "/_emu/node/cid/s3cret/n1/otlp/10.4.0.7", k
}

func strAttr(k, v string) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: v}}}
}

// attrMap returns a resource's string attributes, failing on a key set
// twice.
func attrMap(t *testing.T, r *resourcepb.Resource) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, kv := range r.GetAttributes() {
		if _, dup := out[kv.Key]; dup {
			t.Errorf("attribute %s set twice", kv.Key)
		}
		out[kv.Key] = kv.Value.GetStringValue()
	}
	return out
}

func postOTLP(t *testing.T, h http.Handler, path, contentType string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	r.Header.Set("Content-Type", contentType)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func traceRequest(attrs ...*commonpb.KeyValue) *coltracepb.ExportTraceServiceRequest {
	return &coltracepb.ExportTraceServiceRequest{ResourceSpans: []*tracepb.ResourceSpans{{
		Resource:   &resourcepb.Resource{Attributes: attrs},
		ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{{Name: "checkout", TraceId: bytes.Repeat([]byte{1}, 16), SpanId: bytes.Repeat([]byte{2}, 8)}}}},
	}}}
}

// A pod's OTLP/HTTP trace reaches the Instance's endpoint with the
// cluster, pod and owning workload added to its resource, where the sender
// did not set them.
func TestCollectorForwardsTraceWithPodAttributes(t *testing.T) {
	sink, url := newOTLPHTTPSink(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", url)
	h, base := collectorFixture(t, containerpb.ManagedOpenTelemetryConfig_COLLECTION_AND_INSTRUMENTATION_COMPONENTS)

	body, _ := proto.Marshal(traceRequest(strAttr("service.name", "checkout"), strAttr("k8s.container.name", "sidecar")))
	w := postOTLP(t, h, base+"/v1/traces", "application/x-protobuf", body)
	if w.Code != http.StatusOK {
		t.Fatalf("export = %d: %s", w.Code, w.Body)
	}
	if err := proto.Unmarshal(w.Body.Bytes(), &coltracepb.ExportTraceServiceResponse{}); err != nil || w.Header().Get("Content-Type") != "application/x-protobuf" {
		t.Errorf("response %q (%s): %v", w.Body, w.Header().Get("Content-Type"), err)
	}
	var got coltracepb.ExportTraceServiceRequest
	if !sink.last(t, "/v1/traces", &got) {
		t.Fatal("nothing forwarded to /v1/traces")
	}
	want := map[string]string{
		"service.name": "checkout", "k8s.container.name": "sidecar",
		"cloud.provider": "gcp", "cloud.platform": "gcp_kubernetes_engine", "cloud.account.id": "p",
		"cloud.availability_zone": "us-central1-a", "k8s.cluster.name": "c0",
		"k8s.namespace.name": "shop", "k8s.pod.name": "web-7d9f-abc", "k8s.pod.uid": "u1", "k8s.node.name": "n1",
		"k8s.replicaset.name": "web-7d9f", "k8s.deployment.name": "web",
	}
	if attrs := attrMap(t, got.ResourceSpans[0].Resource); !reflect.DeepEqual(attrs, want) {
		t.Errorf("resource = %v\nwant       %v", attrs, want)
	}
	if n := len(got.ResourceSpans[0].ScopeSpans[0].Spans); n != 1 {
		t.Errorf("%d spans forwarded, want 1", n)
	}
}

// Metrics and logs go to their own paths, with the endpoint's headers and
// compression.
func TestCollectorForwardsMetricsAndLogs(t *testing.T) {
	sink, url := newOTLPHTTPSink(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", url)
	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "x-api-key=k1")
	t.Setenv("OTEL_EXPORTER_OTLP_LOGS_COMPRESSION", "gzip")
	h, base := collectorFixture(t, containerpb.ManagedOpenTelemetryConfig_COLLECTION_AND_INSTRUMENTATION_COMPONENTS)

	metrics, _ := proto.Marshal(&colmetricspb.ExportMetricsServiceRequest{ResourceMetrics: []*metricspb.ResourceMetrics{{
		ScopeMetrics: []*metricspb.ScopeMetrics{{Metrics: []*metricspb.Metric{{Name: "orders"}}}},
	}}})
	if w := postOTLP(t, h, base+"/v1/metrics", "application/x-protobuf", metrics); w.Code != http.StatusOK {
		t.Fatalf("metrics export = %d: %s", w.Code, w.Body)
	}
	var gotM colmetricspb.ExportMetricsServiceRequest
	if !sink.last(t, "/v1/metrics", &gotM) {
		t.Fatal("nothing forwarded to /v1/metrics")
	}
	if a := attrMap(t, gotM.ResourceMetrics[0].Resource); a["k8s.pod.name"] != "web-7d9f-abc" || a["k8s.container.name"] != "app" {
		t.Errorf("metrics resource = %v", a)
	}
	if k := sink.header.Get("x-api-key"); k != "k1" {
		t.Errorf("x-api-key = %q, want k1", k)
	}

	logs, _ := proto.Marshal(&collogspb.ExportLogsServiceRequest{ResourceLogs: []*logspb.ResourceLogs{{
		ScopeLogs: []*logspb.ScopeLogs{{LogRecords: []*logspb.LogRecord{{SeverityText: "INFO"}}}},
	}}})
	if w := postOTLP(t, h, base+"/v1/logs", "application/x-protobuf", logs); w.Code != http.StatusOK {
		t.Fatalf("logs export = %d: %s", w.Code, w.Body)
	}
	if enc := sink.header.Get("Content-Encoding"); enc != "gzip" {
		t.Fatalf("logs Content-Encoding = %q, want gzip", enc)
	}
	sink.mu.Lock()
	zr, err := gzip.NewReader(bytes.NewReader(sink.bodies["/v1/logs"][0]))
	sink.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(zr)
	var gotL collogspb.ExportLogsServiceRequest
	if err := proto.Unmarshal(raw, &gotL); err != nil {
		t.Fatal(err)
	}
	if a := attrMap(t, gotL.ResourceLogs[0].Resource); a["k8s.namespace.name"] != "shop" || a["k8s.cluster.name"] != "c0" {
		t.Errorf("logs resource = %v", a)
	}
}

// OTLP/JSON, whose trace and span IDs are hex, is accepted and answered in
// JSON.
func TestCollectorAcceptsJSON(t *testing.T) {
	sink, url := newOTLPHTTPSink(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", url)
	h, base := collectorFixture(t, containerpb.ManagedOpenTelemetryConfig_COLLECTION_AND_INSTRUMENTATION_COMPONENTS)
	body := `{"resourceSpans":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"web"}}]},
		"scopeSpans":[{"spans":[{"traceId":"5b8efff798038103d269b633813fc60c","spanId":"eee19b7ec3c1b174",
		"parentSpanId":"","name":"GET /","kind":2,"startTimeUnixNano":"1544712660000000000",
		"links":[{"traceId":"5b8efff798038103d269b633813fc60d","spanId":"eee19b7ec3c1b175"}]}]}]}]}`
	w := postOTLP(t, h, base+"/v1/traces", "application/json", []byte(body))
	if w.Code != http.StatusOK {
		t.Fatalf("export = %d: %s", w.Code, w.Body)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" || strings.TrimSpace(w.Body.String()) != "{}" {
		t.Errorf("response %q (%s), want {} as application/json", w.Body, ct)
	}
	var got coltracepb.ExportTraceServiceRequest
	if !sink.last(t, "/v1/traces", &got) {
		t.Fatal("nothing forwarded")
	}
	span := got.ResourceSpans[0].ScopeSpans[0].Spans[0]
	if hex.EncodeToString(span.TraceId) != "5b8efff798038103d269b633813fc60c" || hex.EncodeToString(span.SpanId) != "eee19b7ec3c1b174" ||
		hex.EncodeToString(span.Links[0].SpanId) != "eee19b7ec3c1b175" || span.StartTimeUnixNano != 1544712660000000000 {
		t.Errorf("span = %v", span)
	}
	if a := attrMap(t, got.ResourceSpans[0].Resource); a["service.name"] != "web" || a["k8s.pod.name"] != "web-7d9f-abc" {
		t.Errorf("resource = %v", a)
	}
}

// With no OTLP endpoint in the Instance's environment, exports succeed
// and go nowhere.
func TestCollectorWithoutEndpoint(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	h, base := collectorFixture(t, containerpb.ManagedOpenTelemetryConfig_COLLECTION_AND_INSTRUMENTATION_COMPONENTS)
	body, _ := proto.Marshal(traceRequest())
	if w := postOTLP(t, h, base+"/v1/traces", "application/x-protobuf", body); w.Code != http.StatusOK {
		t.Fatalf("export = %d: %s", w.Code, w.Body)
	}
}

// With the scope off there is no receiver.
func TestCollectorScopeOff(t *testing.T) {
	sink, url := newOTLPHTTPSink(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", url)
	h, base := collectorFixture(t, containerpb.ManagedOpenTelemetryConfig_NONE)
	body, _ := proto.Marshal(traceRequest())
	if w := postOTLP(t, h, base+"/v1/traces", "application/x-protobuf", body); w.Code != http.StatusNotFound {
		t.Errorf("export = %d, want 404", w.Code)
	}
	if n := sink.count(); n != 0 {
		t.Errorf("%d requests forwarded, want none", n)
	}
}

// A backend that fails the export fails the sender's, with a status OTLP
// retries.
func TestCollectorBackendFailure(t *testing.T) {
	sink, url := newOTLPHTTPSink(t)
	sink.status = http.StatusInternalServerError
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", url)
	h, base := collectorFixture(t, containerpb.ManagedOpenTelemetryConfig_COLLECTION_AND_INSTRUMENTATION_COMPONENTS)
	body, _ := proto.Marshal(traceRequest())
	if w := postOTLP(t, h, base+"/v1/traces", "application/x-protobuf", body); w.Code != http.StatusBadGateway {
		t.Errorf("export = %d, want 502", w.Code)
	}
}

// otlpGRPCSink is an OTLP/gRPC trace backend keeping the last request and
// its metadata.
type otlpGRPCSink struct {
	coltracepb.UnimplementedTraceServiceServer
	mu  sync.Mutex
	got *coltracepb.ExportTraceServiceRequest
	md  metadata.MD
}

func (k *otlpGRPCSink) Export(ctx context.Context, req *coltracepb.ExportTraceServiceRequest) (*coltracepb.ExportTraceServiceResponse, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.got = req
	k.md, _ = metadata.FromIncomingContext(ctx)
	return &coltracepb.ExportTraceServiceResponse{}, nil
}

// With OTEL_EXPORTER_OTLP_PROTOCOL=grpc, the collector forwards over gRPC,
// with the endpoint's headers as metadata.
func TestCollectorForwardsOverGRPC(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	sink := &otlpGRPCSink{}
	gs := grpc.NewServer()
	coltracepb.RegisterTraceServiceServer(gs, sink)
	go func() { _ = gs.Serve(ln) }()
	t.Cleanup(gs.Stop)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://"+ln.Addr().String())
	t.Setenv("OTEL_EXPORTER_OTLP_PROTOCOL", "grpc")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_HEADERS", "x-tenant=t1")
	h, base := collectorFixture(t, containerpb.ManagedOpenTelemetryConfig_COLLECTION_AND_INSTRUMENTATION_COMPONENTS)

	body, _ := proto.Marshal(traceRequest())
	if w := postOTLP(t, h, base+"/v1/traces", "application/x-protobuf", body); w.Code != http.StatusOK {
		t.Fatalf("export = %d: %s", w.Code, w.Body)
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if sink.got == nil {
		t.Fatal("nothing received over gRPC")
	}
	if a := attrMap(t, sink.got.ResourceSpans[0].Resource); a["k8s.pod.name"] != "web-7d9f-abc" {
		t.Errorf("resource = %v", a)
	}
	if v := sink.md.Get("x-tenant"); len(v) != 1 || v[0] != "t1" {
		t.Errorf("x-tenant metadata = %v, want [t1]", v)
	}
}

// The scope installs the collector's Service, which leads to the node's
// OTLP ports, and the Instrumentation CRD; NONE removes the Service only.
func TestReconcileManagedOTel(t *testing.T) {
	const (
		ns  = "/api/v1/namespaces/gke-managed-otel"
		svc = "/api/v1/namespaces/gke-managed-otel/services/opentelemetry-collector"
		crd = "/apis/apiextensions.k8s.io/v1/customresourcedefinitions/instrumentations.telemetry.googleapis.com"
	)
	s := newUnitService(t)
	k, kc := newFakeKube(t)
	k.establish = true
	scoped := func(scope containerpb.ManagedOpenTelemetryConfig_Scope) *containerpb.Cluster {
		return &containerpb.Cluster{ManagedOpentelemetryConfig: &containerpb.ManagedOpenTelemetryConfig{Scope: &scope}}
	}
	ctx := context.Background()

	if err := s.reconcileManagedOTel(ctx, kc, scoped(containerpb.ManagedOpenTelemetryConfig_NONE)); err != nil {
		t.Fatalf("NONE before install: %v", err)
	}
	if k.get(svc) != nil || k.get(crd) != nil {
		t.Fatal("NONE installed something")
	}

	if err := s.reconcileManagedOTel(ctx, kc, scoped(containerpb.ManagedOpenTelemetryConfig_COLLECTION_AND_INSTRUMENTATION_COMPONENTS)); err != nil {
		t.Fatalf("install: %v", err)
	}
	if k.get(ns) == nil || k.get(crd) == nil {
		t.Fatalf("namespace %v, CRD %v", k.get(ns) != nil, k.get(crd) != nil)
	}
	got := k.get(svc)
	if field(got, "spec.type") != "ExternalName" || field(got, "spec.externalName") != "metadata.google.internal" {
		t.Errorf("Service spec = %v", got["spec"])
	}
	ports := map[float64]string{}
	for _, p := range field(got, "spec.ports").([]any) {
		pm := p.(map[string]any)
		ports[toFloat(pm["port"])] = fmt.Sprint(pm["name"])
	}
	if !reflect.DeepEqual(ports, map[float64]string{4317: "otlp-grpc", 4318: "otlp-http"}) {
		t.Errorf("Service ports = %v", ports)
	}
	if c := k.get(crd); field(c, "spec.group") != "telemetry.googleapis.com" || field(c, "spec.names.kind") != "Instrumentation" {
		t.Errorf("CRD spec = %v", c["spec"])
	}

	if err := s.reconcileManagedOTel(ctx, kc, scoped(containerpb.ManagedOpenTelemetryConfig_NONE)); err != nil {
		t.Fatalf("NONE: %v", err)
	}
	if k.get(svc) != nil {
		t.Error("Service kept after NONE")
	}
	if k.get(crd) == nil || k.get(ns) == nil {
		t.Error("CRD or namespace removed after NONE")
	}

	// A Service of that name the add-on did not install stays.
	k.put(`{"apiVersion":"v1","kind":"Service","metadata":{"name":"opentelemetry-collector","namespace":"gke-managed-otel"}}`)
	if err := s.reconcileManagedOTel(ctx, kc, scoped(containerpb.ManagedOpenTelemetryConfig_NONE)); err != nil {
		t.Fatalf("NONE with a user Service: %v", err)
	}
	if k.get(svc) == nil {
		t.Error("user's Service removed")
	}
}

func toFloat(v any) float64 {
	switch v := v.(type) {
	case float64:
		return v
	case int:
		return float64(v)
	}
	return -1
}

// The node agent relays OTLP/HTTP and OTLP/gRPC from pods to the
// Instance's receiver, tagged with the sender's address.
func TestNodeOTLPRelay(t *testing.T) {
	var (
		mu     sync.Mutex
		paths  []string
		bodies [][]byte
		status = http.StatusOK
	)
	emuSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		defer mu.Unlock()
		paths, bodies = append(paths, r.URL.Path), append(bodies, b)
		if status != http.StatusOK {
			http.Error(w, "backend down", status)
			return
		}
		w.Header().Set("Content-Type", "application/x-protobuf")
	}))
	t.Cleanup(emuSrv.Close)
	httpL, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	grpcL, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	stop := serveNodeOTLP(httpL, grpcL, emuSrv.URL+"/node/n1")
	t.Cleanup(stop)
	last := func() (string, []byte) {
		mu.Lock()
		defer mu.Unlock()
		if len(paths) == 0 {
			return "", nil
		}
		return paths[len(paths)-1], bodies[len(bodies)-1]
	}

	metrics, _ := proto.Marshal(&colmetricspb.ExportMetricsServiceRequest{ResourceMetrics: []*metricspb.ResourceMetrics{{}}})
	resp, err := http.Post("http://"+httpL.Addr().String()+"/v1/metrics", "application/x-protobuf", bytes.NewReader(metrics))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if p, b := last(); resp.StatusCode != http.StatusOK || p != "/node/n1/otlp/127.0.0.1/v1/metrics" || !bytes.Equal(b, metrics) {
		t.Errorf("HTTP: status %d, relayed to %q (%d bytes)", resp.StatusCode, p, len(b))
	}

	cc, err := grpc.NewClient(grpcL.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cc.Close() })
	ctx := context.Background()
	if _, err := coltracepb.NewTraceServiceClient(cc).Export(ctx, traceRequest(strAttr("service.name", "web"))); err != nil {
		t.Fatalf("gRPC traces: %v", err)
	}
	p, b := last()
	var got coltracepb.ExportTraceServiceRequest
	if err := proto.Unmarshal(b, &got); err != nil || p != "/node/n1/otlp/127.0.0.1/v1/traces" || attrMap(t, got.ResourceSpans[0].Resource)["service.name"] != "web" {
		t.Errorf("gRPC traces relayed to %q: %v (%v)", p, &got, err)
	}
	if _, err := collogspb.NewLogsServiceClient(cc).Export(ctx, &collogspb.ExportLogsServiceRequest{}); err != nil {
		t.Fatalf("gRPC logs: %v", err)
	}
	if p, _ := last(); p != "/node/n1/otlp/127.0.0.1/v1/logs" {
		t.Errorf("gRPC logs relayed to %q", p)
	}

	mu.Lock()
	status = http.StatusBadGateway
	mu.Unlock()
	_, err = colmetricspb.NewMetricsServiceClient(cc).Export(ctx, &colmetricspb.ExportMetricsServiceRequest{})
	if c := grpcstatus.Code(err); c != codes.Unavailable {
		t.Errorf("gRPC export with the backend down = %v (%v), want Unavailable", c, err)
	}
}

// Exports from a pod IP that no pod has (a pod already gone) wait for one
// only once, not on every export, and still go through.
func TestCollectorUnknownPodWaitsOnce(t *testing.T) {
	setPodWait(t, 300*time.Millisecond, 50*time.Millisecond)
	sink, url := newOTLPHTTPSink(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", url)
	h, _ := collectorFixture(t, containerpb.ManagedOpenTelemetryConfig_COLLECTION_AND_INSTRUMENTATION_COMPONENTS)
	body, _ := proto.Marshal(traceRequest())
	path := "/_emu/node/cid/s3cret/n1/otlp/10.4.0.99/v1/traces"
	if w := postOTLP(t, h, path, "application/x-protobuf", body); w.Code != http.StatusOK {
		t.Fatalf("first export = %d: %s", w.Code, w.Body)
	}
	start := time.Now()
	if w := postOTLP(t, h, path, "application/x-protobuf", body); w.Code != http.StatusOK {
		t.Fatalf("second export = %d: %s", w.Code, w.Body)
	}
	if d := time.Since(start); d >= 300*time.Millisecond {
		t.Errorf("second export took %v, waiting for the pod again", d)
	}
	var got coltracepb.ExportTraceServiceRequest
	if !sink.last(t, "/v1/traces", &got) {
		t.Fatal("nothing forwarded")
	}
	if a := attrMap(t, got.ResourceSpans[0].Resource); a["k8s.cluster.name"] != "c0" || a["k8s.pod.name"] != "" {
		t.Errorf("resource = %v, want the cluster only", a)
	}
}

// A signal whose protocol the Instance does not speak is dropped as if it
// had no OTLP endpoint; the other signals still go through.
func TestCollectorUnusableProtocolDropsThatSignal(t *testing.T) {
	sink, url := newOTLPHTTPSink(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", url)
	t.Setenv("OTEL_EXPORTER_OTLP_LOGS_PROTOCOL", "http/json")
	h, base := collectorFixture(t, containerpb.ManagedOpenTelemetryConfig_COLLECTION_AND_INSTRUMENTATION_COMPONENTS)

	logs, _ := proto.Marshal(&collogspb.ExportLogsServiceRequest{ResourceLogs: []*logspb.ResourceLogs{{}}})
	for range 2 {
		if w := postOTLP(t, h, base+"/v1/logs", "application/x-protobuf", logs); w.Code != http.StatusOK {
			t.Fatalf("logs export = %d: %s", w.Code, w.Body)
		}
	}
	traces, _ := proto.Marshal(traceRequest())
	if w := postOTLP(t, h, base+"/v1/traces", "application/x-protobuf", traces); w.Code != http.StatusOK {
		t.Fatalf("traces export = %d: %s", w.Code, w.Body)
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if n := len(sink.bodies["/v1/logs"]); n != 0 {
		t.Errorf("%d logs requests forwarded, want none", n)
	}
	if n := len(sink.bodies["/v1/traces"]); n != 1 {
		t.Errorf("%d traces requests forwarded, want 1", n)
	}
}

// exportTraces posts one request of a resource for each of resources to
// the receiver at path and returns the resources forwarded.
func exportTraces(t *testing.T, h http.Handler, sink *otlpHTTPSink, path string, resources ...[]*commonpb.KeyValue) []map[string]string {
	t.Helper()
	req := &coltracepb.ExportTraceServiceRequest{}
	for _, attrs := range resources {
		req.ResourceSpans = append(req.ResourceSpans, traceRequest(attrs...).ResourceSpans...)
	}
	body, _ := proto.Marshal(req)
	if w := postOTLP(t, h, path, "application/x-protobuf", body); w.Code != http.StatusOK {
		t.Fatalf("export = %d: %s", w.Code, w.Body)
	}
	var got coltracepb.ExportTraceServiceRequest
	if !sink.last(t, "/v1/traces", &got) {
		t.Fatal("nothing forwarded")
	}
	var out []map[string]string
	for _, rs := range got.ResourceSpans {
		out = append(out, attrMap(t, rs.Resource))
	}
	return out
}

// A hostNetwork pod, which shares its node's address, is attributed by
// the k8s.pod.uid its Instrumentation sets.
func TestCollectorAttributesHostNetworkPodByUID(t *testing.T) {
	sink, url := newOTLPHTTPSink(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", url)
	h, _, k := collectorFixtureKube(t, containerpb.ManagedOpenTelemetryConfig_COLLECTION_AND_INSTRUMENTATION_COMPONENTS)
	k.put(`{"apiVersion":"v1","kind":"Pod","metadata":{"name":"agent-x1","namespace":"ops","uid":"u-host"},
		"spec":{"nodeName":"n1","hostNetwork":true,"containers":[{"name":"agent"}]},"status":{"phase":"Running","podIP":"10.128.0.2"}}`)

	got := exportTraces(t, h, sink, "/_emu/node/cid/s3cret/n1/otlp/10.128.0.2/v1/traces",
		[]*commonpb.KeyValue{strAttr("service.name", "agent"), strAttr("k8s.pod.uid", "u-host")})
	want := map[string]string{
		"service.name":   "agent",
		"cloud.provider": "gcp", "cloud.platform": "gcp_kubernetes_engine", "cloud.account.id": "p",
		"cloud.availability_zone": "us-central1-a", "k8s.cluster.name": "c0",
		"k8s.namespace.name": "ops", "k8s.pod.name": "agent-x1", "k8s.pod.uid": "u-host", "k8s.node.name": "n1",
		"k8s.container.name": "agent",
	}
	if !reflect.DeepEqual(got[0], want) {
		t.Errorf("resource = %v\nwant       %v", got[0], want)
	}
}

// A k8s.pod.uid naming a pod of the cluster attributes its resource to
// that pod, in any phase and ahead of the sender's address; one naming no
// pod falls back to the address.
func TestCollectorAttributesByPodUID(t *testing.T) {
	const (
		web   = "/_emu/node/cid/s3cret/n1/otlp/10.4.0.7/v1/traces"
		node  = "/_emu/node/cid/s3cret/n1/otlp/10.128.0.2/v1/traces"
		other = "other"
	)
	uid := func(u string) []*commonpb.KeyValue { return []*commonpb.KeyValue{strAttr("k8s.pod.uid", u)} }
	for _, tc := range []struct {
		name      string
		path      string
		resources [][]*commonpb.KeyValue
		want      []map[string]string // pod attributes of each resource
	}{
		{name: "UID wins over the address", path: web, resources: [][]*commonpb.KeyValue{uid("u0")},
			want: []map[string]string{{"k8s.pod.name": other, "k8s.pod.uid": "u0", "k8s.container.name": "x"}}},
		{name: "each resource its own pod", path: web,
			resources: [][]*commonpb.KeyValue{uid("u0"), uid("u-done"), nil},
			want: []map[string]string{
				{"k8s.pod.name": other, "k8s.pod.uid": "u0", "k8s.container.name": "x"},
				{"k8s.pod.name": "batch-1", "k8s.pod.uid": "u-done", "k8s.container.name": "job"},
				{"k8s.pod.name": "web-7d9f-abc", "k8s.pod.uid": "u1", "k8s.container.name": "app"},
			}},
		{name: "Succeeded pod", path: node, resources: [][]*commonpb.KeyValue{uid("u-done")},
			want: []map[string]string{{"k8s.pod.name": "batch-1", "k8s.pod.uid": "u-done", "k8s.container.name": "job"}}},
		{name: "Failed pod", path: node, resources: [][]*commonpb.KeyValue{uid("u-failed")},
			want: []map[string]string{{"k8s.pod.name": "batch-2", "k8s.pod.uid": "u-failed", "k8s.container.name": "job"}}},
		{name: "unknown UID falls back to the address", path: web, resources: [][]*commonpb.KeyValue{uid("u-gone")},
			want: []map[string]string{{"k8s.pod.name": "web-7d9f-abc", "k8s.pod.uid": "u-gone", "k8s.container.name": "app"}}},
		{name: "unknown UID from a node address", path: node, resources: [][]*commonpb.KeyValue{uid("u-gone")},
			want: []map[string]string{{"k8s.pod.name": "", "k8s.pod.uid": "u-gone", "k8s.container.name": ""}}},
		{name: "the sender's attributes are kept", path: node,
			resources: [][]*commonpb.KeyValue{{strAttr("k8s.pod.uid", "u0"), strAttr("k8s.pod.name", "mine"), strAttr("k8s.container.name", "c")}},
			want:      []map[string]string{{"k8s.pod.name": "mine", "k8s.pod.uid": "u0", "k8s.container.name": "c"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sink, url := newOTLPHTTPSink(t)
			t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", url)
			h, _, k := collectorFixtureKube(t, containerpb.ManagedOpenTelemetryConfig_COLLECTION_AND_INSTRUMENTATION_COMPONENTS)
			k.put(`{"apiVersion":"v1","kind":"Pod","metadata":{"name":"batch-1","namespace":"shop","uid":"u-done"},
				"spec":{"nodeName":"n2","containers":[{"name":"job"}]},"status":{"phase":"Succeeded","podIP":"10.4.0.12"}}`)
			k.put(`{"apiVersion":"v1","kind":"Pod","metadata":{"name":"batch-2","namespace":"shop","uid":"u-failed"},
				"spec":{"nodeName":"n2","containers":[{"name":"job"}]},"status":{"phase":"Failed"}}`)
			got := exportTraces(t, h, sink, tc.path, tc.resources...)
			if len(got) != len(tc.want) {
				t.Fatalf("%d resources forwarded, want %d", len(got), len(tc.want))
			}
			for i, w := range tc.want {
				for key, v := range w {
					if got[i][key] != v {
						t.Errorf("resource %d %s = %q, want %q (%v)", i, key, got[i][key], v, got[i])
					}
				}
				if got[i]["k8s.cluster.name"] != "c0" {
					t.Errorf("resource %d has no cluster attributes: %v", i, got[i])
				}
			}
		})
	}
}

// A k8s.pod.uid that named no pod is not looked up again on every export:
// a pod created with it soon after is not yet found.
func TestCollectorRemembersUnknownPodUID(t *testing.T) {
	sink, url := newOTLPHTTPSink(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", url)
	h, _, k := collectorFixtureKube(t, containerpb.ManagedOpenTelemetryConfig_COLLECTION_AND_INSTRUMENTATION_COMPONENTS)
	const path = "/_emu/node/cid/s3cret/n1/otlp/10.128.0.2/v1/traces"
	res := []*commonpb.KeyValue{strAttr("k8s.pod.uid", "u-late")}
	if got := exportTraces(t, h, sink, path, res); got[0]["k8s.pod.name"] != "" {
		t.Fatalf("resource = %v, want no pod", got[0])
	}
	k.put(`{"apiVersion":"v1","kind":"Pod","metadata":{"name":"late","namespace":"ops","uid":"u-late"},
		"spec":{"nodeName":"n1","hostNetwork":true,"containers":[{"name":"a"}]},"status":{"phase":"Running"}}`)
	if got := exportTraces(t, h, sink, path, res); got[0]["k8s.pod.name"] != "" {
		t.Errorf("resource = %v, want no pod until the miss expires", got[0])
	}
}
