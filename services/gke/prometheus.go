package gke

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/binary"
	"encoding/json"
	"maps"
	"math"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/container/apiv1/containerpb"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/instrumentation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/resource"
)

// GKE managed Prometheus (monitoringConfig.managedPrometheusConfig,
// FR-GKE-020). While it is enabled the cluster has GMP's PodMonitoring and
// ClusterPodMonitoring CRDs (the managed-prometheus add-on, kept when it is
// disabled), and, with an OTLP endpoint set as for FR-GKE-014, the
// Instance scrapes the endpoints they select through the API server's pod
// proxy and exports the samples as OTLP metrics named as the OTel
// Collector's prometheus receiver names them by default (without its
// NormalizeName feature gate). Endpoints' authorization, basicAuth,
// oauth2, tls and proxyUrl, and limits, are not applied; the condition
// names them (ADR 0038).

const defaultScrapeInterval = time.Minute

// promMonitoring is a PodMonitoring or ClusterPodMonitoring, the part
// used.
type promMonitoring struct {
	Kind     string `json:"kind"`
	Metadata struct {
		objectMeta
		Generation int64 `json:"generation"`
	} `json:"metadata"`
	Spec struct {
		Selector      labelSelector `json:"selector"`
		FilterRunning *bool         `json:"filterRunning"`
		TargetLabels  struct {
			FromPod []struct {
				From string `json:"from"`
				To   string `json:"to"`
			} `json:"fromPod"`
			Metadata *[]string `json:"metadata"`
		} `json:"targetLabels"`
		Endpoints []struct {
			Port             intOrString         `json:"port"`
			Scheme           string              `json:"scheme"`
			Path             string              `json:"path"`
			Params           map[string][]string `json:"params"`
			Interval         string              `json:"interval"`
			Timeout          string              `json:"timeout"`
			MetricRelabeling []relabelRule       `json:"metricRelabeling"`
			// Not applied (ADR 0038): only reported.
			Authorization json.RawMessage `json:"authorization"`
			BasicAuth     json.RawMessage `json:"basicAuth"`
			OAuth2        json.RawMessage `json:"oauth2"`
			TLS           json.RawMessage `json:"tls"`
			ProxyURL      string          `json:"proxyUrl"`
		} `json:"endpoints"`
		Limits json.RawMessage `json:"limits"`
	} `json:"spec"`
	Status struct {
		ObservedGeneration int64 `json:"observedGeneration"`
	} `json:"status"`
}

// defaultTargetMetadata is targetLabels.metadata when unset, by kind.
var defaultTargetMetadata = map[string][]string{
	"PodMonitoring":        {"container", "pod", "top_level_controller_name", "top_level_controller_type"},
	"ClusterPodMonitoring": {"container", "namespace", "pod", "top_level_controller_name", "top_level_controller_type"},
}

// intOrString is a Kubernetes IntOrString, kept as written.
type intOrString string

func (v *intOrString) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		*v = intOrString(s)
		return nil
	}
	var n int
	if err := json.Unmarshal(b, &n); err != nil {
		return err
	}
	*v = intOrString(strconv.Itoa(n))
	return nil
}

type labelSelector struct {
	MatchLabels      map[string]string `json:"matchLabels"`
	MatchExpressions []struct {
		Key      string   `json:"key"`
		Operator string   `json:"operator"`
		Values   []string `json:"values"`
	} `json:"matchExpressions"`
}

