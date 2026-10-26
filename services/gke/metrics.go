package gke

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/container/apiv1/containerpb"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/sdk/instrumentation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/resource"
)

// GKE system metrics (monitoringConfig.componentConfig SYSTEM_COMPONENTS)
// over OTLP. Where GKE sends its system metrics to Cloud Monitoring, the
// Instance exports them to the OTLP endpoint of the standard OTel env vars
// in its own environment (OTEL_EXPORTER_OTLP_[METRICS_]ENDPOINT,
// _PROTOCOL, _HEADERS and so on, OTEL_METRIC_EXPORT_INTERVAL), read when
// the cluster's controllers start; with no endpoint the setting is only
// recorded. The Instance reads the kubelets' stats summaries and the API
// server itself, so nothing runs in the cluster. Names follow the OTel Collector's kubeletstats and k8s_cluster
// receivers: one resource per node, pod and container, carrying the
// cluster, namespace, pod, container and owning workload.

const (
	metricsScope           = "github.com/linuxuser586/gcpemu/services/gke"
	defaultMetricsInterval = time.Minute
)

// otlpSettings are the exporter settings the emulator itself chooses; the
// exporters read the rest (endpoint, headers, TLS, timeout) from the env.
type otlpSettings struct {
	protocol string // "grpc" or "http/protobuf"
	interval time.Duration
}

// otlpFromEnv returns the OTLP metrics settings, or false when no endpoint
// is configured.
func otlpFromEnv(getenv func(string) string) (otlpSettings, bool) {
	t, ok := otlpTargetFromEnv(getenv, "metrics")
	if !ok {
		return otlpSettings{}, false
	}
	set := otlpSettings{protocol: t.protocol, interval: defaultMetricsInterval}
	if ms, err := strconv.Atoi(getenv("OTEL_METRIC_EXPORT_INTERVAL")); err == nil && ms > 0 {
		set.interval = time.Duration(ms) * time.Millisecond
	}
	return set, true
}

func newOTLPExporter(ctx context.Context, protocol string) (sdkmetric.Exporter, error) {
	switch protocol {
	case "grpc":
		return otlpmetricgrpc.New(ctx)
	case "http/protobuf":
		return otlpmetrichttp.New(ctx)
	}
	return nil, fmt.Errorf("unsupported OTLP protocol %q (want grpc or http/protobuf)", protocol)
}

// otlpExporter returns an exporter to the OTLP endpoint and its settings,
// or false when no endpoint is set or it can't be made (logging what, the
// metrics not exported).
func (s *Service) otlpExporter(ctx context.Context, key, what string) (sdkmetric.Exporter, otlpSettings, bool) {
	set, ok := otlpFromEnv(os.Getenv)
	if !ok {
		return nil, set, false
	}
	exp, err := newOTLPExporter(ctx, set.protocol)
	if err != nil {
		s.env.Log.Warn("gke: not exporting "+what, "cluster", key, "err", err)
		return nil, set, false
	}
	return exp, set, true
}

func shutdownExporter(exp sdkmetric.Exporter) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = exp.Shutdown(ctx)
}

func systemComponentsOn(c *containerpb.Cluster) bool {
	return slices.Contains(c.GetMonitoringConfig().GetComponentConfig().GetEnableComponents(),
		containerpb.MonitoringComponentConfig_SYSTEM_COMPONENTS)
}

// systemMetricsLoop exports the cluster's system metrics every export
// interval while SYSTEM_COMPONENTS is enabled; with no OTLP endpoint it
// returns at once.
func (s *Service) systemMetricsLoop(ctx context.Context, key string) {
	exp, set, ok := s.otlpExporter(ctx, key, "system metrics")
	if !ok {
		return
	}
	defer shutdownExporter(exp)
	t := time.NewTicker(set.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if err := s.exportSystemMetrics(ctx, key, exp); err != nil && ctx.Err() == nil {
			s.env.Log.Debug("gke: system metrics", "cluster", key, "err", err)
		}
	}
}

