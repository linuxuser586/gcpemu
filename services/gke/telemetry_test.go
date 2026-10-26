package gke

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"cloud.google.com/go/container/apiv1/containerpb"
)

// TestManagedTelemetryREST is FR-GKE-018: the managed OpenTelemetry scope
// and managed Prometheus that OpenTofu sends on create come back on get,
// on both cluster modes.
func TestManagedTelemetryREST(t *testing.T) {
	a := newAPI(t)
	h := newTranscoder(a)
	do := func(method, path, body string) map[string]any {
		t.Helper()
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader(body)))
		if w.Code != http.StatusOK {
			t.Fatalf("%s %s = %d: %s", method, path, w.Code, w.Body)
		}
		var out map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	for _, mode := range []struct{ name, extra string }{
		{"std", `"initialNodeCount": 1`},
		{"ap", `"autopilot": {"enabled": true}`},
	} {
		t.Run(mode.name, func(t *testing.T) {
			op := do("POST", "/v1/"+apParent+"/clusters", `{"cluster": {"name": "`+mode.name+`", `+mode.extra+`,
				"managedOpentelemetryConfig": {"scope": "COLLECTION_AND_INSTRUMENTATION_COMPONENTS"},
				"monitoringConfig": {"managedPrometheusConfig": {"enabled": true}}}}`)
			name, _ := op["name"].(string)
			waitOp(t, a, "ap-test", name)
			got := do("GET", "/v1/"+apParent+"/clusters/"+mode.name, "")
			otel, _ := got["managedOpentelemetryConfig"].(map[string]any)
			if otel["scope"] != "COLLECTION_AND_INSTRUMENTATION_COMPONENTS" {
				t.Errorf("managedOpentelemetryConfig = %v", got["managedOpentelemetryConfig"])
			}
			mon, _ := got["monitoringConfig"].(map[string]any)
			prom, _ := mon["managedPrometheusConfig"].(map[string]any)
			if prom["enabled"] != true {
				t.Errorf("monitoringConfig = %v", got["monitoringConfig"])
			}
		})
	}
}

// TestManagedTelemetryUpdate is FR-GKE-018: a ClusterUpdate changes the
// stored managed OpenTelemetry scope, and one that does not name it leaves
// it as it is.
func TestManagedTelemetryUpdate(t *testing.T) {
	a := newAPI(t)
	ctx := context.Background()
	for _, c := range []*containerpb.Cluster{{Name: "std", InitialNodeCount: 1}, autopilot("ap")} {
		createRunning(t, a, apParent, c)
		cl := apParent + "/clusters/" + c.Name
		update := func(what string, u *containerpb.ClusterUpdate, want containerpb.ManagedOpenTelemetryConfig_Scope) {
			t.Helper()
			op, err := a.UpdateCluster(ctx, &containerpb.UpdateClusterRequest{Name: cl, Update: u})
			if err != nil {
				t.Fatalf("%s: UpdateCluster: %v", c.Name, err)
			}
			waitOp(t, a, "ap-test", op.Name) // the scope's add-ons are reconciled
			got, err := a.GetCluster(ctx, &containerpb.GetClusterRequest{Name: cl})
			if err != nil {
				t.Fatal(err)
			}
			if s := got.GetManagedOpentelemetryConfig().GetScope(); s != want {
				t.Errorf("%s: scope after %s = %v, want %v", c.Name, what, s, want)
			}
		}
		all := containerpb.ManagedOpenTelemetryConfig_COLLECTION_AND_INSTRUMENTATION_COMPONENTS
		none := containerpb.ManagedOpenTelemetryConfig_NONE
		update("update", &containerpb.ClusterUpdate{DesiredManagedOpentelemetryConfig: &containerpb.ManagedOpenTelemetryConfig{Scope: &all}}, all)
		update("unrelated update", &containerpb.ClusterUpdate{DesiredLoggingService: "none"}, all)
		update("NONE", &containerpb.ClusterUpdate{DesiredManagedOpentelemetryConfig: &containerpb.ManagedOpenTelemetryConfig{Scope: &none}}, none)
	}
}

