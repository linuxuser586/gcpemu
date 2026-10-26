package gke

import (
	"bytes"
	stdgzip "compress/gzip"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/container/apiv1/containerpb"
	"go.opentelemetry.io/otel/attribute"
	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/encoding/gzip"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// The Managed collector (FR-GKE-019, ADR 0034). While a cluster's Managed
// OpenTelemetry scope is
// COLLECTION_AND_INSTRUMENTATION_COMPONENTS, the Instance receives OTLP
// from the cluster's pods at
//
//	POST /_emu/node/{clusterId}/{secret}/{node}/otlp/{podIP}/v1/{traces|metrics|logs}
//
// which the node agent relays its OTLP ports to, tagged with the sender's
// address. Each resource gets the cluster, namespace, pod, container
// (when the pod has one), node and owning workload that the sender did
// not set, as FR-GKE-014's system metrics have them, and the request goes
// on to the Instance's OTLP endpoint for its signal, named by the standard
// OTel env vars (read per request). With no OTLP endpoint the request succeeds
// and the data is dropped. Nothing is batched, queued or retried: a
// failed forward fails the sender's export, which retries as OTLP says.

const (
	defaultOTLPTimeout = 10 * time.Second
	// maxOTLPBody bounds a request the receiver reads.
	maxOTLPBody = 32 << 20
)

// otlpTarget is where the managed collector forwards one signal.
type otlpTarget struct {
	protocol string // "grpc" or "http/protobuf"
	// endpoint is the URL to POST to for http/protobuf, host:port for grpc.
	endpoint    string
	insecure    bool // grpc without TLS
	headers     map[string]string
	timeout     time.Duration
	compression string // "gzip" or ""
	certificate string // CA file for TLS
}

// otlpTargetFromEnv returns the target of signal ("traces", "metrics" or
// "logs") from the standard OTel env vars, a signal's own winning over the
// general ones, or false when no endpoint is configured.
func otlpTargetFromEnv(getenv func(string) string, signal string) (otlpTarget, bool) {
	sig := "OTEL_EXPORTER_OTLP_" + strings.ToUpper(signal) + "_"
	env := func(name string) string {
		if v := getenv(sig + name); v != "" {
			return v
		}
		return getenv("OTEL_EXPORTER_OTLP_" + name)
	}
	t := otlpTarget{protocol: "http/protobuf", timeout: defaultOTLPTimeout, certificate: env("CERTIFICATE")}
	if p := env("PROTOCOL"); p != "" {
		t.protocol = p
	}
	endpoint := getenv(sig + "ENDPOINT")
	if endpoint == "" {
		base := getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
		if base == "" {
			return otlpTarget{}, false
		}
		endpoint = base
		if t.protocol != "grpc" {
			endpoint = strings.TrimSuffix(base, "/") + "/v1/" + signal
		}
	}
	t.endpoint = endpoint
	if t.protocol == "grpc" {
		t.insecure = strings.EqualFold(env("INSECURE"), "true")
		if u, err := url.Parse(endpoint); err == nil && (u.Scheme == "http" || u.Scheme == "https") {
			t.endpoint, t.insecure = u.Host, u.Scheme == "http"
		}
	}
	if ms, err := strconv.Atoi(getenv(sig + "TIMEOUT")); err == nil && ms > 0 {
		t.timeout = time.Duration(ms) * time.Millisecond
	} else if ms, err := strconv.Atoi(getenv("OTEL_EXPORTER_OTLP_TIMEOUT")); err == nil && ms > 0 {
		t.timeout = time.Duration(ms) * time.Millisecond
	}
	if env("COMPRESSION") == "gzip" {
		t.compression = "gzip"
	}
	if h := env("HEADERS"); h != "" {
		t.headers = map[string]string{}
		for _, kv := range strings.Split(h, ",") {
			k, v, ok := strings.Cut(kv, "=")
			if !ok {
				continue
			}
			if dv, err := url.PathUnescape(strings.TrimSpace(v)); err == nil {
				v = dv
			}
			t.headers[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return t, true
}

// otlpSignal is how the receiver handles one signal's requests.
type otlpSignal struct {
	newRequest  func() proto.Message
	newResponse func() proto.Message
	// resources returns the resource of each of the request's batches,
	// creating those that are missing.
	resources func(proto.Message) []*resourcepb.Resource
	// export sends a request over gRPC.
	export func(context.Context, grpc.ClientConnInterface, proto.Message, ...grpc.CallOption) error
}

// otlpSignals are the signals the receiver serves, by path element.
var otlpSignals = map[string]otlpSignal{
	"traces": {
		newRequest:  func() proto.Message { return &coltracepb.ExportTraceServiceRequest{} },
		newResponse: func() proto.Message { return &coltracepb.ExportTraceServiceResponse{} },
		resources: func(m proto.Message) (out []*resourcepb.Resource) {
			for _, r := range m.(*coltracepb.ExportTraceServiceRequest).ResourceSpans {
				if r.Resource == nil {
					r.Resource = &resourcepb.Resource{}
				}
				out = append(out, r.Resource)
			}
			return out
		},
		export: func(ctx context.Context, cc grpc.ClientConnInterface, m proto.Message, opts ...grpc.CallOption) error {
			_, err := coltracepb.NewTraceServiceClient(cc).Export(ctx, m.(*coltracepb.ExportTraceServiceRequest), opts...)
			return err
		},
	},
	"metrics": {
		newRequest:  func() proto.Message { return &colmetricspb.ExportMetricsServiceRequest{} },
		newResponse: func() proto.Message { return &colmetricspb.ExportMetricsServiceResponse{} },
		resources: func(m proto.Message) (out []*resourcepb.Resource) {
			for _, r := range m.(*colmetricspb.ExportMetricsServiceRequest).ResourceMetrics {
				if r.Resource == nil {
					r.Resource = &resourcepb.Resource{}
				}
				out = append(out, r.Resource)
			}
			return out
		},
		export: func(ctx context.Context, cc grpc.ClientConnInterface, m proto.Message, opts ...grpc.CallOption) error {
			_, err := colmetricspb.NewMetricsServiceClient(cc).Export(ctx, m.(*colmetricspb.ExportMetricsServiceRequest), opts...)
			return err
		},
	},
	"logs": {
		newRequest:  func() proto.Message { return &collogspb.ExportLogsServiceRequest{} },
		newResponse: func() proto.Message { return &collogspb.ExportLogsServiceResponse{} },
		resources: func(m proto.Message) (out []*resourcepb.Resource) {
			for _, r := range m.(*collogspb.ExportLogsServiceRequest).ResourceLogs {
				if r.Resource == nil {
					r.Resource = &resourcepb.Resource{}
				}
				out = append(out, r.Resource)
			}
			return out
		},
		export: func(ctx context.Context, cc grpc.ClientConnInterface, m proto.Message, opts ...grpc.CallOption) error {
			_, err := collogspb.NewLogsServiceClient(cc).Export(ctx, m.(*collogspb.ExportLogsServiceRequest), opts...)
			return err
		},
	},
}

func managedOTelOn(c *containerpb.Cluster) bool {
	return c.GetManagedOpentelemetryConfig().GetScope() == containerpb.ManagedOpenTelemetryConfig_COLLECTION_AND_INSTRUMENTATION_COMPONENTS
}

// serveOTLP receives one OTLP/HTTP export of signal from the pod at ip.
func (s *Service) serveOTLP(w http.ResponseWriter, r *http.Request, key, ip, signal string) {
	sig, ok := otlpSignals[signal]
	if !ok {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "OTLP exports are POSTed", http.StatusMethodNotAllowed)
		return
	}
	rec, err := s.loadKey(key)
	if err != nil || !managedOTelOn(rec.cluster()) {
		http.NotFound(w, r)
		return
	}
	body, err := readOTLPBody(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	isJSON := ct == "application/json"
	req := sig.newRequest()
	if isJSON {
		err = unmarshalOTLPJSON(body, req)
	} else {
		err = proto.Unmarshal(body, req)
	}
	if err != nil {
		http.Error(w, "invalid OTLP request: "+err.Error(), http.StatusBadRequest)
		return
	}
	attrs := protoAttrs(append(clusterResourceAttrs(rec), s.podResourceAttrs(r.Context(), key, rec, ip)...))
	for _, res := range sig.resources(req) {
		addMissingAttrs(res, attrs)
	}
	if t, ok := otlpTargetFromEnv(os.Getenv, signal); ok {
		if err := forwardOTLP(r.Context(), t, sig, req); err != nil {
			s.env.Log.Debug("gke: managed collector", "cluster", key, "signal", signal, "err", err)
			http.Error(w, "forwarding to the Instance's OTLP endpoint: "+err.Error(), http.StatusBadGateway)
			return
		}
	}
	if isJSON {
		out, _ := protojson.Marshal(sig.newResponse())
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(out)
		return
	}
	out, _ := proto.Marshal(sig.newResponse())
	w.Header().Set("Content-Type", "application/x-protobuf")
	_, _ = w.Write(out)
}

// otlpIDKeys are the OTLP/JSON fields that hold IDs in hex, where
// protobuf's JSON mapping has base64.
var otlpIDKeys = map[string]bool{"traceId": true, "spanId": true, "parentSpanId": true}

// unmarshalOTLPJSON decodes an OTLP/JSON request into m.
func unmarshalOTLPJSON(b []byte, m proto.Message) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return err
	}
	if err := hexIDsToBase64(v); err != nil {
		return err
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return protojson.UnmarshalOptions{DiscardUnknown: true}.Unmarshal(b, m)
}

// hexIDsToBase64 rewrites, in decoded JSON v, the hex IDs of
// otlpIDKeys as base64.
func hexIDsToBase64(v any) error {
	switch v := v.(type) {
	case map[string]any:
		for k, e := range v {
			if s, ok := e.(string); ok && otlpIDKeys[k] {
				id, err := hex.DecodeString(s)
				if err != nil {
					return fmt.Errorf("%s %q is not hex", k, s)
				}
				v[k] = base64.StdEncoding.EncodeToString(id)
				continue
			}
			if err := hexIDsToBase64(e); err != nil {
				return err
			}
		}
	case []any:
		for _, e := range v {
			if err := hexIDsToBase64(e); err != nil {
				return err
			}
		}
	}
	return nil
}

// readOTLPBody reads a request body, gunzipping it if it is compressed.
func readOTLPBody(r *http.Request) ([]byte, error) {
	var body io.Reader = http.MaxBytesReader(nil, r.Body, maxOTLPBody)
	if strings.EqualFold(r.Header.Get("Content-Encoding"), "gzip") {
		zr, err := stdgzip.NewReader(body)
		if err != nil {
			return nil, err
		}
		body = io.LimitReader(zr, maxOTLPBody)
	}
	return io.ReadAll(body)
}

// protoAttrs converts string attributes to OTLP's form.
func protoAttrs(attrs []attribute.KeyValue) []*commonpb.KeyValue {
	out := make([]*commonpb.KeyValue, 0, len(attrs))
	for _, a := range attrs {
		out = append(out, &commonpb.KeyValue{Key: string(a.Key), Value: &commonpb.AnyValue{
			Value: &commonpb.AnyValue_StringValue{StringValue: a.Value.Emit()}}})
	}
	return out
}

// addMissingAttrs adds to res those of attrs whose keys it does not have.
func addMissingAttrs(res *resourcepb.Resource, attrs []*commonpb.KeyValue) {
	has := map[string]bool{}
	for _, kv := range res.Attributes {
		has[kv.Key] = true
	}
	for _, kv := range attrs {
		if !has[kv.Key] {
			res.Attributes = append(res.Attributes, proto.Clone(kv).(*commonpb.KeyValue))
		}
	}
}

// otlpPod is a pod as the receiver reads it.
type otlpPod struct {
	Metadata objectMeta `json:"metadata"`
	Spec     struct {
		NodeName    string `json:"nodeName"`
		HostNetwork bool   `json:"hostNetwork"`
		Containers  []struct {
			Name string `json:"name"`
		} `json:"containers"`
	} `json:"spec"`
	Status struct {
		Phase string `json:"phase"`
		PodIP string `json:"podIP"`
	} `json:"status"`
}

// otlpPodEntry is a pod IP's cached attributes, none when no pod had it.
type otlpPodEntry struct {
	attrs []attribute.KeyValue
	exp   time.Time
}

// otlpPodTTL and otlpNoPodTTL are how long a pod IP's attributes, or the
// finding that no pod has it, are reused.
const (
	otlpPodTTL   = 5 * time.Second
	otlpNoPodTTL = 30 * time.Second
)

// podResourceAttrs returns the attributes of the pod at ip: namespace,
// pod, its container if it has only one, node and owning workload. There
// are none for an address outside the pod range, or when no pod has it
// within podWait (as for the metadata server, the kubelet reports a new
// pod's IP a moment after it starts); that is remembered, so that a pod
// already gone does not wait on every export.
func (s *Service) podResourceAttrs(ctx context.Context, key string, rec *clusterRecord, ip string) []attribute.KeyValue {
	if !s.isPodIP(rec, ip) {
		return nil
	}
	rt := s.rtFor(key)
	if v, ok := rt.otlp.Load(ip); ok {
		if e := v.(otlpPodEntry); time.Now().Before(e.exp) {
			return e.attrs
		}
	}
	kc, err := s.kube(key)
	if err != nil {
		return nil
	}
	wctx, cancel := context.WithTimeout(ctx, podWait)
	defer cancel()
	for {
		if attrs, found := lookupOTLPPod(wctx, kc, ip); found {
			rt.otlp.Store(ip, otlpPodEntry{attrs: attrs, exp: time.Now().Add(otlpPodTTL)})
			return attrs
		}
		select {
		case <-wctx.Done():
			if ctx.Err() == nil {
				rt.otlp.Store(ip, otlpPodEntry{exp: time.Now().Add(otlpNoPodTTL)})
			}
			return nil
		case <-time.After(podPoll):
		}
	}
}

// lookupOTLPPod returns the attributes of the running pod with ip, found
// false when there is none or the API server cannot tell.
func lookupOTLPPod(ctx context.Context, kc *kubeClient, ip string) ([]attribute.KeyValue, bool) {
	var pods kubeList[otlpPod]
	if err := kc.get(ctx, "/api/v1/pods?fieldSelector=status.podIP%3D"+url.QueryEscape(ip), &pods); err != nil {
		return nil, false
	}
	for _, p := range pods.Items {
		if p.Status.PodIP != ip || p.Spec.HostNetwork || p.Status.Phase == "Succeeded" || p.Status.Phase == "Failed" {
			continue
		}
		m := p.Metadata
		attrs := []attribute.KeyValue{
			attribute.String("k8s.namespace.name", m.Namespace),
			attribute.String("k8s.pod.name", m.Name),
			attribute.String("k8s.pod.uid", m.UID),
		}
		if p.Spec.NodeName != "" {
			attrs = append(attrs, attribute.String("k8s.node.name", p.Spec.NodeName))
		}
		if len(p.Spec.Containers) == 1 {
			attrs = append(attrs, attribute.String("k8s.container.name", p.Spec.Containers[0].Name))
		}
		// The owners' owners: a ReplicaSet's Deployment, a Job's CronJob.
		owners := map[string]objectMeta{}
		for _, o := range m.OwnerReferences {
			var path string
			switch o.Kind {
			case "ReplicaSet":
				path = "/apis/apps/v1/namespaces/" + m.Namespace + "/replicasets/" + o.Name
			case "Job":
				path = "/apis/batch/v1/namespaces/" + m.Namespace + "/jobs/" + o.Name
			default:
				continue
			}
			var owned metricsOwned
			if err := kc.get(ctx, path, &owned); err == nil {
				owners[o.Kind+"/"+m.Namespace+"/"+o.Name] = owned.Metadata
			}
		}
		return append(attrs, workloadAttrs(m, owners)...), true
	}
	return nil, false
}

// forwardOTLP sends req, a request of sig, to t.
func forwardOTLP(ctx context.Context, t otlpTarget, sig otlpSignal, req proto.Message) error {
	ctx, cancel := context.WithTimeout(ctx, t.timeout)
	defer cancel()
	switch t.protocol {
	case "grpc":
		cc, err := otlpConn(t)
		if err != nil {
			return err
		}
		var opts []grpc.CallOption
		if t.compression == "gzip" {
			opts = append(opts, grpc.UseCompressor(gzip.Name))
		}
		for k, v := range t.headers {
			ctx = metadata.AppendToOutgoingContext(ctx, strings.ToLower(k), v)
		}
		return sig.export(ctx, cc, req, opts...)
	case "http/protobuf":
	default:
		return fmt.Errorf("unsupported OTLP protocol %q (want grpc or http/protobuf)", t.protocol)
	}
	body, err := proto.Marshal(req)
	if err != nil {
		return err
	}
	if t.compression == "gzip" {
		var b bytes.Buffer
		zw := stdgzip.NewWriter(&b)
		_, _ = zw.Write(body)
		_ = zw.Close()
		body = b.Bytes()
	}
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, t.endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	hr.Header.Set("Content-Type", "application/x-protobuf")
	if t.compression == "gzip" {
		hr.Header.Set("Content-Encoding", "gzip")
	}
	for k, v := range t.headers {
		hr.Header.Set(k, v)
	}
	tlsConf, err := t.tlsConfig()
	if err != nil {
		return err
	}
	tr := http.DefaultTransport
	if tlsConf != nil {
		ht := http.DefaultTransport.(*http.Transport).Clone()
		ht.TLSClientConfig = tlsConf
		defer ht.CloseIdleConnections()
		tr = ht
	}
	resp, err := (&http.Client{Transport: tr}).Do(hr)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("%s: %s %s", t.endpoint, resp.Status, bytes.TrimSpace(msg))
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

// tlsConfig trusts t.certificate when it is set, and is nil otherwise.
func (t otlpTarget) tlsConfig() (*tls.Config, error) {
	if t.certificate == "" {
		return nil, nil
	}
	pem, err := os.ReadFile(t.certificate)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, errors.New(t.certificate + ": no certificates")
	}
	return &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}, nil
}

// otlpConns are the gRPC connections to OTLP endpoints, by endpoint and
// transport security; they live as long as the process, like the HTTP
// transport's.
var (
	otlpConnMu sync.Mutex
	otlpConns  = map[string]*grpc.ClientConn{}
)

// otlpConn returns the connection to t's gRPC endpoint.
func otlpConn(t otlpTarget) (*grpc.ClientConn, error) {
	key := fmt.Sprint(t.endpoint, "|", t.insecure, "|", t.certificate)
	otlpConnMu.Lock()
	defer otlpConnMu.Unlock()
	if cc := otlpConns[key]; cc != nil {
		return cc, nil
	}
	creds := insecure.NewCredentials()
	if !t.insecure {
		conf, err := t.tlsConfig()
		if err != nil {
			return nil, err
		}
		if conf == nil {
			conf = &tls.Config{MinVersion: tls.VersionTLS12}
		}
		creds = credentials.NewTLS(conf)
	}
	cc, err := grpc.NewClient(t.endpoint, grpc.WithTransportCredentials(creds))
	if err != nil {
		return nil, err
	}
	otlpConns[key] = cc
	return cc, nil
}

// ---- node agent ----

// Ports where the node agent receives OTLP, on metadataIP: the managed
// collector's Service names metadata.google.internal.
const (
	otlpGRPCPort = 4317
	otlpHTTPPort = 4318
)

// serveNodeOTLP relays OTLP/HTTP from httpL and OTLP/gRPC from grpcL to
// the Instance's receiver under emu (the node's endpoint base URL), tagged
// with each sender's address. It returns a function that stops both.
func serveNodeOTLP(httpL, grpcL net.Listener, emu string) func() {
	u, _ := url.Parse(emu)
	hs := &http.Server{ReadHeaderTimeout: 10 * time.Second, Handler: &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			src, _, _ := net.SplitHostPort(pr.In.RemoteAddr)
			pr.Out.URL.Scheme = u.Scheme
			pr.Out.URL.Host = u.Host
			pr.Out.URL.Path = u.Path + "/otlp/" + src + pr.In.URL.Path
			pr.Out.URL.RawPath = ""
			pr.Out.Host = u.Host
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			http.Error(w, "managed collector unavailable: "+err.Error(), http.StatusServiceUnavailable)
		},
	}}
	go func() { _ = hs.Serve(httpL) }()
	gs := grpc.NewServer()
	coltracepb.RegisterTraceServiceServer(gs, otlpTraceRelay{emu: emu})
	colmetricspb.RegisterMetricsServiceServer(gs, otlpMetricsRelay{emu: emu})
	collogspb.RegisterLogsServiceServer(gs, otlpLogsRelay{emu: emu})
	go func() { _ = gs.Serve(grpcL) }()
	return func() {
		gs.Stop()
		_ = hs.Close()
	}
}

