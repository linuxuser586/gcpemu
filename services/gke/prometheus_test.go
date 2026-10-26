package gke

import (
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// decodeJSON decodes s, as the API server would return it, into a T.
func decodeJSON[T any](t *testing.T, s string) T {
	t.Helper()
	var v T
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

const promPodsJSON = `{"items": [
  {"metadata": {"name": "web-1", "namespace": "a", "uid": "u1", "labels": {"app": "web", "team": "red"},
     "ownerReferences": [{"kind": "ReplicaSet", "name": "web-5d8f"}]},
   "spec": {"nodeName": "n1", "containers": [{"name": "srv", "ports": [{"name": "metrics", "containerPort": 8080}]}]},
   "status": {"phase": "Running", "podIP": "10.0.0.1"}},
  {"metadata": {"name": "web-2", "namespace": "b", "uid": "u2", "labels": {"app": "web"}},
   "spec": {"nodeName": "n1", "containers": [{"name": "srv", "ports": [{"name": "metrics", "containerPort": 8080}]}]},
   "status": {"phase": "Running", "podIP": "10.0.0.2"}},
  {"metadata": {"name": "web-3", "namespace": "a", "uid": "u3", "labels": {"app": "web"}},
   "spec": {"containers": [{"name": "srv", "ports": [{"name": "metrics", "containerPort": 8080}]}]},
   "status": {"phase": "Succeeded", "podIP": "10.0.0.3"}},
  {"metadata": {"name": "web-4", "namespace": "a", "uid": "u4", "labels": {"app": "web"}},
   "spec": {"containers": [{"name": "srv", "ports": [{"name": "metrics", "containerPort": 8080}]}]},
   "status": {"phase": "Pending"}},
  {"metadata": {"name": "db", "namespace": "a", "uid": "u5", "labels": {"app": "db"}},
   "spec": {"containers": [{"name": "pg", "ports": [{"name": "metrics", "containerPort": 9187}]}]},
   "status": {"phase": "Running", "podIP": "10.0.0.5"}}
]}`

// FR-GKE-020: a PodMonitoring selects running pods with an IP in its own
// namespace, a ClusterPodMonitoring across namespaces; each endpoint of
// each selected pod is a target labelled as GMP labels it.
func TestScrapeTargets(t *testing.T) {
	pods := decodeJSON[kubeList[promPod]](t, promPodsJSON).Items
	pm := decodeJSON[kubeList[promMonitoring]](t, `{"items": [{"kind": "PodMonitoring",
	  "metadata": {"name": "web", "namespace": "a"},
	  "spec": {"selector": {"matchLabels": {"app": "web"}},
	    "endpoints": [{"port": "metrics", "interval": "10s", "path": "/stats", "params": {"x": ["1"]}}]}}]}`).Items
	cpm := decodeJSON[kubeList[promMonitoring]](t, `{"items": [{"kind": "ClusterPodMonitoring",
	  "metadata": {"name": "all-web"},
	  "spec": {"selector": {"matchExpressions": [{"key": "app", "operator": "In", "values": ["web"]}]},
	    "targetLabels": {"fromPod": [{"from": "team", "to": "owner"}], "metadata": ["pod", "node"]},
	    "endpoints": [{"port": 8080, "interval": "30s", "timeout": "5s"}]}}]}`).Items
	owners := map[string]objectMeta{"ReplicaSet/a/web-5d8f": {Name: "web-5d8f", Namespace: "a",
		OwnerReferences: []struct {
			Kind string `json:"kind"`
			Name string `json:"name"`
		}{{Kind: "Deployment", Name: "web"}}}}

	got := scrapeTargets(append(pm, cpm...), pods, owners)
	byInstance := map[string]promTarget{}
	for _, tg := range got {
		byInstance[tg.job+"/"+tg.instance] = tg
	}
	if len(got) != 3 {
		t.Fatalf("targets = %v, want web/web-1:metrics, all-web/web-1:8080, all-web/web-2:8080", slices.Collect(maps.Keys(byInstance)))
	}

	web, ok := byInstance["web/web-1:metrics"]
	if !ok {
		t.Fatalf("no target web/web-1:metrics in %v", slices.Collect(maps.Keys(byInstance)))
	}
	if web.namespace != "a" || web.pod != "web-1" || web.podIP != "10.0.0.1" || web.port != 8080 ||
		web.path != "/stats" || web.params.Get("x") != "1" || web.scheme != "http" ||
		web.interval != 10*time.Second || web.timeout != 10*time.Second {
		t.Errorf("web target = %+v", web)
	}
	wantLabels := map[string]string{"container": "srv", "pod": "web-1", "namespace": "a",
		"top_level_controller_name": "web", "top_level_controller_type": "Deployment"}
	if !maps.Equal(web.labels, wantLabels) {
		t.Errorf("web target labels = %v, want %v", web.labels, wantLabels)
	}

	all, ok := byInstance["all-web/web-2:8080"]
	if !ok {
		t.Fatalf("no target all-web/web-2:8080 in %v", slices.Collect(maps.Keys(byInstance)))
	}
	if all.namespace != "b" || all.port != 8080 || all.path != "/metrics" || all.interval != 30*time.Second || all.timeout != 5*time.Second {
		t.Errorf("all-web target = %+v", all)
	}
	// A numeric port names no container; team is not on web-2; the
	// metadata leave out namespace.
	if want := map[string]string{"pod": "web-2", "node": "n1"}; !maps.Equal(all.labels, want) {
		t.Errorf("all-web web-2 labels = %v, want %v", all.labels, want)
	}
	cpmDefault := decodeJSON[kubeList[promMonitoring]](t, `{"items": [{"kind": "ClusterPodMonitoring",
	  "metadata": {"name": "dflt"},
	  "spec": {"selector": {"matchLabels": {"app": "web"}}, "endpoints": [{"port": 8080, "interval": "30s"}]}}]}`).Items
	if l := scrapeTargets(cpmDefault, pods[1:2], nil)[0].labels; l["namespace"] != "b" {
		t.Errorf("default ClusterPodMonitoring labels = %v, want namespace=b", l)
	}
	if l := byInstance["all-web/web-1:8080"].labels; l["owner"] != "red" {
		t.Errorf("all-web web-1 labels = %v, want owner=red from the pod's team label", l)
	}
}

const promBody = `# HELP http_requests_total Requests served.
# TYPE http_requests_total counter
http_requests_total{code="200",secret="x"} 7
http_requests_total{code="500",secret="y"} 1
# TYPE queue_depth gauge
queue_depth 3
# TYPE latency_seconds histogram
latency_seconds_bucket{le="0.1"} 2
latency_seconds_bucket{le="1"} 5
latency_seconds_bucket{le="+Inf"} 6
latency_seconds_sum 2.5
latency_seconds_count 6
# TYPE rpc_seconds summary
rpc_seconds{quantile="0.5"} 0.2
rpc_seconds_sum 4
rpc_seconds_count 10
# TYPE go_gc_count counter
go_gc_count 12
legacy_thing{kind="a"} 9
`

// FR-GKE-020: a scrape exports one resource for the target, carrying
// FR-GKE-014's attributes and the prometheus receiver's job and instance,
// with each family named and typed as the receiver makes it, target
// labels on every point, metricRelabeling applied, and the up and
// scrape_* metrics the receiver adds.
func TestPromTargetMetrics(t *testing.T) {
	start := time.Unix(1000, 0)
	at := start.Add(time.Minute)
	tg := promTarget{job: "web", instance: "web-1:metrics", namespace: "a", pod: "web-1", podUID: "u1",
		podIP: "10.0.0.1", node: "n1", owner: []attribute.KeyValue{attribute.String("k8s.replicaset.name", "web-5d8f"), attribute.String("k8s.deployment.name", "web")},
		scheme: "http", port: 8080, labels: map[string]string{"container": "srv", "pod": "web-1", "namespace": "a"},
		relabel: decodeJSON[[]relabelRule](t, `[
		  {"action": "drop", "sourceLabels": ["__name__"], "regex": "go_.*"},
		  {"action": "labeldrop", "regex": "secret"},
		  {"sourceLabels": ["kind"], "targetLabel": "variant", "replacement": "v-$1"}]`)}
	cluster := []attribute.KeyValue{attribute.String("k8s.cluster.name", "c1")}

	rm := tg.resourceMetrics(cluster, promScrape{body: []byte(promBody), start: start, at: at, took: 20 * time.Millisecond})

	res := map[string]string{}
	for _, kv := range rm.Resource.Attributes() {
		res[string(kv.Key)] = kv.Value.Emit()
	}
	for k, v := range map[string]string{"k8s.cluster.name": "c1", "k8s.namespace.name": "a", "k8s.pod.name": "web-1",
		"k8s.pod.uid": "u1", "k8s.node.name": "n1", "k8s.deployment.name": "web", "service.name": "web",
		"service.instance.id": "web-1:metrics", "server.address": "10.0.0.1", "server.port": "8080", "url.scheme": "http"} {
		if res[k] != v {
			t.Errorf("resource %s = %q, want %q (all: %v)", k, res[k], v, res)
		}
	}
	ms := map[string]metricdata.Metrics{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			ms[m.Name] = m
		}
	}
	if _, ok := ms["go_gc_count"]; ok {
		t.Error("go_gc_count exported, want it dropped by metricRelabeling")
	}

	sum, ok := ms["http_requests_total"].Data.(metricdata.Sum[float64])
	if !ok || !sum.IsMonotonic || sum.Temporality != metricdata.CumulativeTemporality || len(sum.DataPoints) != 2 {
		t.Fatalf("http_requests_total = %+v, want a monotonic cumulative sum of 2 points", ms["http_requests_total"])
	}
	if ms["http_requests_total"].Description != "Requests served." {
		t.Errorf("description = %q", ms["http_requests_total"].Description)
	}
	for _, dp := range sum.DataPoints {
		code, _ := dp.Attributes.Value("code")
		ctr, _ := dp.Attributes.Value("container")
		ns, _ := dp.Attributes.Value("namespace")
		_, secret := dp.Attributes.Value("secret")
		_, job := dp.Attributes.Value("job")
		if secret || job || ctr.AsString() != "srv" || ns.AsString() != "a" || !dp.StartTime.Equal(start) || !dp.Time.Equal(at) {
			t.Errorf("http_requests_total point %v: attrs %v, start %v, time %v", dp.Value, dp.Attributes.ToSlice(), dp.StartTime, dp.Time)
		}
		if want := map[string]float64{"200": 7, "500": 1}[code.AsString()]; dp.Value != want {
			t.Errorf("http_requests_total{code=%q} = %v, want %v", code.AsString(), dp.Value, want)
		}
	}

	if g, ok := ms["queue_depth"].Data.(metricdata.Gauge[float64]); !ok || g.DataPoints[0].Value != 3 {
		t.Errorf("queue_depth = %+v", ms["queue_depth"])
	}
	if g, ok := ms["legacy_thing"].Data.(metricdata.Gauge[float64]); !ok || len(g.DataPoints) != 1 {
		t.Errorf("untyped legacy_thing = %+v, want a gauge", ms["legacy_thing"])
	} else if v, _ := g.DataPoints[0].Attributes.Value("variant"); v.AsString() != "v-a" {
		t.Errorf("legacy_thing attrs = %v, want variant=v-a from the replace rule", g.DataPoints[0].Attributes.ToSlice())
	}

	h, ok := ms["latency_seconds"].Data.(metricdata.Histogram[float64])
	if !ok || len(h.DataPoints) != 1 {
		t.Fatalf("latency_seconds = %+v, want one histogram point", ms["latency_seconds"])
	}
	hp := h.DataPoints[0]
	if !slices.Equal(hp.Bounds, []float64{0.1, 1}) || !slices.Equal(hp.BucketCounts, []uint64{2, 3, 1}) || hp.Count != 6 || hp.Sum != 2.5 {
		t.Errorf("latency_seconds point: bounds %v counts %v count %d sum %v", hp.Bounds, hp.BucketCounts, hp.Count, hp.Sum)
	}

	s, ok := ms["rpc_seconds"].Data.(metricdata.Summary)
	if !ok || len(s.DataPoints) != 1 || s.DataPoints[0].Count != 10 || s.DataPoints[0].Sum != 4 ||
		len(s.DataPoints[0].QuantileValues) != 1 || s.DataPoints[0].QuantileValues[0] != (metricdata.QuantileValue{Quantile: 0.5, Value: 0.2}) {
		t.Errorf("rpc_seconds = %+v", ms["rpc_seconds"])
	}

	for name, want := range map[string]float64{"up": 1, "scrape_duration_seconds": 0.02, "scrape_samples_scraped": 13, "scrape_samples_post_metric_relabeling": 12} {
		g, ok := ms[name].Data.(metricdata.Gauge[float64])
		if !ok || g.DataPoints[0].Value != want {
			t.Errorf("%s = %+v, want %v", name, ms[name], want)
		}
	}
}

// FR-GKE-020: a failed scrape exports only up = 0, as the receiver does.
func TestPromTargetMetricsFailed(t *testing.T) {
	tg := promTarget{job: "web", instance: "web-1:metrics", scheme: "http", port: 8080}
	rm := tg.resourceMetrics(nil, promScrape{err: errors.New("connection refused"), at: time.Unix(1000, 0)})
	var names []string
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			names = append(names, m.Name)
			if m.Name == "up" && m.Data.(metricdata.Gauge[float64]).DataPoints[0].Value != 0 {
				t.Errorf("up = %+v, want 0", m)
			}
		}
	}
	if !slices.Contains(names, "up") || slices.ContainsFunc(names, func(n string) bool { return n != "up" && !strings.HasPrefix(n, "scrape_") }) {
		t.Errorf("metrics = %v, want up and the scrape_ metrics only", names)
	}
}

