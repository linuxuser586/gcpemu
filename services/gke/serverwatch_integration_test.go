package gke_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"cloud.google.com/go/container/apiv1/containerpb"

	"github.com/linuxuser586/gcpemu/emutest"
	"github.com/linuxuser586/gcpemu/internal/runtime"
)

// TestKilledServerRestarts kills a RUNNING cluster's Cluster server, as k3s
// exiting under load does (#840): the Service starts it again on its
// volume, so the Kubernetes API answers with its earlier objects and the
// Instance is ready again, without being stopped.
func TestKilledServerRestarts(t *testing.T) {
	emutest.RequireRuntime(t)
	t.Parallel()
	emutest.HoldClusterSlot(t)
	ctx := context.Background()
	inst := emutest.Start(t, []string{"gke"})
	cm := newClient(t, inst)
	op, err := cm.CreateCluster(ctx, &containerpb.CreateClusterRequest{Parent: parent, Cluster: &containerpb.Cluster{
		Name: "crash", NodePools: []*containerpb.NodePool{{Name: "np", InitialNodeCount: 1}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	waitOp(t, cm, zone, op.Name)
	name := parent + "/clusters/crash"
	c, err := cm.GetCluster(ctx, &containerpb.GetClusterRequest{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	adm, pool := adminClient(t, inst, name)
	e := &env{t: t, inst: inst, c: c, ca: pool, adm: adm}
	e.mustKube("POST", "/api/v1/namespaces/default/configmaps", map[string]any{"metadata": map[string]any{"name": "before"}, "data": map[string]string{"k": "v"}}, nil)

	// Killed through a client of its own, as docker kill is: the
	// Instance's record of starting the server is untouched.
	cl, err := runtime.Detect()
	if err != nil {
		t.Fatal(err)
	}
	id := inst.Internal().ID
	sel := map[string]string{runtime.LabelInstance: id, runtime.LabelService: "gke", runtime.LabelRole: "server"}
	servers, err := cl.ListContainers(ctx, sel)
	if err != nil || len(servers) != 1 {
		t.Fatalf("Cluster servers = %v, %v; want 1", servers, err)
	}
	if err := cl.StopContainer(ctx, servers[0].ID, 0); err != nil {
		t.Fatal(err)
	}

	ready := func() bool {
		resp, err := http.Get(inst.GatewayURL() + "/_emu/v1/ready")
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}
	eventually(t, 4*time.Minute, "the Cluster server back and the Instance ready", func() bool {
		cts, err := cl.ListContainers(ctx, sel)
		if err != nil || len(cts) != 1 || cts[0].ID == servers[0].ID || cts[0].Status != "running" {
			return false
		}
		// The endpoint resets connections until the server is back.
		resp, err := adm.Get("https://" + c.Endpoint + "/api/v1/namespaces/default/configmaps/before")
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == http.StatusOK && ready()
	})
	var cmap struct{ Data map[string]string }
	e.mustKube("GET", "/api/v1/namespaces/default/configmaps/before", nil, &cmap)
	if cmap.Data["k"] != "v" {
		t.Errorf("configmap after the restart = %v", cmap)
	}
	if c, err := cm.GetCluster(ctx, &containerpb.GetClusterRequest{Name: name}); err != nil || c.Status != containerpb.Cluster_RUNNING {
		t.Errorf("cluster after the restart: %v, %v; want RUNNING", c.GetStatus(), err)
	}
}