type otlpTraceRelay struct {
	coltracepb.UnimplementedTraceServiceServer
	emu string
}

func (r otlpTraceRelay) Export(ctx context.Context, req *coltracepb.ExportTraceServiceRequest) (*coltracepb.ExportTraceServiceResponse, error) {
	resp := &coltracepb.ExportTraceServiceResponse{}
	return resp, relayOTLP(ctx, r.emu, "traces", req, resp)
}

type otlpMetricsRelay struct {
	colmetricspb.UnimplementedMetricsServiceServer
	emu string
}

func (r otlpMetricsRelay) Export(ctx context.Context, req *colmetricspb.ExportMetricsServiceRequest) (*colmetricspb.ExportMetricsServiceResponse, error) {
	resp := &colmetricspb.ExportMetricsServiceResponse{}
	return resp, relayOTLP(ctx, r.emu, "metrics", req, resp)
}

type otlpLogsRelay struct {
	collogspb.UnimplementedLogsServiceServer
	emu string
}

func (r otlpLogsRelay) Export(ctx context.Context, req *collogspb.ExportLogsServiceRequest) (*collogspb.ExportLogsServiceResponse, error) {
	resp := &collogspb.ExportLogsServiceResponse{}
	return resp, relayOTLP(ctx, r.emu, "logs", req, resp)
}