// TestAutopilotManagedPrometheus is FR-GKE-018: GKE turns managed
// Prometheus on for an Autopilot cluster unless the request turns it off,
// and leaves a Standard cluster's as the request has it.
func TestAutopilotManagedPrometheus(t *testing.T) {
	components := &containerpb.MonitoringConfig{ComponentConfig: &containerpb.MonitoringComponentConfig{
		EnableComponents: []containerpb.MonitoringComponentConfig_Component{containerpb.MonitoringComponentConfig_SYSTEM_COMPONENTS}}}
	off := &containerpb.MonitoringConfig{ManagedPrometheusConfig: &containerpb.ManagedPrometheusConfig{Enabled: false}}
	tests := []struct {
		name string
		c    *containerpb.Cluster
		want bool
	}{
		{"ap-default", autopilot("ap-default"), true},
		{"ap-components", withMonitoring(autopilot("ap-components"), components), true},
		{"ap-off", withMonitoring(autopilot("ap-off"), off), false},
		{"std-default", &containerpb.Cluster{Name: "std-default", InitialNodeCount: 1}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := createRunning(t, newAPI(t), apParent, tt.c)
			if enabled := got.GetMonitoringConfig().GetManagedPrometheusConfig().GetEnabled(); enabled != tt.want {
				t.Errorf("managedPrometheusConfig.enabled = %v, want %v", enabled, tt.want)
			}
			if tt.c.MonitoringConfig.GetComponentConfig() != nil && len(got.GetMonitoringConfig().GetComponentConfig().GetEnableComponents()) != 1 {
				t.Errorf("componentConfig lost: %v", got.GetMonitoringConfig())
			}
		})
	}
}

func withMonitoring(c *containerpb.Cluster, m *containerpb.MonitoringConfig) *containerpb.Cluster {
	c.MonitoringConfig = m
	return c
}

// TestManagedPrometheusUpdate is FR-GKE-018: managed Prometheus round-trips
// through desiredMonitoringConfig, and an update that does not name it
// leaves it as it is, so an Autopilot cluster keeps GKE's default.
func TestManagedPrometheusUpdate(t *testing.T) {
	a := newAPI(t)
	ctx := context.Background()
	createRunning(t, a, apParent, autopilot("ap"))
	cl := apParent + "/clusters/ap"
	update := func(m *containerpb.MonitoringConfig) bool {
		t.Helper()
		if _, err := a.UpdateCluster(ctx, &containerpb.UpdateClusterRequest{Name: cl, Update: &containerpb.ClusterUpdate{DesiredMonitoringConfig: m}}); err != nil {
			t.Fatalf("UpdateCluster: %v", err)
		}
		got, err := a.GetCluster(ctx, &containerpb.GetClusterRequest{Name: cl})
		if err != nil {
			t.Fatal(err)
		}
		return got.GetMonitoringConfig().GetManagedPrometheusConfig().GetEnabled()
	}
	if update(&containerpb.MonitoringConfig{ComponentConfig: &containerpb.MonitoringComponentConfig{
		EnableComponents: []containerpb.MonitoringComponentConfig_Component{containerpb.MonitoringComponentConfig_SYSTEM_COMPONENTS}}}) != true {
		t.Error("componentConfig update turned managed Prometheus off")
	}
	if update(&containerpb.MonitoringConfig{ManagedPrometheusConfig: &containerpb.ManagedPrometheusConfig{}}) != false {
		t.Error("disabling managed Prometheus did not stick")
	}
	if update(&containerpb.MonitoringConfig{ManagedPrometheusConfig: &containerpb.ManagedPrometheusConfig{Enabled: true}}) != true {
		t.Error("enabling managed Prometheus did not stick")
	}
}

// TestNoVersionBelowOTelFloor guards FR-GKE-018's skipped check: GKE
// rejects a Managed OpenTelemetry scope other than NONE below 1.34, and no
// offered version is that old. Offering one means implementing the check.
func TestNoVersionBelowOTelFloor(t *testing.T) {
	for _, v := range versions {
		if compareVersions(v.GKE, "1.34") < 0 {
			t.Errorf("%s is below 1.34: reject a Managed OpenTelemetry scope on it (FR-GKE-018)", v.GKE)
		}
	}
}
