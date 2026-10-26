// Command otlpapp is the workload of the GKE Managed collector test
// (FR-GKE-019). It sends one trace, one metric and one log to GKE's
// in-cluster collector address over OTLP, as PROTOCOL (http or grpc)
// says, and prints OTLPAPP OK once each export has succeeded.
package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/proto"
)

const collector = "opentelemetry-collector.gke-managed-otel.svc.cluster.local"

func main() {
	protocol := os.Getenv("PROTOCOL")
	var err error
	for i := 0; i < 30; i++ {
		if err = run(protocol); err == nil {
			fmt.Println("OTLPAPP OK")
			return
		}
		fmt.Println("OTLPAPP RETRY:", err)
		time.Sleep(2 * time.Second)
	}
	fmt.Println("OTLPAPP FAILED:", err)
	os.Exit(1)
}

func resource() *resourcepb.Resource {
	return &resourcepb.Resource{Attributes: []*commonpb.KeyValue{{Key: "service.name",
		Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "otlpapp-" + os.Getenv("PROTOCOL")}}}}}
}

func run(protocol string) error {
	now := uint64(time.Now().UnixNano())
	traces := &coltracepb.ExportTraceServiceRequest{ResourceSpans: []*tracepb.ResourceSpans{{Resource: resource(),
		ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{{Name: "checkout",
			TraceId: bytes.Repeat([]byte{1}, 16), SpanId: bytes.Repeat([]byte{2}, 8), StartTimeUnixNano: now, EndTimeUnixNano: now}}}}}}}
	metrics := &colmetricspb.ExportMetricsServiceRequest{ResourceMetrics: []*metricspb.ResourceMetrics{{Resource: resource(),
		ScopeMetrics: []*metricspb.ScopeMetrics{{Metrics: []*metricspb.Metric{{Name: "orders", Data: &metricspb.Metric_Gauge{
			Gauge: &metricspb.Gauge{DataPoints: []*metricspb.NumberDataPoint{{TimeUnixNano: now, Value: &metricspb.NumberDataPoint_AsInt{AsInt: 1}}}}}}}}}}}}
	logs := &collogspb.ExportLogsServiceRequest{ResourceLogs: []*logspb.ResourceLogs{{Resource: resource(),
		ScopeLogs: []*logspb.ScopeLogs{{LogRecords: []*logspb.LogRecord{{TimeUnixNano: now, SeverityText: "INFO",
			Body: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "order placed"}}}}}}}}}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	switch protocol {
	case "http":
		for path, m := range map[string]proto.Message{"/v1/traces": traces, "/v1/metrics": metrics, "/v1/logs": logs} {
			if err := post(ctx, "http://"+collector+":4318"+path, m); err != nil {
				return err
			}
		}
		return nil
	case "grpc":
		cc, err := grpc.NewClient(collector+":4317", grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			return err
		}
		defer cc.Close()
		if _, err := coltracepb.NewTraceServiceClient(cc).Export(ctx, traces); err != nil {
			return fmt.Errorf("traces: %w", err)
		}
		if _, err := colmetricspb.NewMetricsServiceClient(cc).Export(ctx, metrics); err != nil {
			return fmt.Errorf("metrics: %w", err)
		}
		if _, err := collogspb.NewLogsServiceClient(cc).Export(ctx, logs); err != nil {
			return fmt.Errorf("logs: %w", err)
		}
		return nil
	}
	return fmt.Errorf("PROTOCOL %q: want http or grpc", protocol)
}

func post(ctx context.Context, url string, m proto.Message) error {
	b, err := proto.Marshal(m)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-protobuf")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s %s", url, resp.Status, body)
	}
	return nil
}