// relayOTLP posts a gRPC export to the receiver as OTLP/HTTP and maps its
// answer back to a gRPC status.
func relayOTLP(ctx context.Context, emu, signal string, req, resp proto.Message) error {
	src := ""
	if p, ok := peer.FromContext(ctx); ok {
		src, _, _ = net.SplitHostPort(p.Addr.String())
	}
	body, err := proto.Marshal(req)
	if err != nil {
		return status.Error(codes.Internal, err.Error())
	}
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, emu+"/otlp/"+src+"/v1/"+signal, bytes.NewReader(body))
	if err != nil {
		return status.Error(codes.Internal, err.Error())
	}
	hr.Header.Set("Content-Type", "application/x-protobuf")
	hresp, err := http.DefaultClient.Do(hr)
	if err != nil {
		return status.Error(codes.Unavailable, "managed collector unavailable: "+err.Error())
	}
	defer hresp.Body.Close()
	out, err := io.ReadAll(io.LimitReader(hresp.Body, maxOTLPBody))
	if err != nil {
		return status.Error(codes.Unavailable, err.Error())
	}
	switch c := hresp.StatusCode; {
	case c == http.StatusOK:
		if err := proto.Unmarshal(out, resp); err != nil {
			return status.Error(codes.Internal, err.Error())
		}
		return nil
	case c == http.StatusBadRequest:
		return status.Error(codes.InvalidArgument, string(bytes.TrimSpace(out)))
	case c == http.StatusNotFound:
		return status.Error(codes.Unimplemented, "the cluster's managed OpenTelemetry is off")
	case c == http.StatusTooManyRequests || c == http.StatusBadGateway || c == http.StatusServiceUnavailable || c == http.StatusGatewayTimeout:
		return status.Error(codes.Unavailable, string(bytes.TrimSpace(out)))
	default:
		return status.Error(codes.Internal, hresp.Status+": "+string(bytes.TrimSpace(out)))
	}
}