// Kubernetes objects as the metrics read them.
type (
	metricsNode struct {
		Metadata objectMeta `json:"metadata"`
		Status   struct {
			Allocatable map[string]string `json:"allocatable"`
		} `json:"status"`
	}
	metricsPod struct {
		Metadata objectMeta `json:"metadata"`
		Spec     struct {
			NodeName   string `json:"nodeName"`
			Containers []struct {
				Name      string `json:"name"`
				Resources struct {
					Requests map[string]string `json:"requests"`
					Limits   map[string]string `json:"limits"`
				} `json:"resources"`
			} `json:"containers"`
		} `json:"spec"`
		Status struct {
			ContainerStatuses []struct {
				Name         string `json:"name"`
				RestartCount int64  `json:"restartCount"`
				LastState    struct {
					Terminated *struct {
						Reason string `json:"reason"`
					} `json:"terminated"`
				} `json:"lastState"`
			} `json:"containerStatuses"`
		} `json:"status"`
	}
	metricsOwned struct {
		Metadata objectMeta `json:"metadata"`
	}
	// kubeletStats is the kubelet's /stats/summary, the part used.
	kubeletStats struct {
		Node struct {
			kubeletUsage
		} `json:"node"`
		Pods []kubeletPod `json:"pods"`
	}
	kubeletPod struct {
		PodRef struct {
			UID string `json:"uid"`
		} `json:"podRef"`
		kubeletUsage
		Containers []struct {
			Name string `json:"name"`
			kubeletUsage
		} `json:"containers"`
	}
	kubeletUsage struct {
		CPU struct {
			UsageNanoCores *uint64 `json:"usageNanoCores"`
		} `json:"cpu"`
		Memory struct {
			WorkingSetBytes *uint64 `json:"workingSetBytes"`
		} `json:"memory"`
	}
)