// FR-GKE-020: the condition marking a PodMonitoring configured names the
// endpoint settings the Instance does not apply (ADR 0036), and says
// nothing when there are none.
func TestConfiguredStatus(t *testing.T) {
	cond := func(spec string) map[string]any {
		t.Helper()
		m := decodeJSON[promMonitoring](t, `{"metadata": {"name": "web", "generation": 3}, "spec": `+spec+`}`)
		st := configuredStatus(&m, time.Unix(0, 0))
		if st["observedGeneration"] != int64(3) {
			t.Errorf("observedGeneration = %v, want 3", st["observedGeneration"])
		}
		c := st["conditions"].([]map[string]any)[0]
		if c["type"] != "ConfigurationCreateSuccess" || c["status"] != "True" {
			t.Errorf("condition = %v", c)
		}
		return c
	}
	if c := cond(`{"endpoints": [{"port": 8080}]}`); c["message"] != nil {
		t.Errorf("message = %q, want none", c["message"])
	}
	c := cond(`{"limits": {"samples": 10}, "endpoints": [{"port": 8080, "tls": {"insecureSkipVerify": true}},
	  {"port": "m", "basicAuth": {"username": "u"}, "proxyUrl": "http://p"}]}`)
	want := "Not applied by the emulator: endpoints[0].tls, endpoints[1].basicAuth, endpoints[1].proxyUrl, limits."
	if c["message"] != want {
		t.Errorf("message = %q, want %q", c["message"], want)
	}
}
