package gke_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"cloud.google.com/go/container/apiv1/containerpb"
)

// TestDiagnosticsReadTheEnvsCluster keeps a timeout's report on the cluster
// the subtest uses: PrivateCluster has an env of its own for priv (#1004).
func TestDiagnosticsReadTheEnvsCluster(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/namespaces/default/pods":
			w.Write([]byte(`{"items":[{"metadata":{"name":"egress"},"status":{"phase":"Pending"}}]}`))
		case "/api/v1/nodes":
			w.Write([]byte(`{"items":[{"metadata":{"name":"priv-node"},"status":{"conditions":[{"type":"DiskPressure","status":"True"}]}}]}`))
		default:
			w.Write([]byte(`{"items":[]}`))
		}
	}))
	defer srv.Close()
	e := &env{t: t, adm: srv.Client(), c: &containerpb.Cluster{Endpoint: strings.TrimPrefix(srv.URL, "https://")}}

	for name, diag := range map[string]func() string{"pod": e.diagnosePod("default", "egress"), "selector": e.diagnose("default", "app=web")} {
		got := diag()
		for _, want := range []string{"egress phase=Pending", "priv-node DiskPressure=True"} {
			if !strings.Contains(got, want) {
				t.Errorf("%s: missing %q in:\n%s", name, want, got)
			}
		}
	}
}