// exportSystemMetrics reads the cluster's usage and exports it, one
// resource at a time, when SYSTEM_COMPONENTS is enabled. It stops at the
// first failed export, so an unreachable backend costs one timeout.
func (s *Service) exportSystemMetrics(ctx context.Context, key string, exp sdkmetric.Exporter) error {
	rec, err := s.loadKey(key)
	if err != nil {
		return err
	}
	c := rec.cluster()
	if !systemComponentsOn(c) {
		return nil
	}
	kc, err := s.kube(key)
	if err != nil {
		return err
	}
	var (
		nodes kubeList[metricsNode]
		pods  kubeList[metricsPod]
	)
	if err := kc.get(ctx, "/api/v1/nodes", &nodes); err != nil {
		return err
	}
	if err := kc.get(ctx, "/api/v1/pods", &pods); err != nil {
		return err
	}
	stats := map[string]*kubeletStats{}
	for _, n := range nodes.Items {
		var st kubeletStats
		if err := kc.get(ctx, "/api/v1/nodes/"+n.Metadata.Name+"/proxy/stats/summary", &st); err != nil {
			s.env.Log.Debug("gke: kubelet stats", "cluster", key, "node", n.Metadata.Name, "err", err)
			continue
		}
		stats[n.Metadata.Name] = &st
	}

	now := time.Now()
	clusterAttrs := clusterResourceAttrs(rec)
	var out []metricdata.ResourceMetrics
	emit := func(attrs []attribute.KeyValue, ms []metricdata.Metrics) {
		if len(ms) == 0 {
			return
		}
		out = append(out, metricdata.ResourceMetrics{
			Resource:     resource.NewSchemaless(append(slices.Clone(clusterAttrs), attrs...)...),
			ScopeMetrics: []metricdata.ScopeMetrics{{Scope: instrumentation.Scope{Name: metricsScope}, Metrics: ms}},
		})
	}
	g := gauges{now: now}

	for _, n := range nodes.Items {
		g.ms = nil
		if st := stats[n.Metadata.Name]; st != nil {
			g.usage("k8s.node", &st.Node.kubeletUsage)
		}
		g.quantity("k8s.node.allocatable_cpu", "{cpu}", "Allocatable CPU", n.Status.Allocatable["cpu"])
		g.quantity("k8s.node.allocatable_memory", "By", "Allocatable memory", n.Status.Allocatable["memory"])
		emit([]attribute.KeyValue{attribute.String("k8s.node.name", n.Metadata.Name)}, g.ms)
	}

	owners := s.workloadOwners(ctx, kc, key)
	for _, p := range pods.Items {
		m := p.Metadata
		podAttrs := []attribute.KeyValue{
			attribute.String("k8s.namespace.name", m.Namespace),
			attribute.String("k8s.pod.name", m.Name),
			attribute.String("k8s.pod.uid", m.UID),
		}
		if p.Spec.NodeName != "" {
			podAttrs = append(podAttrs, attribute.String("k8s.node.name", p.Spec.NodeName))
		}
		podAttrs = append(podAttrs, workloadAttrs(m, owners)...)

		var podStats *kubeletPod
		if st := stats[p.Spec.NodeName]; st != nil {
			for i := range st.Pods {
				if st.Pods[i].PodRef.UID == m.UID {
					podStats = &st.Pods[i]
				}
			}
		}
		g.ms = nil
		if podStats != nil {
			g.usage("k8s.pod", &podStats.kubeletUsage)
		}
		emit(podAttrs, g.ms)

		for _, ctr := range p.Spec.Containers {
			g.ms = nil
			attrs := append(slices.Clone(podAttrs), attribute.String("k8s.container.name", ctr.Name))
			if podStats != nil {
				for i := range podStats.Containers {
					if podStats.Containers[i].Name == ctr.Name {
						g.usage("container", &podStats.Containers[i].kubeletUsage)
					}
				}
			}
			rs := ctr.Resources
			g.quantity("k8s.container.cpu_request", "{cpu}", "CPU requested", rs.Requests["cpu"])
			g.quantity("k8s.container.cpu_limit", "{cpu}", "CPU limit", rs.Limits["cpu"])
			g.quantity("k8s.container.memory_request", "By", "Memory requested", rs.Requests["memory"])
			g.quantity("k8s.container.memory_limit", "By", "Memory limit", rs.Limits["memory"])
			for _, cs := range p.Status.ContainerStatuses {
				if cs.Name != ctr.Name {
					continue
				}
				g.int("k8s.container.restarts", "{restart}", "Restarts of the container", cs.RestartCount)
				if t := cs.LastState.Terminated; t != nil && t.Reason != "" {
					attrs = append(attrs, attribute.String("k8s.container.status.last_terminated_reason", t.Reason))
				}
			}
			emit(attrs, g.ms)
		}
	}

	for i := range out {
		if err := exp.Export(ctx, &out[i]); err != nil {
			return err
		}
	}
	return nil
}

// clusterResourceAttrs are the resource attributes naming the cluster.
func clusterResourceAttrs(rec *clusterRecord) []attribute.KeyValue {
	attrs := []attribute.KeyValue{
		attribute.String("cloud.provider", "gcp"),
		attribute.String("cloud.platform", "gcp_kubernetes_engine"),
		attribute.String("cloud.account.id", rec.Int.Project),
		attribute.String("k8s.cluster.name", rec.Int.Name),
	}
	if isZone(rec.Int.Location) {
		return append(attrs, attribute.String("cloud.availability_zone", rec.Int.Location))
	}
	return append(attrs, attribute.String("cloud.region", rec.Int.Location))
}

// workloadOwners returns the cluster's ReplicaSets and Jobs by
// kind/namespace/name, for workloadAttrs. Without them, pods only lose
// their Deployment and CronJob.
func (s *Service) workloadOwners(ctx context.Context, kc *kubeClient, key string) map[string]objectMeta {
	owners := map[string]objectMeta{}
	for kind, path := range map[string]string{"ReplicaSet": "/apis/apps/v1/replicasets", "Job": "/apis/batch/v1/jobs"} {
		var list kubeList[metricsOwned]
		if err := kc.get(ctx, path, &list); err != nil {
			s.env.Log.Debug("gke: workload owners", "cluster", key, "path", path, "err", err)
			continue
		}
		for _, o := range list.Items {
			owners[kind+"/"+o.Metadata.Namespace+"/"+o.Metadata.Name] = o.Metadata
		}
	}
	return owners
}

