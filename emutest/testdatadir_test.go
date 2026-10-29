package emutest_test

import (
	"context"
	"testing"

	"github.com/linuxuser586/gcpemu/emutest"
	"github.com/linuxuser586/gcpemu/internal/runtime"
)

// TestWithTestDataDirRemovesObjects: a Persistent Instance in a dir the
// test owns keeps its data, networks and volumes across a restart, and
// none of its networks or volumes are left once the test ends (#842).
func TestWithTestDataDirRemovesObjects(t *testing.T) {
	emutest.RequireRuntime(t)
	ctx := context.Background()
	var id string
	t.Run("restart", func(t *testing.T) {
		persistent := emutest.WithTestDataDir(t, t.TempDir())
		inst := emutest.Start(t, []string{"gcs"}, persistent)
		id = inst.Internal().ID
		if err := inst.Storage(t).Bucket("kept-bucket").Create(ctx, "persist-proj", nil); err != nil {
			t.Fatal(err)
		}
		plane, err := inst.Internal().Env.Containers.Netplane(ctx)
		if err != nil {
			t.Fatal(err)
		}
		svc, err := plane.Services(ctx)
		if err != nil {
			t.Fatal(err)
		}
		rt := plane.Runtime()
		before, err := rt.InspectNetwork(ctx, svc.Name)
		if err != nil {
			t.Fatal(err)
		}
		vol := rt.Name("data")
		if err := rt.CreateVolume(ctx, vol, rt.Labels("", "", "")); err != nil {
			t.Fatal(err)
		}
		if err := inst.Close(ctx); err != nil {
			t.Fatal(err)
		}

		inst = emutest.Start(t, []string{"gcs"}, persistent)
		if inst.Internal().ID != id {
			t.Fatalf("Instance ID %s after a restart, want %s", inst.Internal().ID, id)
		}
		if _, err := inst.Storage(t).Bucket("kept-bucket").Attrs(ctx); err != nil {
			t.Errorf("bucket after a restart: %v", err)
		}
		if after, err := rt.InspectNetwork(ctx, svc.Name); err != nil || after.ID != before.ID {
			t.Errorf("services network after a restart: %s, %v; want the same network %s", after.ID, err, before.ID)
		}
		if vols, err := rt.ListVolumes(ctx, map[string]string{runtime.LabelInstance: id}); err != nil || len(vols) != 1 {
			t.Errorf("volumes after a restart: %q, %v; want %s", vols, err, vol)
		}
	})
	cl, err := runtime.Detect()
	if err != nil {
		t.Fatal(err)
	}
	sel := map[string]string{runtime.LabelInstance: id}
	nets, err := cl.ListNetworks(ctx, sel)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range nets {
		t.Errorf("network %s left after the test", n.Name)
	}
	vols, err := cl.ListVolumes(ctx, sel)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range vols {
		t.Errorf("volume %s left after the test", v)
	}
}
