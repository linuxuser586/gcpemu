package gke

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
)

// Instrumentation objects (FR-GKE-021). An Instrumentation
// (telemetry.googleapis.com) selects pods of its namespace, and GKE sets
// the standard OTel SDK variables in their containers when they are
// created, pointing at the Managed collector (collector.go). Here a
// mutating webhook that the Instance serves through the Google front end,
// as it serves the CA injector's (cainject.go), sets them while the cluster's Managed
// OpenTelemetry scope asks for instrumentation. The variables and their
// values are those "Managed OpenTelemetry for GKE" lists; no Recording of
// a cluster with the scope exists yet (ADR 0023).

// managedCollectorURL is where GKE points a pod's OTel SDK: the Managed
// collector's OTLP/HTTP port at GKE's in-cluster collector address.
var managedCollectorURL = fmt.Sprintf("http://opentelemetry-collector.gke-managed-otel.svc.cluster.local:%d", otlpHTTPPort)

// podUIDAttribute is the resource attribute GKE gives a hostNetwork pod,
// from the K8S_POD_UID variable it also sets.
const podUIDAttribute = "k8s.pod.uid=$(K8S_POD_UID)"

const (
	hookInstrument      = "instrument" // sets GKE's variables in a new pod
	instrumentationName = "gcpemu-managed-otel-instrumentation"
	telemetryAPI        = "/apis/telemetry.googleapis.com/v1alpha1"
)

// installInstrumentationWebhook registers (or updates) the webhook setting
// GKE's variables in a running cluster. It fails open: while the Instance
// is unreachable, pods are created unconfigured. Errors are logged.
func (s *Service) installInstrumentationWebhook(ctx context.Context, d *deps, key string) {
	hooks, kc, ok := s.hookBase(ctx, d, key, "Instrumentation objects configure no pods")
	if !ok {
		return
	}
	if err := applyObject(ctx, kc, "/apis/admissionregistration.k8s.io/v1/mutatingwebhookconfigurations", instrumentationName,
		instrumentationWebhook(hooks+hookInstrument, s.env.CA.PEM())); err != nil {
		s.env.Log.Warn("gke: registering the Instrumentation webhook failed", "cluster", key, "err", err)
	}
}

// instrumentationWebhook is the MutatingWebhookConfiguration, on new pods
// outside kube-system.
func instrumentationWebhook(url string, caPEM []byte) map[string]any {
	return map[string]any{
		"apiVersion": "admissionregistration.k8s.io/v1",
		"kind":       "MutatingWebhookConfiguration",
		"metadata": map[string]any{
			"name":   instrumentationName,
			"labels": map[string]string{"app.kubernetes.io/managed-by": "gcpemu"},
		},
		"webhooks": []map[string]any{{
			"name":                    "instrumentation.telemetry.gcpemu.dev",
			"admissionReviewVersions": []string{"v1"},
			"sideEffects":             "None",
			"failurePolicy":           "Ignore",
			"timeoutSeconds":          10,
			"reinvocationPolicy":      "IfNeeded",
			"clientConfig":            map[string]any{"url": url, "caBundle": base64.StdEncoding.EncodeToString(caPEM)},
			"rules": []map[string]any{{
				"apiGroups": []string{""}, "apiVersions": []string{"v1"},
				"operations": []string{"CREATE"}, "resources": []string{"pods"}, "scope": "Namespaced",
			}},
			"namespaceSelector": map[string]any{"matchExpressions": []map[string]any{
				{"key": "kubernetes.io/metadata.name", "operator": "NotIn", "values": []string{kubeSystem}},
			}},
		}},
	}
}

// serveInstrumentation answers the Instrumentation webhook: a new pod that
// an Instrumentation of its namespace selects gets GKE's variables while
// the cluster's Managed OpenTelemetry scope is on. Without the scope, or
// when the Instrumentations cannot be read, the pod is admitted unchanged.
func (s *Service) serveInstrumentation(w http.ResponseWriter, r *http.Request, key string) {
	var ar admissionReview
	if err := json.NewDecoder(r.Body).Decode(&ar); err != nil || ar.Request == nil {
		http.Error(w, "bad AdmissionReview", http.StatusBadRequest)
		return
	}
	req := ar.Request
	resp := &admissionResponse{UID: req.UID, Allowed: true}
	var pod instrumentedPod
	if rec, err := s.loadKey(key); err == nil && managedOTelOn(rec.cluster()) &&
		req.Operation == "CREATE" && json.Unmarshal(req.Object, &pod) == nil {
		var list struct {
			Items []instrumentationObject `json:"items"`
		}
		if kc, err := s.kube(key); err == nil {
			if err := kc.get(r.Context(), telemetryAPI+"/namespaces/"+req.Namespace+"/instrumentations", &list); err != nil && !isKubeNotFound(err) {
				s.env.Log.Warn("gke: reading Instrumentation objects failed; the pod is not configured", "cluster", key, "namespace", req.Namespace, "err", err)
			}
		}
		if ops := instrumentationPatch(&pod, list.Items); len(ops) > 0 {
			b, _ := json.Marshal(ops)
			resp.PatchType, resp.Patch = "JSONPatch", base64.StdEncoding.EncodeToString(b)
		}
	}
	writeJSON(w, admissionReview{APIVersion: "admission.k8s.io/v1", Kind: "AdmissionReview", Response: resp})
}

