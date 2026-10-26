package gke

import (
	"encoding/json"
	"fmt"
	"maps"
	"strings"
	"testing"
)

// instrumentedEnv runs instrumentationPatch on pod with the namespace's
// Instrumentation objects insts (a JSON array) and returns each patched
// container's env, by patch path, as JSON.
func instrumentedEnv(t *testing.T, pod, insts string) map[string]string {
	t.Helper()
	var p instrumentedPod
	if err := json.Unmarshal([]byte(pod), &p); err != nil {
		t.Fatal(err)
	}
	var is []instrumentationObject
	if err := json.Unmarshal([]byte(insts), &is); err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, op := range instrumentationPatch(&p, is) {
		if op.Op != "add" {
			t.Fatalf("op %+v, want add", op)
		}
		b, err := json.Marshal(op.Value)
		if err != nil {
			t.Fatal(err)
		}
		out[op.Path] = string(b)
	}
	return out
}

// gkeDefaults are the variables GKE sets in a selected container when the
// Instrumentation leaves otelSDKConfig unset, as "Managed OpenTelemetry for
// GKE" lists them.
const gkeDefaults = `{"name":"OTEL_EXPORTER_OTLP_ENDPOINT","value":"http://opentelemetry-collector.gke-managed-otel.svc.cluster.local:4318"},` +
	`{"name":"OTEL_TRACES_SAMPLER","value":"parentbased_always_on"},` +
	`{"name":"OTEL_TRACES_SAMPLER_ARG","value":"1.0"},` +
	`{"name":"OTEL_METRIC_EXPORT_INTERVAL","value":"30000"},` +
	`{"name":"OTEL_TRACES_EXPORTER","value":"otlp"},` +
	`{"name":"OTEL_METRICS_EXPORTER","value":"otlp"},` +
	`{"name":"OTEL_LOGS_EXPORTER","value":"otlp"}`

// FR-GKE-021: an Instrumentation with configInjection enabled sets GKE's
// variables in the containers of the pods it selects.
func TestInstrumentationDefaults(t *testing.T) {
	got := instrumentedEnv(t,
		`{"metadata":{"labels":{"app":"shop"}},"spec":{"containers":[{"name":"app"}]}}`,
		`[{"spec":{"selector":{"matchLabels":{"app":"shop"}},"autoInstrumentationConfig":{"configInjection":{"enabled":true}}}}]`)
	want := `[` + gkeDefaults + `]`
	if got["/spec/containers/0/env"] != want || len(got) != 1 {
		t.Errorf("patch = %v\nwant /spec/containers/0/env = %s", got, want)
	}
}

