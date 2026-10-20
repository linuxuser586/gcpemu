package gke_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"testing"

	"cloud.google.com/go/container/apiv1/containerpb"

	"github.com/linuxuser586/gcpemu/emutest"
	"github.com/linuxuser586/gcpemu/services/gke"
)

// TestClusterWaitCondition is FR-CI-007's gke/cluster: met once the
// cluster is RUNNING, pending while it is being created or does not exist
// yet, and failed once it is in ERROR or STOPPING.
func TestClusterWaitCondition(t *testing.T) {
	inst := emutest.Start(t, []string{"gke"})
	svc, _ := inst.Env.Lookup("gke")
	for name, st := range map[string]containerpb.Cluster_Status{
		"up": containerpb.Cluster_RUNNING, "fixing": containerpb.Cluster_RECONCILING, "new": containerpb.Cluster_PROVISIONING,
		"broken": containerpb.Cluster_ERROR, "going": containerpb.Cluster_STOPPING,
	} {
		if err := gke.PutCluster(svc, project, zone, name, st, "boom"); err != nil {
			t.Fatal(err)
		}
	}
	if err := gke.PutCluster(svc, project, "us-central1", "regional", containerpb.Cluster_RUNNING, ""); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct{ target, state, reason string }{
		{project + "/" + zone + "/up", "met", ""},
		{project + "/us-central1/regional", "met", ""},
		{project + "/" + zone + "/fixing", "pending", "status RECONCILING"},
		{project + "/" + zone + "/new", "pending", "status PROVISIONING"},
		{project + "/" + zone + "/missing", "pending", "not found"},
		{project + "/us-central1/up", "pending", "not found"},
		{"other/" + zone + "/up", "pending", "not found"},
		{project + "/" + zone + "/broken", "failed", "status ERROR: boom"},
		{project + "/" + zone + "/going", "failed", "status STOPPING"},
	} {
		code, got := condition(t, inst, "gke/cluster="+tc.target)
		if code != 200 || got.State != tc.state || got.Reason != tc.reason {
			t.Errorf("gke/cluster=%s: %d %+v, want %s %q", tc.target, code, got, tc.state, tc.reason)
		}
	}
	if code, got := condition(t, inst, "gke/cluster="+project+"/mars-north1/up"); code != 400 {
		t.Errorf("gke/cluster in an unknown location: %d %+v, want 400", code, got)
	}
}

type waitCondition struct{ State, Reason, Error string }

// condition asks the Admin API for one Wait condition.
func condition(t *testing.T, inst *emutest.Instance, cond string) (int, waitCondition) {
	t.Helper()
	resp, err := http.Get(inst.GatewayURL() + "/_emu/v1/conditions?" + url.Values{"for": {cond}}.Encode())
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var out struct {
		Conditions []waitCondition
		Error      string
	}
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("%s: %v", b, err)
	}
	if len(out.Conditions) == 1 {
		return resp.StatusCode, out.Conditions[0]
	}
	return resp.StatusCode, waitCondition{Error: out.Error}
}