// instrumentationObject is an Instrumentation object as injection reads it.
type instrumentationObject struct {
	Metadata objectMeta `json:"metadata"`
	Spec     struct {
		Selector                  labelSelector `json:"selector"`
		AutoInstrumentationConfig struct {
			ConfigInjection struct {
				Enabled bool `json:"enabled"`
			} `json:"configInjection"`
		} `json:"autoInstrumentationConfig"`
		// OTelSDKConfig is kept raw by provider, since a provider set to
		// null turns its signal off where one left out does not.
		OTelSDKConfig    map[string]json.RawMessage `json:"otelSDKConfig"`
		PromptsResponses struct {
			UploadBasePath string `json:"uploadBasePath"`
		} `json:"promptsResponses"`
	} `json:"spec"`
}

// sdkVars are the variables GKE sets for an Instrumentation's
// otelSDKConfig, in the order "Managed OpenTelemetry for GKE" lists them:
// the sampler (parent-based, always on unless a ratio is set), the metric
// export interval (30 s by default) and each signal's exporter, none for a
// null provider.
func sdkVars(sdk map[string]json.RawMessage) []envVar {
	var tracer struct {
		Sampler struct {
			ParentBased struct {
				Root struct {
					TraceIDRatioBased *struct {
						Ratio string `json:"ratio"`
					} `json:"trace_id_ratio_based"`
				} `json:"root"`
			} `json:"parent_based"`
		} `json:"sampler"`
	}
	var meter struct {
		Readers []struct {
			Periodic struct {
				Interval int `json:"interval"`
			} `json:"periodic"`
		} `json:"readers"`
	}
	_ = json.Unmarshal(sdk["tracer_provider"], &tracer)
	_ = json.Unmarshal(sdk["meter_provider"], &meter)
	sampler, ratio := "parentbased_always_on", "1.0"
	if r := tracer.Sampler.ParentBased.Root.TraceIDRatioBased; r != nil && r.Ratio != "" {
		sampler, ratio = "parentbased_traceidratio", r.Ratio
	}
	interval := 30000
	if len(meter.Readers) > 0 && meter.Readers[0].Periodic.Interval > 0 {
		interval = meter.Readers[0].Periodic.Interval
	}
	exporter := func(provider string) string {
		if string(sdk[provider]) == "null" {
			return "none"
		}
		return "otlp"
	}
	return []envVar{
		{Name: "OTEL_TRACES_SAMPLER", Value: sampler},
		{Name: "OTEL_TRACES_SAMPLER_ARG", Value: ratio},
		{Name: "OTEL_METRIC_EXPORT_INTERVAL", Value: strconv.Itoa(interval)},
		{Name: "OTEL_TRACES_EXPORTER", Value: exporter("tracer_provider")},
		{Name: "OTEL_METRICS_EXPORTER", Value: exporter("meter_provider")},
		{Name: "OTEL_LOGS_EXPORTER", Value: exporter("logger_provider")},
	}
}

// envVar is a container's env entry.
type envVar struct {
	Name      string          `json:"name"`
	Value     string          `json:"value,omitempty"`
	ValueFrom json.RawMessage `json:"valueFrom,omitempty"`
}

// envContainer is a container or init container as injection reads it.
type envContainer struct {
	Name          string   `json:"name"`
	RestartPolicy string   `json:"restartPolicy,omitempty"`
	Env           []envVar `json:"env,omitempty"`
}

// instrumentedPod is a pod as injection reads it.
type instrumentedPod struct {
	Metadata objectMeta `json:"metadata"`
	Spec     struct {
		HostNetwork    bool           `json:"hostNetwork"`
		InitContainers []envContainer `json:"initContainers"`
		Containers     []envContainer `json:"containers"`
	} `json:"spec"`
}