// workloadAttrs names the workload that owns a pod: its ReplicaSet and
// Deployment, StatefulSet, DaemonSet, or Job and CronJob.
func workloadAttrs(m objectMeta, owners map[string]objectMeta) []attribute.KeyValue {
	var out []attribute.KeyValue
	add := func(kind, name string) {
		out = append(out, attribute.String("k8s."+strings.ToLower(kind)+".name", name))
	}
	for _, o := range m.OwnerReferences {
		switch o.Kind {
		case "ReplicaSet", "Job":
			add(o.Kind, o.Name)
			for _, oo := range owners[o.Kind+"/"+m.Namespace+"/"+o.Name].OwnerReferences {
				if oo.Kind == "Deployment" || oo.Kind == "CronJob" {
					add(oo.Kind, oo.Name)
				}
			}
		case "StatefulSet", "DaemonSet":
			add(o.Kind, o.Name)
		}
	}
	return out
}

// gauges collects one resource's gauge metrics.
type gauges struct {
	now time.Time
	ms  []metricdata.Metrics
}

func (g *gauges) float(name, unit, desc string, v float64) {
	g.ms = append(g.ms, metricdata.Metrics{Name: name, Unit: unit, Description: desc,
		Data: metricdata.Gauge[float64]{DataPoints: []metricdata.DataPoint[float64]{{Time: g.now, Value: v}}}})
}

func (g *gauges) int(name, unit, desc string, v int64) {
	g.ms = append(g.ms, metricdata.Metrics{Name: name, Unit: unit, Description: desc,
		Data: metricdata.Gauge[int64]{DataPoints: []metricdata.DataPoint[int64]{{Time: g.now, Value: v}}}})
}

// usage adds prefix.cpu.usage and prefix.memory.working_set, where the
// kubelet reports them.
func (g *gauges) usage(prefix string, u *kubeletUsage) {
	if v := u.CPU.UsageNanoCores; v != nil {
		g.float(prefix+".cpu.usage", "{cpu}", "CPU in use", float64(*v)/1e9)
	}
	if v := u.Memory.WorkingSetBytes; v != nil {
		g.int(prefix+".memory.working_set", "By", "Memory working set", int64(*v))
	}
}

// quantity adds a Kubernetes quantity, if set and valid: whole bytes for
// unit By, otherwise cores.
func (g *gauges) quantity(name, unit, desc, q string) {
	if q == "" {
		return
	}
	v, err := parseQuantity(q)
	if err != nil {
		return
	}
	if unit == "By" {
		g.int(name, unit, desc, int64(v))
	} else {
		g.float(name, unit, desc, v)
	}
}

// quantitySuffixes are Kubernetes' quantity suffixes, binary first so
// that "Mi" is not read as "M".
var quantitySuffixes = []struct {
	s   string
	mul float64
	div float64 // for the fractional suffixes, which are inexact as multipliers
}{
	{"Ki", 1 << 10, 1}, {"Mi", 1 << 20, 1}, {"Gi", 1 << 30, 1}, {"Ti", 1 << 40, 1}, {"Pi", 1 << 50, 1}, {"Ei", 1 << 60, 1},
	{"n", 1, 1e9}, {"u", 1, 1e6}, {"m", 1, 1e3}, {"k", 1e3, 1}, {"M", 1e6, 1}, {"G", 1e9, 1}, {"T", 1e12, 1}, {"P", 1e15, 1}, {"E", 1e18, 1},
}

var quantityNum = regexp.MustCompile(`^[+-]?(\d+\.?\d*|\.\d+)([eE][+-]?\d+)?$`)

// parseQuantity parses a Kubernetes resource quantity ("250m", "64Mi",
// "1e3").
func parseQuantity(q string) (float64, error) {
	num, mul, div := q, 1.0, 1.0
	for _, sf := range quantitySuffixes {
		if strings.HasSuffix(q, sf.s) {
			num, mul, div = strings.TrimSuffix(q, sf.s), sf.mul, sf.div
			break
		}
	}
	if !quantityNum.MatchString(num) {
		return 0, fmt.Errorf("invalid quantity %q", q)
	}
	v, err := strconv.ParseFloat(num, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid quantity %q", q)
	}
	return v * mul / div, nil
}