// FR-GKE-021: otelSDKConfig sets the sampler and the metric export
// interval, and a null provider turns its signal's exporter off, as GKE's
// examples do.
func TestInstrumentationSDKConfig(t *testing.T) {
	const pod = `{"spec":{"containers":[{"name":"app"}]}}`
	inst := func(sdk string) string {
		return `[{"spec":{"selector":{},"autoInstrumentationConfig":{"configInjection":{"enabled":true}},"otelSDKConfig":` + sdk + `}}]`
	}
	for _, tc := range []struct{ name, sdk, want string }{
		{"sampling and interval", `{"tracer_provider":{"sampler":{"parent_based":{"root":{"trace_id_ratio_based":{"ratio":"0.25"}}}}},
			"meter_provider":{"readers":[{"periodic":{"interval":60000}}]}}`,
			`{"name":"OTEL_EXPORTER_OTLP_ENDPOINT","value":"http://opentelemetry-collector.gke-managed-otel.svc.cluster.local:4318"},` +
				`{"name":"OTEL_TRACES_SAMPLER","value":"parentbased_traceidratio"},` +
				`{"name":"OTEL_TRACES_SAMPLER_ARG","value":"0.25"},` +
				`{"name":"OTEL_METRIC_EXPORT_INTERVAL","value":"60000"},` +
				`{"name":"OTEL_TRACES_EXPORTER","value":"otlp"},` +
				`{"name":"OTEL_METRICS_EXPORTER","value":"otlp"},` +
				`{"name":"OTEL_LOGS_EXPORTER","value":"otlp"}`},
		{"traces only", `{"meter_provider":null,"logger_provider":null}`,
			`{"name":"OTEL_EXPORTER_OTLP_ENDPOINT","value":"http://opentelemetry-collector.gke-managed-otel.svc.cluster.local:4318"},` +
				`{"name":"OTEL_TRACES_SAMPLER","value":"parentbased_always_on"},` +
				`{"name":"OTEL_TRACES_SAMPLER_ARG","value":"1.0"},` +
				`{"name":"OTEL_METRIC_EXPORT_INTERVAL","value":"30000"},` +
				`{"name":"OTEL_TRACES_EXPORTER","value":"otlp"},` +
				`{"name":"OTEL_METRICS_EXPORTER","value":"none"},` +
				`{"name":"OTEL_LOGS_EXPORTER","value":"none"}`},
		{"no traces", `{"tracer_provider":null}`,
			`{"name":"OTEL_EXPORTER_OTLP_ENDPOINT","value":"http://opentelemetry-collector.gke-managed-otel.svc.cluster.local:4318"},` +
				`{"name":"OTEL_TRACES_SAMPLER","value":"parentbased_always_on"},` +
				`{"name":"OTEL_TRACES_SAMPLER_ARG","value":"1.0"},` +
				`{"name":"OTEL_METRIC_EXPORT_INTERVAL","value":"30000"},` +
				`{"name":"OTEL_TRACES_EXPORTER","value":"none"},` +
				`{"name":"OTEL_METRICS_EXPORTER","value":"otlp"},` +
				`{"name":"OTEL_LOGS_EXPORTER","value":"otlp"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := instrumentedEnv(t, pod, inst(tc.sdk))
			if want := `[` + tc.want + `]`; got["/spec/containers/0/env"] != want {
				t.Errorf("env = %s\nwant %s", got["/spec/containers/0/env"], want)
			}
		})
	}
}

// FR-GKE-021: an Instrumentation selects pods by label selector, an empty
// one selecting every pod of its namespace. When several select a pod, the
// most recently updated takes effect, as GKE documents, even one whose
// injection is paused. A pod none selects is unchanged.
func TestInstrumentationSelection(t *testing.T) {
	inst := func(rv, selector string, enabled bool, ratio string) string {
		return fmt.Sprintf(`{"metadata":{"resourceVersion":%q},"spec":{"selector":%s,"autoInstrumentationConfig":{"configInjection":{"enabled":%t}},`+
			`"otelSDKConfig":{"tracer_provider":{"sampler":{"parent_based":{"root":{"trace_id_ratio_based":{"ratio":%q}}}}}}}}`, rv, selector, enabled, ratio)
	}
	shop := `{"metadata":{"labels":{"app":"shop","tier":"web"}},"spec":{"containers":[{"name":"app"}]}}`
	for _, tc := range []struct {
		name, pod string
		insts     []string
		ratio     string // OTEL_TRACES_SAMPLER_ARG, or "" for no patch
	}{
		{"match labels", shop, []string{inst("1", `{"matchLabels":{"app":"shop"}}`, true, "0.5")}, "0.5"},
		{"other labels", shop, []string{inst("1", `{"matchLabels":{"app":"cart"}}`, true, "0.5")}, ""},
		{"no labels", `{"spec":{"containers":[{"name":"app"}]}}`, []string{inst("1", `{"matchLabels":{"app":"shop"}}`, true, "0.5")}, ""},
		{"empty selector", `{"spec":{"containers":[{"name":"app"}]}}`, []string{inst("1", `{}`, true, "0.5")}, "0.5"},
		{"no Instrumentation", shop, nil, ""},
		{"match expressions", shop, []string{inst("1", `{"matchExpressions":[
			{"key":"app","operator":"In","values":["shop","cart"]},
			{"key":"tier","operator":"NotIn","values":["db"]},
			{"key":"tier","operator":"Exists"},
			{"key":"canary","operator":"DoesNotExist"}]}`, true, "0.5")}, "0.5"},
		{"failed expression", shop, []string{inst("1", `{"matchExpressions":[{"key":"canary","operator":"Exists"}]}`, true, "0.5")}, ""},
		{"newest wins", shop, []string{
			inst("120", `{"matchLabels":{"app":"shop"}}`, true, "0.1"),
			inst("90", `{}`, true, "0.9"),
			inst("130", `{"matchLabels":{"app":"cart"}}`, true, "0.3"),
		}, "0.1"},
		{"newest paused", shop, []string{inst("12", `{}`, false, "0.9"), inst("11", `{}`, true, "0.1")}, ""},
		{"paused", shop, []string{inst("1", `{}`, false, "0.5")}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := instrumentedEnv(t, tc.pod, "["+strings.Join(tc.insts, ",")+"]")
			if tc.ratio == "" {
				if len(got) != 0 {
					t.Errorf("patch = %v, want none", got)
				}
				return
			}
			if want := `{"name":"OTEL_TRACES_SAMPLER_ARG","value":"` + tc.ratio + `"}`; !strings.Contains(got["/spec/containers/0/env"], want) {
				t.Errorf("env = %s, want %s", got["/spec/containers/0/env"], want)
			}
		})
	}
}

// selectAll is an Instrumentation selecting every pod of its namespace.
const selectAll = `[{"spec":{"selector":{},"autoInstrumentationConfig":{"configInjection":{"enabled":true}}}}]`

// FR-GKE-021: a pod's own value of a variable GKE sets is replaced by
// GKE's, since GKE documents that its automatic configuration overrides
// manual configuration. The pod's other variables, and their order, stay;
// GKE's follow them.
func TestInstrumentationOverridesPodVariables(t *testing.T) {
	got := instrumentedEnv(t, `{"spec":{"containers":[{"name":"app","env":[
		{"name":"OTEL_SERVICE_NAME","value":"shop"},
		{"name":"OTEL_EXPORTER_OTLP_ENDPOINT","value":"http://my-collector:4318"},
		{"name":"OTEL_LOGS_EXPORTER","value":"none"},
		{"name":"POD_IP","valueFrom":{"fieldRef":{"fieldPath":"status.podIP"}}}]}]}}`, selectAll)
	want := `[{"name":"OTEL_SERVICE_NAME","value":"shop"},` +
		`{"name":"POD_IP","valueFrom":{"fieldRef":{"fieldPath":"status.podIP"}}},` + gkeDefaults + `]`
	if got["/spec/containers/0/env"] != want {
		t.Errorf("env = %s\nwant %s", got["/spec/containers/0/env"], want)
	}
}

// FR-GKE-021: GKE's variables go to the pod's containers and native
// sidecars (init containers that keep running), not to init containers
// that run to completion.
func TestInstrumentationContainers(t *testing.T) {
	got := instrumentedEnv(t, `{"spec":{
		"initContainers":[{"name":"migrate"},{"name":"proxy","restartPolicy":"Always"}],
		"containers":[{"name":"app"},{"name":"worker","env":[{"name":"QUEUE","value":"orders"}]}]}}`, selectAll)
	want := map[string]string{
		"/spec/initContainers/1/env": `[` + gkeDefaults + `]`,
		"/spec/containers/0/env":     `[` + gkeDefaults + `]`,
		"/spec/containers/1/env":     `[{"name":"QUEUE","value":"orders"},` + gkeDefaults + `]`,
	}
	if !maps.Equal(got, want) {
		t.Errorf("patch = %v\nwant %v", got, want)
	}
}

// FR-GKE-021: a hostNetwork pod shares its node's address, so GKE also
// names it by UID in OTEL_RESOURCE_ATTRIBUTES, from K8S_POD_UID.
func TestInstrumentationHostNetwork(t *testing.T) {
	got := instrumentedEnv(t, `{"spec":{"hostNetwork":true,"containers":[{"name":"app"}]}}`, selectAll)
	want := `[` + gkeDefaults + `,{"name":"K8S_POD_UID","valueFrom":{"fieldRef":{"apiVersion":"v1","fieldPath":"metadata.uid"}}},` +
		`{"name":"OTEL_RESOURCE_ATTRIBUTES","value":"k8s.pod.uid=$(K8S_POD_UID)"}]`
	if got["/spec/containers/0/env"] != want {
		t.Errorf("env = %s\nwant %s", got["/spec/containers/0/env"], want)
	}
}

// FR-GKE-021: promptsResponses.uploadBasePath adds GKE's variables for
// uploading generative AI prompts and responses there.
func TestInstrumentationPromptsResponses(t *testing.T) {
	got := instrumentedEnv(t, `{"spec":{"containers":[{"name":"app"}]}}`,
		`[{"spec":{"selector":{},"autoInstrumentationConfig":{"configInjection":{"enabled":true}},"promptsResponses":{"uploadBasePath":"gs://chats/prod"}}}]`)
	want := `[` + gkeDefaults + `,{"name":"OTEL_INSTRUMENTATION_GENAI_UPLOAD_FORMAT","value":"jsonl"},` +
		`{"name":"OTEL_INSTRUMENTATION_GENAI_COMPLETION_HOOK","value":"upload"},` +
		`{"name":"OTEL_SEMCONV_STABILITY_OPT_IN","value":"gen_ai_latest_experimental"},` +
		`{"name":"OTEL_INSTRUMENTATION_GENAI_UPLOAD_BASE_PATH","value":"gs://chats/prod"}]`
	if got["/spec/containers/0/env"] != want {
		t.Errorf("env = %s\nwant %s", got["/spec/containers/0/env"], want)
	}
}

// A pod that already has GKE's variables, as when the API server calls the
// webhook again after another one changed the pod, needs no patch.
func TestInstrumentationAlreadyInjected(t *testing.T) {
	got := instrumentedEnv(t, `{"spec":{"containers":[{"name":"app","env":[{"name":"QUEUE","value":"orders"},`+gkeDefaults+`]}]}}`, selectAll)
	if len(got) != 0 {
		t.Errorf("patch = %v, want none", got)
	}
}

// FR-GKE-021: a hostNetwork pod's own OTEL_RESOURCE_ATTRIBUTES are kept
// after GKE's k8s.pod.uid, rather than replaced, since replacing them
// would drop attributes such as the service's name.
func TestInstrumentationHostNetworkKeepsAttributes(t *testing.T) {
	got := instrumentedEnv(t, `{"spec":{"hostNetwork":true,"containers":[{"name":"app","env":[
		{"name":"OTEL_RESOURCE_ATTRIBUTES","value":"service.name=shop,deployment.environment=prod"}]}]}}`, selectAll)
	want := `[` + gkeDefaults + `,{"name":"K8S_POD_UID","valueFrom":{"fieldRef":{"apiVersion":"v1","fieldPath":"metadata.uid"}}},` +
		`{"name":"OTEL_RESOURCE_ATTRIBUTES","value":"k8s.pod.uid=$(K8S_POD_UID),service.name=shop,deployment.environment=prod"}]`
	if got["/spec/containers/0/env"] != want {
		t.Errorf("env = %s\nwant %s", got["/spec/containers/0/env"], want)
	}
}
