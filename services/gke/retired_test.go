package gke

import (
	"context"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/container/apiv1/containerpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/linuxuser586/gcpemu/internal/store"
)

// table builds a GKE version table (newest first) from version names.
func tableOf(vs ...string) []k8sVersion {
	out := make([]k8sVersion, len(vs))
	for i, v := range vs {
		out[i] = k8sVersion{GKE: v}
	}
	return out
}

// A Retired version moves to the newest patch of its minor, or, when its
// whole minor is gone, to the newest patch of the oldest newer minor left
// (ADR 0085).
func TestRetiredTarget(t *testing.T) {
	tbl := tableOf("1.38.2-gke.100", "1.38.1-gke.100", "1.37.3-gke.100", "1.37.2-gke.100", "1.36.5-gke.100")
	tests := []struct {
		name, v, want string
		retired       bool
	}{
		{"in the table", "1.37.2-gke.100", "", false},
		{"patch gone, minor left", "1.36.4-gke.100", "1.36.5-gke.100", true},
		{"minor gone", "1.35.9-gke.100", "1.36.5-gke.100", true},
		{"several minors gone", "1.33.1-gke.100", "1.36.5-gke.100", true},
		{"newer than the table", "1.39.0-gke.100", "", false},
		{"newest patch of its minor, which is gone", "1.37.4-gke.100", "1.38.2-gke.100", true},
		{"no version stored", "", "", false},
	}
	for _, tt := range tests {
		got, ok := retiredTarget(tbl, tt.v)
		if got != tt.want || ok != tt.retired {
			t.Errorf("%s: retiredTarget(%q) = %q, %v; want %q, %v", tt.name, tt.v, got, ok, tt.want, tt.retired)
		}
	}
	// A minor wholly gone below an older minor still left moves up, never
	// down: the oldest minor left that is newer than it.
	if got, _ := retiredTarget(tableOf("1.38.1-gke.100", "1.36.5-gke.100"), "1.37.1-gke.100"); got != "1.38.1-gke.100" {
		t.Errorf("retiredTarget across a gap = %q, want 1.38.1-gke.100", got)
	}
}

// putVersioned stores a cluster named name with status st, its control
// plane on master, and pools.
func putVersioned(t *testing.T, s *Service, name string, st containerpb.Cluster_Status, master string, pools ...*containerpb.NodePool) string {
	t.Helper()
	rec := &clusterRecord{Int: clusterInternal{Project: "p", Location: "us-central1-a", Name: name}}
	c := &containerpb.Cluster{Name: name, Id: "id-" + name, Location: "us-central1-a", Status: st,
		CurrentMasterVersion: master, InitialClusterVersion: master, NodePools: pools}
	c.CurrentNodeVersion = lowestNodeVersion(c)
	rec.setCluster(c)
	if err := s.env.Store.Update(func(tx store.Tx) error { return putCluster(tx, rec) }); err != nil {
		t.Fatal(err)
	}
	return rec.key()
}

// poolAt is a RUNNING node pool name on version.
func poolAt(name, version string, autoUpgrade bool) *containerpb.NodePool {
	return &containerpb.NodePool{Name: name, Version: version, Status: containerpb.NodePool_RUNNING,
		Management: &containerpb.NodeManagement{AutoUpgrade: autoUpgrade, AutoRepair: true}}
}

// recoverHeld runs recoverClusters with a substituted version table and
// returns, for each cluster restored, the cluster as its restore found it.
// Restores are held until the test ends, as with no runtime they fail at
// once and mark the cluster ERROR.
func recoverHeld(t *testing.T, s *Service, tbl []k8sVersion, want int) map[string]*containerpb.Cluster {
	t.Helper()
	s.versionTable = tbl
	type seen struct {
		key string
		c   *containerpb.Cluster
	}
	got, release := make(chan seen, want), make(chan struct{})
	s.restoring = func(_ context.Context, k string) {
		rec, err := s.loadKey(k)
		if err != nil {
			t.Error(err)
		}
		got <- seen{k, rec.cluster()}
		<-release
	}
	t.Cleanup(func() {
		close(release)
		_ = s.Stop(context.Background())
	})
	s.recoverClusters()
	out := map[string]*containerpb.Cluster{}
	for range want {
		select {
		case r := <-got:
			out[r.key] = r.c
		case <-time.After(10 * time.Second):
			t.Fatalf("%d of %d clusters restored", len(out), want)
		}
	}
	return out
}

// opsOf returns the Operations of project p of type typ.
func opsOf(s *Service, typ containerpb.Operation_Type) []*containerpb.Operation {
	var out []*containerpb.Operation
	for _, op := range s.listOps("p", "-") {
		if op.OperationType == typ {
			out = append(out, op)
		}
	}
	return out
}

var retiredTable = tableOf("1.38.1-gke.100", "1.37.3-gke.100", "1.37.1-gke.100", "1.36.5-gke.100")