// instrumentationPatch returns the JSON patch setting GKE's variables in
// pod's containers, given the Instrumentation objects of its namespace, or
// nil when none selects it.
func instrumentationPatch(pod *instrumentedPod, insts []instrumentationObject) []patchOp {
	var chosen *instrumentationObject
	for i := range insts {
		if insts[i].Spec.Selector.selects(pod.Metadata.Labels) && (chosen == nil || newer(&insts[i], chosen)) {
			chosen = &insts[i]
		}
	}
	if chosen == nil || !chosen.Spec.AutoInstrumentationConfig.ConfigInjection.Enabled {
		return nil
	}
	vars := append([]envVar{{Name: "OTEL_EXPORTER_OTLP_ENDPOINT", Value: managedCollectorURL}}, sdkVars(chosen.Spec.OTelSDKConfig)...)
	if base := chosen.Spec.PromptsResponses.UploadBasePath; base != "" {
		vars = append(vars,
			envVar{Name: "OTEL_INSTRUMENTATION_GENAI_UPLOAD_FORMAT", Value: "jsonl"},
			envVar{Name: "OTEL_INSTRUMENTATION_GENAI_COMPLETION_HOOK", Value: "upload"},
			envVar{Name: "OTEL_SEMCONV_STABILITY_OPT_IN", Value: "gen_ai_latest_experimental"},
			envVar{Name: "OTEL_INSTRUMENTATION_GENAI_UPLOAD_BASE_PATH", Value: base})
	}
	if pod.Spec.HostNetwork {
		// The pod shares its node's address, so the Managed collector
		// cannot tell it by its sender's.
		vars = append(vars,
			envVar{Name: "K8S_POD_UID", ValueFrom: json.RawMessage(`{"fieldRef":{"apiVersion":"v1","fieldPath":"metadata.uid"}}`)},
			envVar{Name: "OTEL_RESOURCE_ATTRIBUTES", Value: podUIDAttribute})
	}
	var ops []patchOp
	add := func(path string, c envContainer) {
		if env := withVars(c.Env, vars); !slices.EqualFunc(env, c.Env, sameVar) {
			ops = append(ops, patchOp{Op: "add", Path: path + "/env", Value: env})
		}
	}
	for i, c := range pod.Spec.InitContainers {
		if c.RestartPolicy == "Always" { // a native sidecar
			add("/spec/initContainers/"+strconv.Itoa(i), c)
		}
	}
	for i, c := range pod.Spec.Containers {
		add("/spec/containers/"+strconv.Itoa(i), c)
	}
	return ops
}

func sameVar(a, b envVar) bool {
	return a.Name == b.Name && a.Value == b.Value && bytes.Equal(a.ValueFrom, b.ValueFrom)
}

// withVars is env with vars set: the entries of env that vars names are
// dropped, as GKE's automatic configuration overrides the pod's own, and
// vars follow the rest. The pod's own OTEL_RESOURCE_ATTRIBUTES are the
// exception: they follow GKE's k8s.pod.uid, so its other attributes stay.
func withVars(env, vars []envVar) []envVar {
	vars = slices.Clone(vars)
	for i, v := range vars {
		if v.Name != "OTEL_RESOURCE_ATTRIBUTES" {
			continue
		}
		for _, e := range env {
			if e.Name == v.Name && e.Value != "" && e.Value != podUIDAttribute {
				vars[i].Value = podUIDAttribute + "," + strings.TrimPrefix(e.Value, podUIDAttribute+",")
			}
		}
	}
	out := slices.DeleteFunc(slices.Clone(env), func(e envVar) bool {
		return slices.ContainsFunc(vars, func(v envVar) bool { return v.Name == e.Name })
	})
	return append(out, vars...)
}

// newer reports whether a was updated after b. Each write gives an
// object a new resourceVersion, which k3s's datastore numbers in order.
func newer(a, b *instrumentationObject) bool {
	ra, _ := strconv.ParseUint(a.Metadata.ResourceVersion, 10, 64)
	rb, _ := strconv.ParseUint(b.Metadata.ResourceVersion, 10, 64)
	return ra > rb
}

// labelSelector is a Kubernetes label selector; an empty one selects
// everything.
type labelSelector struct {
	MatchLabels      map[string]string `json:"matchLabels"`
	MatchExpressions []struct {
		Key      string   `json:"key"`
		Operator string   `json:"operator"`
		Values   []string `json:"values"`
	} `json:"matchExpressions"`
}

// selects reports whether the selector selects labels.
func (s *labelSelector) selects(labels map[string]string) bool {
	for k, v := range s.MatchLabels {
		if got, ok := labels[k]; !ok || got != v {
			return false
		}
	}
	for _, e := range s.MatchExpressions {
		v, ok := labels[e.Key]
		var match bool
		switch e.Operator {
		case "In":
			match = ok && slices.Contains(e.Values, v)
		case "NotIn":
			match = !ok || !slices.Contains(e.Values, v)
		case "Exists":
			match = ok
		case "DoesNotExist":
			match = !ok
		}
		if !match {
			return false
		}
	}
	return true
}