// matches reports whether labels satisfy the selector; an empty selector
// matches everything.
func (s *labelSelector) matches(labels map[string]string) bool {
	for k, v := range s.MatchLabels {
		if got, ok := labels[k]; !ok || got != v {
			return false
		}
	}
	for _, e := range s.MatchExpressions {
		v, ok := labels[e.Key]
		switch e.Operator {
		case "In":
			if !ok || !slices.Contains(e.Values, v) {
				return false
			}
		case "NotIn":
			if ok && slices.Contains(e.Values, v) {
				return false
			}
		case "Exists":
			if !ok {
				return false
			}
		case "DoesNotExist":
			if ok {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// promPod is a pod as the scraper reads it.
type promPod struct {
	Metadata objectMeta `json:"metadata"`
	Spec     struct {
		NodeName   string `json:"nodeName"`
		Containers []struct {
			Name  string `json:"name"`
			Ports []struct {
				Name          string `json:"name"`
				ContainerPort int    `json:"containerPort"`
			} `json:"ports"`
		} `json:"containers"`
	} `json:"spec"`
	Status struct {
		Phase string `json:"phase"`
		PodIP string `json:"podIP"`
	} `json:"status"`
}

// promTarget is one endpoint of one selected pod.
type promTarget struct {
	id            string // monitoring, endpoint and pod: stable across discoveries
	job, instance string
	namespace     string
	pod, podUID   string
	podIP, node   string
	owner         []attribute.KeyValue // workloadAttrs
	scheme, path  string
	port          int
	params        url.Values
	interval      time.Duration
	timeout       time.Duration
	labels        map[string]string // target labels other than job and instance
	relabel       []relabelRule
}

// scrapeTargets returns the targets the monitorings select among pods;
// owners are the pods' ReplicaSets and Jobs by kind/namespace/name.
func scrapeTargets(mons []promMonitoring, pods []promPod, owners map[string]objectMeta) []promTarget {
	var out []promTarget
	for _, m := range mons {
		cluster := m.Kind == "ClusterPodMonitoring"
		filter := m.Spec.FilterRunning == nil || *m.Spec.FilterRunning
		meta := defaultTargetMetadata[m.Kind]
		if m.Spec.TargetLabels.Metadata != nil {
			meta = *m.Spec.TargetLabels.Metadata
		}
		for _, p := range pods {
			if !cluster && p.Metadata.Namespace != m.Metadata.Namespace || p.Status.PodIP == "" ||
				filter && (p.Status.Phase == "Failed" || p.Status.Phase == "Succeeded") ||
				!m.Spec.Selector.matches(p.Metadata.Labels) {
				continue
			}
			ctlName, ctlType := topLevelController(p.Metadata, owners)
			for i, ep := range m.Spec.Endpoints {
				port, container := resolvePort(&p, string(ep.Port))
				if port == 0 {
					continue
				}
				t := promTarget{
					id:        m.Kind + "/" + m.Metadata.Namespace + "/" + m.Metadata.Name + "/" + strconv.Itoa(i) + "/" + p.Metadata.UID,
					job:       m.Metadata.Name,
					instance:  p.Metadata.Name + ":" + string(ep.Port),
					namespace: p.Metadata.Namespace,
					pod:       p.Metadata.Name,
					podUID:    p.Metadata.UID,
					podIP:     p.Status.PodIP,
					node:      p.Spec.NodeName,
					scheme:    ep.Scheme,
					path:      ep.Path,
					port:      port,
					params:    url.Values(ep.Params),
					interval:  promDuration(ep.Interval, defaultScrapeInterval),
					labels:    map[string]string{},
					relabel:   ep.MetricRelabeling,
					owner:     workloadAttrs(p.Metadata, owners),
				}
				if ctlType == "DaemonSet" && p.Spec.NodeName != "" {
					t.instance = p.Spec.NodeName + ":" + string(ep.Port)
				}
				if t.scheme == "" {
					t.scheme = "http"
				}
				if t.path == "" {
					t.path = "/metrics"
				}
				t.timeout = min(promDuration(ep.Timeout, t.interval), t.interval)
				if !cluster {
					t.labels["namespace"] = p.Metadata.Namespace // GMP's protected label
				}
				for _, l := range meta {
					v := map[string]string{"container": container, "namespace": p.Metadata.Namespace, "pod": p.Metadata.Name, "node": p.Spec.NodeName,
						"top_level_controller_name": ctlName, "top_level_controller_type": ctlType}[l]
					if v != "" {
						t.labels[l] = v
					}
				}
				for _, fp := range m.Spec.TargetLabels.FromPod {
					to := fp.To
					if to == "" {
						to = fp.From
					}
					if v, ok := p.Metadata.Labels[fp.From]; ok {
						t.labels[to] = v
					}
				}
				out = append(out, t)
			}
		}
	}
	return out
}

// resolvePort returns the number of port (a number or a container port's
// name) and, for a name, its container.
func resolvePort(p *promPod, port string) (int, string) {
	if n, err := strconv.Atoi(port); err == nil {
		return n, ""
	}
	for _, c := range p.Spec.Containers {
		for _, cp := range c.Ports {
			if cp.Name == port {
				return cp.ContainerPort, c.Name
			}
		}
	}
	return 0, ""
}

// topLevelController returns the pod's top-level controller: the
// Deployment or CronJob above its ReplicaSet or Job where there is one.
func topLevelController(m objectMeta, owners map[string]objectMeta) (name, kind string) {
	for _, o := range m.OwnerReferences {
		name, kind = o.Name, o.Kind
		if o.Kind == "ReplicaSet" || o.Kind == "Job" {
			for _, oo := range owners[o.Kind+"/"+m.Namespace+"/"+o.Name].OwnerReferences {
				if oo.Kind == "Deployment" || oo.Kind == "CronJob" {
					name, kind = oo.Name, oo.Kind
				}
			}
		}
	}
	return name, kind
}

// promDuration parses a Prometheus duration ("30s", "1m30s", "1h"), or
// returns def when s is empty or invalid.
func promDuration(s string, def time.Duration) time.Duration {
	if s == "" {
		return def
	}
	var total time.Duration
	units := map[string]time.Duration{"ms": time.Millisecond, "s": time.Second, "m": time.Minute,
		"h": time.Hour, "d": 24 * time.Hour, "w": 7 * 24 * time.Hour, "y": 365 * 24 * time.Hour}
	for s != "" {
		i := 0
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
		}
		j := i
		for j < len(s) && (s[j] < '0' || s[j] > '9') {
			j++
		}
		n, err := strconv.Atoi(s[:i])
		u, ok := units[s[i:j]]
		if err != nil || !ok {
			return def
		}
		total += time.Duration(n) * u
		s = s[j:]
	}
	if total <= 0 {
		return def
	}
	return total
}

// relabelRule is a Prometheus relabeling rule as GMP's CRDs spell it.
type relabelRule struct {
	Action       string   `json:"action"`
	SourceLabels []string `json:"sourceLabels"`
	Separator    *string  `json:"separator"`
	Regex        *string  `json:"regex"`
	Modulus      uint64   `json:"modulus"`
	TargetLabel  string   `json:"targetLabel"`
	Replacement  *string  `json:"replacement"`
}

// apply applies the rule to the series' labels (with __name__), reporting
// false when the series is dropped.
func (r *relabelRule) apply(ls map[string]string) bool {
	sep, re, repl := ";", "(.*)", "$1"
	if r.Separator != nil {
		sep = *r.Separator
	}
	if r.Regex != nil {
		re = *r.Regex
	}
	if r.Replacement != nil {
		repl = *r.Replacement
	}
	rx, err := regexp.Compile("^(?:" + re + ")$")
	if err != nil {
		return true
	}
	vals := make([]string, len(r.SourceLabels))
	for i, l := range r.SourceLabels {
		vals[i] = ls[l]
	}
	val := strings.Join(vals, sep)
	switch r.Action {
	case "", "replace":
		m := rx.FindStringSubmatchIndex(val)
		if m == nil {
			return true
		}
		target := string(rx.ExpandString(nil, r.TargetLabel, val, m))
		if target == "" {
			return true
		}
		if v := string(rx.ExpandString(nil, repl, val, m)); v != "" {
			ls[target] = v
		} else {
			delete(ls, target)
		}
	case "lowercase":
		ls[r.TargetLabel] = strings.ToLower(val)
	case "uppercase":
		ls[r.TargetLabel] = strings.ToUpper(val)
	case "keep":
		return rx.MatchString(val)
	case "drop":
		return !rx.MatchString(val)
	case "keepequal":
		return ls[r.TargetLabel] == val
	case "dropequal":
		return ls[r.TargetLabel] != val
	case "hashmod":
		if r.Modulus > 0 {
			h := md5.Sum([]byte(val))
			ls[r.TargetLabel] = strconv.FormatUint(binary.BigEndian.Uint64(h[8:])%r.Modulus, 10)
		}
	case "labeldrop", "labelkeep":
		for l := range ls {
			if l != model.MetricNameLabel && rx.MatchString(l) == (r.Action == "labeldrop") {
				delete(ls, l)
			}
		}
	}
	return true
}

// promScrape is the outcome of one scrape: the body, or the error that
// failed it.
type promScrape struct {
	body  []byte
	err   error
	start time.Time // the target's first scrape: the start of its cumulative points
	at    time.Time
	took  time.Duration
}

// resourceMetrics converts a scrape of t into the metrics the OTel
// Collector's prometheus receiver would make of it: one resource for the
// target, with cluster's attributes and the target's, counters as
// cumulative monotonic sums, gauges and untyped samples as gauges,
// histograms and summaries as themselves, and the receiver's up and
// scrape_* metrics. Target labels (but job and instance, which are on
// the resource) are on every point; metricRelabeling sees a histogram or
// summary as one series named after its family.
func (t *promTarget) resourceMetrics(cluster []attribute.KeyValue, sc promScrape) metricdata.ResourceMetrics {
	attrs := append(slices.Clone(cluster),
		attribute.String("k8s.namespace.name", t.namespace),
		attribute.String("k8s.pod.name", t.pod),
		attribute.String("k8s.pod.uid", t.podUID),
		attribute.String("service.name", t.job),
		attribute.String("service.instance.id", t.instance),
		attribute.String("server.address", t.podIP),
		attribute.String("server.port", strconv.Itoa(t.port)),
		attribute.String("url.scheme", t.scheme),
	)
	if t.node != "" {
		attrs = append(attrs, attribute.String("k8s.node.name", t.node))
	}
	attrs = append(attrs, t.owner...)
	rm := metricdata.ResourceMetrics{Resource: resource.NewSchemaless(attrs...)}

	var ms []metricdata.Metrics
	up, scraped, kept := 0.0, 0, 0
	if sc.err == nil {
		var err error
		ms, scraped, kept, err = t.convert(sc)
		if err == nil {
			up = 1
		}
	}
	g := gauges{now: sc.at}
	g.float("up", "", "The scraping was successful", up)
	g.float("scrape_duration_seconds", "s", "Duration of the scrape", sc.took.Seconds())
	g.float("scrape_samples_scraped", "", "The number of samples the target exposed", float64(scraped))
	g.float("scrape_samples_post_metric_relabeling", "", "The number of samples remaining after metric relabeling was applied", float64(kept))
	ms = append(ms, g.ms...)
	rm.ScopeMetrics = []metricdata.ScopeMetrics{{Scope: instrumentation.Scope{Name: metricsScope}, Metrics: ms}}
	return rm
}

// convert parses the scraped body and converts the series that survive
// relabeling, counting the samples exposed and kept.
func (t *promTarget) convert(sc promScrape) (ms []metricdata.Metrics, scraped, kept int, err error) {
	for _, line := range bytes.Split(sc.body, []byte("\n")) {
		if l := bytes.TrimSpace(line); len(l) > 0 && l[0] != '#' {
			scraped++
		}
	}
	p := expfmt.NewTextParser(model.UTF8Validation)
	fams, err := p.TextToMetricFamilies(bytes.NewReader(sc.body))
	if err != nil {
		return nil, scraped, 0, err
	}
	byName := map[string]*metricdata.Metrics{}
	var order []string
	for _, n := range slices.Sorted(maps.Keys(fams)) {
		f := fams[n]
		for _, m := range f.GetMetric() {
			ls := map[string]string{model.MetricNameLabel: n, "job": t.job, "instance": t.instance}
			for k, v := range t.labels {
				ls[k] = v
			}
			for _, lp := range m.GetLabel() {
				ls[lp.GetName()] = lp.GetValue()
			}
			if !t.relabelSeries(ls) {
				continue
			}
			name := ls[model.MetricNameLabel]
			at := sc.at
			if m.TimestampMs != nil {
				at = time.UnixMilli(m.GetTimestampMs())
			}
			out := byName[name]
			if out == nil {
				out = &metricdata.Metrics{Name: name, Description: f.GetHelp()}
				byName[name] = out
				order = append(order, name)
			}
			kept += addSeries(out, f.GetType(), m, seriesAttrs(ls), sc.start, at)
		}
	}
	for _, n := range order {
		if byName[n].Data != nil {
			ms = append(ms, *byName[n])
		}
	}
	return ms, scraped, kept, nil
}

// relabelSeries applies the target's metricRelabeling, reporting false
// when the series is dropped or left without a name.
func (t *promTarget) relabelSeries(ls map[string]string) bool {
	for i := range t.relabel {
		if !t.relabel[i].apply(ls) {
			return false
		}
	}
	return ls[model.MetricNameLabel] != ""
}

// seriesAttrs are a series' point attributes: its labels but the name and
// those on the resource.
func seriesAttrs(ls map[string]string) attribute.Set {
	kvs := make([]attribute.KeyValue, 0, len(ls))
	for k, v := range ls {
		switch k {
		case model.MetricNameLabel, "job", "instance":
			continue
		}
		kvs = append(kvs, attribute.String(k, v))
	}
	return attribute.NewSet(kvs...)
}

// addSeries adds one series of a family of type typ to out as a point,
// returning the number of samples it holds. A series whose type differs
// from the points already in out, as after a relabeling merges two
// families, is dropped.
func addSeries(out *metricdata.Metrics, typ dto.MetricType, m *dto.Metric, attrs attribute.Set, start, at time.Time) int {
	switch typ {
	case dto.MetricType_COUNTER:
		sum, ok := out.Data.(metricdata.Sum[float64])
		if out.Data == nil {
			sum, ok = metricdata.Sum[float64]{Temporality: metricdata.CumulativeTemporality, IsMonotonic: true}, true
		}
		if !ok {
			return 0
		}
		sum.DataPoints = append(sum.DataPoints, metricdata.DataPoint[float64]{Attributes: attrs, StartTime: start, Time: at, Value: m.GetCounter().GetValue()})
		out.Data = sum
		return 1
	case dto.MetricType_GAUGE, dto.MetricType_UNTYPED:
		v := m.GetGauge().GetValue()
		if typ == dto.MetricType_UNTYPED {
			v = m.GetUntyped().GetValue()
		}
		g, ok := out.Data.(metricdata.Gauge[float64])
		if out.Data != nil && !ok {
			return 0
		}
		g.DataPoints = append(g.DataPoints, metricdata.DataPoint[float64]{Attributes: attrs, Time: at, Value: v})
		out.Data = g
		return 1
	case dto.MetricType_HISTOGRAM:
		h, ok := out.Data.(metricdata.Histogram[float64])
		if out.Data == nil {
			h, ok = metricdata.Histogram[float64]{Temporality: metricdata.CumulativeTemporality}, true
		}
		if !ok {
			return 0
		}
		ph := m.GetHistogram()
		dp := metricdata.HistogramDataPoint[float64]{Attributes: attrs, StartTime: start, Time: at,
			Count: ph.GetSampleCount(), Sum: ph.GetSampleSum()}
		var prev uint64
		for _, b := range ph.GetBucket() {
			if math.IsInf(b.GetUpperBound(), 1) {
				continue
			}
			dp.Bounds = append(dp.Bounds, b.GetUpperBound())
			dp.BucketCounts = append(dp.BucketCounts, b.GetCumulativeCount()-prev)
			prev = b.GetCumulativeCount()
		}
		dp.BucketCounts = append(dp.BucketCounts, dp.Count-prev)
		h.DataPoints = append(h.DataPoints, dp)
		out.Data = h
		return len(ph.GetBucket()) + 2
	case dto.MetricType_SUMMARY:
		s, ok := out.Data.(metricdata.Summary)
		if out.Data != nil && !ok {
			return 0
		}
		ps := m.GetSummary()
		dp := metricdata.SummaryDataPoint{Attributes: attrs, StartTime: start, Time: at, Count: ps.GetSampleCount(), Sum: ps.GetSampleSum()}
		for _, q := range ps.GetQuantile() {
			dp.QuantileValues = append(dp.QuantileValues, metricdata.QuantileValue{Quantile: q.GetQuantile(), Value: q.GetValue()})
		}
		s.DataPoints = append(s.DataPoints, dp)
		out.Data = s
		return len(ps.GetQuantile()) + 2
	}
	return 0
}

const (
	promSyncInterval = 2 * time.Second
	monitoringPath   = "/apis/monitoring.googleapis.com/v1"
)

func managedPrometheusOn(c *containerpb.Cluster) bool {
	return c.GetMonitoringConfig().GetManagedPrometheusConfig().GetEnabled()
}

// promState is a target's scrape schedule.
type promState struct {
	start time.Time // first scrape
	next  time.Time
	busy  bool
}

// prometheusScraper scrapes one cluster's targets, each every interval,
// and exports the results with exp (nil: status only).
type prometheusScraper struct {
	s   *Service
	key string
	exp sdkmetric.Exporter
	wg  sync.WaitGroup
	mu  sync.Mutex
	st  map[string]*promState // by target id
}

// prometheusLoop runs the cluster's managed Prometheus while it is
// enabled: it marks new PodMonitorings and ClusterPodMonitorings
// configured, as GMP's operator does, and, with an OTLP endpoint set,
// scrapes their targets.
func (s *Service) prometheusLoop(ctx context.Context, key string) {
	ps := &prometheusScraper{s: s, key: key, st: map[string]*promState{}}
	if exp, _, ok := s.otlpExporter(ctx, key, "managed Prometheus metrics"); ok {
		ps.exp = exp
		defer shutdownExporter(exp)
	}
	defer ps.wg.Wait()
	t := time.NewTicker(promSyncInterval)
	defer t.Stop()
	for {
		if err := ps.sync(ctx); err != nil && ctx.Err() == nil {
			s.env.Log.Debug("gke: managed Prometheus", "cluster", key, "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// sync updates the monitorings' status and starts the scrapes that are
// due.
func (ps *prometheusScraper) sync(ctx context.Context) error {
	rec, err := ps.s.loadKey(ps.key)
	if err != nil {
		return err
	}
	if !managedPrometheusOn(rec.cluster()) {
		return nil
	}
	kc, err := ps.s.kube(ps.key)
	if err != nil {
		return err
	}
	var mons []promMonitoring
	for _, kind := range []struct{ kind, resource string }{{"PodMonitoring", "podmonitorings"}, {"ClusterPodMonitoring", "clusterpodmonitorings"}} {
		var list kubeList[promMonitoring]
		if err := kc.get(ctx, monitoringPath+"/"+kind.resource, &list); err != nil {
			if isKubeNotFound(err) {
				return nil // the add-on is not installed yet
			}
			return err
		}
		for _, m := range list.Items {
			m.Kind = kind.kind
			if m.Status.ObservedGeneration != m.Metadata.Generation {
				if err := markConfigured(ctx, kc, kind.resource, &m); err != nil {
					ps.s.env.Log.Debug("gke: managed Prometheus status", "cluster", ps.key, "name", m.Metadata.Name, "err", err)
				}
			}
			mons = append(mons, m)
		}
	}
	if ps.exp == nil || len(mons) == 0 {
		return nil
	}
	var pods kubeList[promPod]
	if err := kc.get(ctx, "/api/v1/pods", &pods); err != nil {
		return err
	}
	targets := scrapeTargets(mons, pods.Items, ps.s.workloadOwners(ctx, kc, ps.key))
	cluster := clusterResourceAttrs(rec)
	now := time.Now()
	ps.mu.Lock()
	defer ps.mu.Unlock()
	seen := map[string]bool{}
	for _, t := range targets {
		seen[t.id] = true
		st := ps.st[t.id]
		if st == nil {
			st = &promState{start: now, next: now}
			ps.st[t.id] = st
		}
		if st.busy || now.Before(st.next) {
			continue
		}
		st.busy, st.next = true, now.Add(t.interval)
		ps.wg.Add(1)
		go func() {
			defer ps.wg.Done()
			ps.scrape(ctx, kc, t, cluster, st.start)
			ps.mu.Lock()
			st.busy = false
			ps.mu.Unlock()
		}()
	}
	for id := range ps.st {
		if !seen[id] {
			delete(ps.st, id)
		}
	}
	return nil
}

// scrape scrapes t through the API server's pod proxy and exports the
// result.
func (ps *prometheusScraper) scrape(ctx context.Context, kc *kubeClient, t promTarget, cluster []attribute.KeyValue, start time.Time) {
	sctx, cancel := context.WithTimeout(ctx, t.timeout)
	defer cancel()
	path := "/api/v1/namespaces/" + t.namespace + "/pods/" + t.scheme + ":" + t.pod + ":" + strconv.Itoa(t.port) + "/proxy" + t.path
	if len(t.params) > 0 {
		path += "?" + t.params.Encode()
	}
	sc := promScrape{start: start, at: time.Now()}
	sc.err = kc.get(sctx, path, &sc.body)
	sc.took = time.Since(sc.at)
	if ctx.Err() != nil {
		return
	}
	rm := t.resourceMetrics(cluster, sc)
	if err := ps.exp.Export(ctx, &rm); err != nil && ctx.Err() == nil {
		ps.s.env.Log.Debug("gke: managed Prometheus export", "cluster", ps.key, "target", t.id, "err", err)
	}
}

// markConfigured sets the condition GMP's operator sets on a
// PodMonitoring or ClusterPodMonitoring whose configuration it took, for
// the current generation.
func markConfigured(ctx context.Context, kc *kubeClient, resource string, m *promMonitoring) error {
	path := monitoringPath + "/"
	if m.Metadata.Namespace != "" {
		path += "namespaces/" + m.Metadata.Namespace + "/"
	}
	return kc.mergePatch(ctx, path+resource+"/"+m.Metadata.Name+"/status", map[string]any{"status": configuredStatus(m, time.Now())})
}

// configuredStatus is m's status once configured; the condition's message
// names the settings the Instance does not apply (ADR 0038).
func configuredStatus(m *promMonitoring, now time.Time) map[string]any {
	ts := now.UTC().Format(time.RFC3339)
	cond := map[string]any{"type": "ConfigurationCreateSuccess", "status": "True", "lastUpdateTime": ts, "lastTransitionTime": ts}
	var ignored []string
	for i, ep := range m.Spec.Endpoints {
		for _, f := range []struct {
			name string
			set  bool
		}{
			{"authorization", len(ep.Authorization) > 0}, {"basicAuth", len(ep.BasicAuth) > 0}, {"oauth2", len(ep.OAuth2) > 0},
			{"tls", len(ep.TLS) > 0}, {"proxyUrl", ep.ProxyURL != ""},
		} {
			if f.set {
				ignored = append(ignored, "endpoints["+strconv.Itoa(i)+"]."+f.name)
			}
		}
	}
	if len(m.Spec.Limits) > 0 {
		ignored = append(ignored, "limits")
	}
	if len(ignored) > 0 {
		cond["message"] = "Not applied by the emulator: " + strings.Join(ignored, ", ") + "."
	}
	return map[string]any{"observedGeneration": m.Metadata.Generation, "conditions": []map[string]any{cond}}
}