// A cluster on a Retired version has its control plane and its pools on
// Retired versions upgraded before it is brought up, whatever its
// autoUpgrade and maintenance settings, and the moves are reported as DONE
// Operations that hold no lock.
func TestRecoverUpgradesRetired(t *testing.T) {
	s := newUnitService(t)
	key := putVersioned(t, s, "c", containerpb.Cluster_RUNNING, "1.36.2-gke.100",
		poolAt("old", "1.35.4-gke.100", false), poolAt("kept", "1.36.5-gke.100", true))
	if err := s.updateCluster(key, func(_ *clusterRecord, c *containerpb.Cluster) error {
		c.MaintenancePolicy = &containerpb.MaintenancePolicy{Window: &containerpb.MaintenanceWindow{
			MaintenanceExclusions: map[string]*containerpb.TimeWindow{"freeze": {
				StartTime: timestamppb.New(time.Now().Add(-time.Hour)), EndTime: timestamppb.New(time.Now().Add(time.Hour)),
			}},
		}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	c := recoverHeld(t, s, retiredTable, 1)[key]
	if c.CurrentMasterVersion != "1.36.5-gke.100" {
		t.Errorf("currentMasterVersion at bring-up = %q, want 1.36.5-gke.100", c.CurrentMasterVersion)
	}
	if v := findPool(c, "old").Version; v != "1.36.5-gke.100" {
		t.Errorf("pool with autoUpgrade off at bring-up = %q, want 1.36.5-gke.100", v)
	}
	if v := findPool(c, "kept").Version; v != "1.36.5-gke.100" {
		t.Errorf("pool on a table version = %q, want it unmoved", v)
	}
	if c.CurrentNodeVersion != "1.36.5-gke.100" {
		t.Errorf("currentNodeVersion = %q, want 1.36.5-gke.100", c.CurrentNodeVersion)
	}
	for typ, msg := range map[containerpb.Operation_Type]string{
		containerpb.Operation_UPGRADE_MASTER: "Automatic upgrade from retired version 1.36.2-gke.100 to 1.36.5-gke.100",
		containerpb.Operation_UPGRADE_NODES:  "Automatic upgrade from retired version 1.35.4-gke.100 to 1.36.5-gke.100",
	} {
		ops := opsOf(s, typ)
		if len(ops) != 1 {
			t.Errorf("%v Operations = %d, want 1", typ, len(ops))
			continue
		}
		op := ops[0]
		if op.Status != containerpb.Operation_DONE || op.StatusMessage != msg || op.Error != nil ||
			op.StartTime == "" || op.StartTime != op.EndTime {
			t.Errorf("%v Operation = %v, want DONE now with %q", typ, op, msg)
		}
	}
	// The Operations hold no lock: a client call right after recovery runs.
	if err := s.acquire(context.Background(), key, "client-op"); err != nil {
		t.Errorf("a client call after recovery: %v", err)
	}
}

// A pool's move is capped at the control plane's version; a cluster newer
// than the table, or on a table version, is left as it is, and so is one
// being deleted.
func TestRecoverRetiredLimits(t *testing.T) {
	s := newUnitService(t)
	capped := putVersioned(t, s, "capped", containerpb.Cluster_ERROR, "1.37.1-gke.100",
		poolAt("np", "1.37.0-gke.100", true), poolAt("low", "1.36.5-gke.100", true))
	newer := putVersioned(t, s, "newer", containerpb.Cluster_RUNNING, "1.39.0-gke.100", poolAt("np", "1.39.0-gke.100", true))
	// Its delete runs again on start, so it has no Operations of its own:
	// the counts below include it.
	putVersioned(t, s, "deleting", containerpb.Cluster_STOPPING, "1.36.2-gke.100", poolAt("np", "1.36.2-gke.100", true))
	s.dataPlaneDestroy = func(context.Context, string) error { return nil }

	got := recoverHeld(t, s, retiredTable, 2)
	c := got[capped]
	if c.CurrentMasterVersion != "1.37.1-gke.100" {
		t.Errorf("master on a table version moved to %q", c.CurrentMasterVersion)
	}
	if v := findPool(c, "np").Version; v != "1.37.1-gke.100" {
		t.Errorf("pool = %q, want 1.37.1-gke.100 (capped at the master)", v)
	}
	if c.CurrentNodeVersion != "1.36.5-gke.100" {
		t.Errorf("currentNodeVersion = %q, want the lowest pool version 1.36.5-gke.100", c.CurrentNodeVersion)
	}
	if n := got[newer]; n.CurrentMasterVersion != "1.39.0-gke.100" || findPool(n, "np").Version != "1.39.0-gke.100" {
		t.Errorf("cluster newer than the table moved: master %q, pool %q", n.CurrentMasterVersion, findPool(n, "np").Version)
	}
	if ops := opsOf(s, containerpb.Operation_UPGRADE_MASTER); len(ops) != 0 {
		t.Errorf("UPGRADE_MASTER Operations = %v, want none", ops)
	}
	if ops := opsOf(s, containerpb.Operation_UPGRADE_NODES); len(ops) != 1 || !strings.HasSuffix(ops[0].TargetLink, "/clusters/capped/nodePools/np") {
		t.Errorf("UPGRADE_NODES Operations = %v, want one for capped's pool np", ops)
	}
}

// A cluster with no node pools, as Autopilot clusters are, reports its
// moved control plane's version as its node version.
func TestRecoverRetiredNoPools(t *testing.T) {
	s := newUnitService(t)
	key := putVersioned(t, s, "auto", containerpb.Cluster_RUNNING, "1.36.2-gke.100")
	c := recoverHeld(t, s, retiredTable, 1)[key]
	if c.CurrentMasterVersion != "1.36.5-gke.100" || c.CurrentNodeVersion != "1.36.5-gke.100" {
		t.Errorf("versions = master %q, nodes %q; want both 1.36.5-gke.100", c.CurrentMasterVersion, c.CurrentNodeVersion)
	}
	if ops := opsOf(s, containerpb.Operation_UPGRADE_NODES); len(ops) != 0 {
		t.Errorf("UPGRADE_NODES Operations = %v, want none with no pools", ops)
	}
}
