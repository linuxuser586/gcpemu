package gke_test

import (
	"context"
	"testing"
	"time"

	"cloud.google.com/go/container/apiv1/containerpb"

	"github.com/linuxuser586/gcpemu/emutest"
)

// TestStateSnapshot covers FR-CORE-012 for GKE: restoring a State
// snapshot brings back a cluster deleted since, with a working data plane
// and its nodes.
func TestStateSnapshot(t *testing.T) {
	emutest.RequireRuntime(t)
	t.Parallel()
	ctx := context.Background()
	inst := emutest.Start(t, []string{"gke"})
	cm := newClient(t, inst)
	op, err := cm.CreateCluster(ctx, &containerpb.CreateClusterRequest{Parent: parent, Cluster: &containerpb.Cluster{
		Name: "snap", NodePools: []*containerpb.NodePool{{Name: "np", InitialNodeCount: 1}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	waitOp(t, cm, zone, op.Name, 3*time.Minute)
	name := parent + "/clusters/snap"

	if _, err := inst.SaveSnapshot(ctx, "fixture"); err != nil {
		t.Fatal(err)
	}
	op, err = cm.DeleteCluster(ctx, &containerpb.DeleteClusterRequest{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	waitOp(t, cm, zone, op.Name, 2*time.Minute)

	if err := inst.RestoreSnapshot(ctx, "fixture"); err != nil {
		t.Fatal(err)
	}
	var c *containerpb.Cluster
	for deadline := time.Now().Add(3 * time.Minute); ; time.Sleep(500 * time.Millisecond) {
		c, err = cm.GetCluster(ctx, &containerpb.GetClusterRequest{Name: name})
		if err == nil && c.Status == containerpb.Cluster_RUNNING {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("cluster not RUNNING 3m after restore: %v %v %q\n%s", err, c.GetStatus(), c.GetStatusMessage(), containerDiagnostics(inst.ID))
		}
	}
	adm, pool := adminClient(t, inst, name)
	e := &env{t: t, inst: inst, c: c, ca: pool, adm: adm}
	var nodes struct{ Items []any }
	e.mustKube("GET", "/api/v1/nodes", nil, &nodes)
	if len(nodes.Items) != 1 {
		t.Errorf("nodes after restore = %d", len(nodes.Items))
	}
}
