package gke_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"cloud.google.com/go/container/apiv1/containerpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/linuxuser586/gcpemu/emutest"
	"github.com/linuxuser586/gcpemu/internal/runtime"
	"github.com/linuxuser586/gcpemu/services/gke"
)

// stalled blocks a hook until ctx ends (or a while passes, so a missing
// cancellation shows as a test failure rather than a hang), then holds a
// little longer, like a bring-up slow to stop.
func stalled(ctx context.Context) {
	select {
	case <-ctx.Done():
	case <-time.After(10 * time.Second):
	}
	time.Sleep(2 * time.Second)
}

// TestResetRestoreDuringBringUp restores a State snapshot while an earlier
// bring-up of the cluster is in flight: the Reset stops that bring-up
// before the restored one starts the server, so the restored Kubernetes
// objects survive (#293).
func TestResetRestoreDuringBringUp(t *testing.T) {
	emutest.RequireRuntime(t)
	t.Parallel()
	emutest.HoldClusterSlot(t)
	ctx := context.Background()
	inst := emutest.Start(t, []string{"gke"})
	cm := newClient(t, inst)
	op, err := cm.CreateCluster(ctx, &containerpb.CreateClusterRequest{Parent: parent, Cluster: &containerpb.Cluster{
		Name: "inflight", NodePools: []*containerpb.NodePool{{Name: "np", InitialNodeCount: 1}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	waitOp(t, cm, zone, op.Name)
	name := parent + "/clusters/inflight"
	c, err := cm.GetCluster(ctx, &containerpb.GetClusterRequest{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	putConfigMap(t, inst, c, "saved")
	if _, err := inst.Internal().SaveSnapshot(ctx, "fixture"); err != nil {
		t.Fatal(err)
	}

	gsvc, _ := inst.Internal().Env.Lookup("gke")
	var calls atomic.Int32
	var earlierDone atomic.Bool
	entered := make(chan struct{})
	gke.SetBeforeServerStart(gsvc, func(ctx context.Context, key string) {
		if calls.Add(1) == 1 {
			close(entered)
			stalled(ctx)
			earlierDone.Store(true)
			return
		}
		if !earlierDone.Load() {
			t.Error("the restored bring-up reached the server before the earlier one stopped")
		}
	})
	// The first restore's bring-up stalls before touching the server.
	if err := inst.Internal().RestoreSnapshot(ctx, "fixture"); err != nil {
		t.Fatal(err)
	}
	<-entered
	if err := inst.Internal().RestoreSnapshot(ctx, "fixture"); err != nil {
		t.Fatal(err)
	}
	c = waitRunning(t, inst, name)
	if n := calls.Load(); n != 2 {
		t.Errorf("server bring-ups = %d, want 2", n)
	}
	e := masterAuthEnv(t, inst, c)
	checkConfigMap(t, e, "saved")
}

// TestResetStopsOperation resets while CreateCluster is mid bring-up: the
// operation has stopped when Reset returns and creates no runtime objects
// afterwards, and the service takes new operations (#293).
func TestResetStopsOperation(t *testing.T) {
	emutest.RequireRuntime(t)
	t.Parallel()
	emutest.HoldClusterSlot(t)
	ctx := context.Background()
	inst := emutest.Start(t, []string{"gke"})
	gsvc, _ := inst.Internal().Env.Lookup("gke")
	var stopped atomic.Bool
	entered := make(chan struct{})
	gke.SetBeforeServerStart(gsvc, func(ctx context.Context, key string) {
		select {
		case <-entered:
			return // later clusters run normally
		default:
		}
		close(entered)
		stalled(ctx)
		stopped.Store(true)
	})
	cm := newClient(t, inst)
	if _, err := cm.CreateCluster(ctx, &containerpb.CreateClusterRequest{Parent: parent, Cluster: &containerpb.Cluster{
		Name: "cut", NodePools: []*containerpb.NodePool{{Name: "np", InitialNodeCount: 1}},
	}}); err != nil {
		t.Fatal(err)
	}
	<-entered
	if err := inst.Internal().Reset(ctx); err != nil {
		t.Fatal(err)
	}
	if !stopped.Load() {
		t.Fatal("CreateCluster still running after Reset returned")
	}

	cl, err := runtime.Detect()
	if err != nil {
		t.Fatal(err)
	}
	sel := map[string]string{runtime.LabelInstance: inst.Internal().ID, runtime.LabelService: "gke"}
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(500 * time.Millisecond) {
		cs, err := cl.ListContainers(ctx, sel)
		if err != nil {
			t.Fatal(err)
		}
		vs, err := cl.ListVolumes(ctx, sel)
		if err != nil {
			t.Fatal(err)
		}
		if len(cs) > 0 || len(vs) > 0 {
			t.Fatalf("runtime objects after Reset: %d containers %v, volumes %v", len(cs), cs, vs)
		}
	}

	op, err := cm.CreateCluster(ctx, &containerpb.CreateClusterRequest{Parent: parent, Cluster: &containerpb.Cluster{
		Name: "after", NodePools: []*containerpb.NodePool{{Name: "np", InitialNodeCount: 1}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	waitOp(t, cm, zone, op.Name)
}

// TestCreateClusterDuringReset issues CreateCluster while a Reset is
// tearing GKE down, after GKE has started its new background generation:
// the API gateway refuses it as UNAVAILABLE, so no bring-up outlives the wipe
// and nothing is left in the Store or the runtime (#302).
func TestCreateClusterDuringReset(t *testing.T) {
	emutest.RequireRuntime(t)
	t.Parallel()
	emutest.HoldClusterSlot(t)
	ctx := context.Background()
	inst := emutest.Start(t, []string{"gke"})
	gsvc, _ := inst.Internal().Env.Lookup("gke")
	cm := newClient(t, inst)
	var createErr error
	gke.SetBeforeResetTeardown(gsvc, func() {
		cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		_, createErr = cm.CreateCluster(cctx, &containerpb.CreateClusterRequest{Parent: parent, Cluster: &containerpb.Cluster{
			Name: "racer", NodePools: []*containerpb.NodePool{{Name: "np", InitialNodeCount: 1}},
		}})
	})
	if err := inst.Internal().Reset(ctx); err != nil {
		t.Fatal(err)
	}
	gke.SetBeforeResetTeardown(gsvc, nil)
	if status.Code(createErr) != codes.Unavailable {
		t.Errorf("CreateCluster during Reset: %v, want UNAVAILABLE", createErr)
	}

	resp, err := cm.ListClusters(ctx, &containerpb.ListClustersRequest{Parent: parent})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Clusters) > 0 {
		t.Errorf("clusters after Reset: %v", resp.Clusters)
	}
	cl, err := runtime.Detect()
	if err != nil {
		t.Fatal(err)
	}
	sel := map[string]string{runtime.LabelInstance: inst.Internal().ID, runtime.LabelService: "gke"}
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(500 * time.Millisecond) {
		cs, err := cl.ListContainers(ctx, sel)
		if err != nil {
			t.Fatal(err)
		}
		vs, err := cl.ListVolumes(ctx, sel)
		if err != nil {
			t.Fatal(err)
		}
		if len(cs) > 0 || len(vs) > 0 {
			t.Fatalf("runtime objects after Reset: %d containers %v, volumes %v", len(cs), cs, vs)
		}
	}
}
