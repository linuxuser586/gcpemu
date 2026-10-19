package gke_test

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/container/apiv1/containerpb"

	"github.com/linuxuser586/gcpemu/emutest"
	"github.com/linuxuser586/gcpemu/internal/instance"
	"github.com/linuxuser586/gcpemu/internal/runtime"
	"github.com/linuxuser586/gcpemu/services/gke"
)

// TestStateSnapshot covers FR-CORE-012 for GKE: restoring a State
// snapshot brings back a cluster deleted since, with its Kubernetes
// objects, its cluster CA and its nodes (#286).
func TestStateSnapshot(t *testing.T) {
	emutest.RequireRuntime(t)
	t.Parallel()
	emutest.HoldClusterSlot(t)
	ctx := context.Background()
	inst := emutest.Start(t, []string{"gke"})
	cm := newClient(t, inst)
	op, err := cm.CreateCluster(ctx, &containerpb.CreateClusterRequest{Parent: parent, Cluster: &containerpb.Cluster{
		Name: "snap", NodePools: []*containerpb.NodePool{{Name: "np", InitialNodeCount: 1}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	waitOp(t, cm, zone, op.Name)
	name := parent + "/clusters/snap"
	saved, err := cm.GetCluster(ctx, &containerpb.GetClusterRequest{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	putConfigMap(t, inst, saved, "saved")

	if _, err := inst.SaveSnapshot(ctx, "fixture"); err != nil {
		t.Fatal(err)
	}
	checkArchive(t, filepath.Join(inst.Dir, instance.SnapshotsDir, "fixture"), saved.Id)
	op, err = cm.DeleteCluster(ctx, &containerpb.DeleteClusterRequest{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	waitOp(t, cm, zone, op.Name)

	if err := inst.RestoreSnapshot(ctx, "fixture"); err != nil {
		t.Fatal(err)
	}
	c := waitRunning(t, inst, name)
	if c.MasterAuth.GetClusterCaCertificate() != saved.MasterAuth.GetClusterCaCertificate() {
		t.Error("clusterCaCertificate changed by the restore")
	}
	e := masterAuthEnv(t, inst, c)
	checkConfigMap(t, e, "saved")
	checkNodesReady(t, e, 1)
}

// TestStateSnapshotErrorCluster saves a State snapshot while a cluster is
// in ERROR: its datastore is not saved, and the restore brings it back
// RUNNING on a fresh data plane, with a new cluster CA, none of its
// earlier Kubernetes objects and its nodes rejoined (#286, #295).
func TestStateSnapshotErrorCluster(t *testing.T) {
	emutest.RequireRuntime(t)
	t.Parallel()
	emutest.HoldClusterSlot(t)
	ctx := context.Background()
	inst := emutest.Start(t, []string{"gke"})
	cm := newClient(t, inst)
	op, err := cm.CreateCluster(ctx, &containerpb.CreateClusterRequest{Parent: parent, Cluster: &containerpb.Cluster{
		Name: "snaperr", NodePools: []*containerpb.NodePool{{Name: "np", InitialNodeCount: 1}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	waitOp(t, cm, zone, op.Name)
	name := parent + "/clusters/snaperr"
	saved := waitRunning(t, inst, name)
	putConfigMap(t, inst, saved, "saved")
	gsvc, ok := inst.Env.Lookup("gke")
	if !ok {
		t.Fatal("gke service not found")
	}
	if err := gke.SetClusterStatus(gsvc, name, containerpb.Cluster_ERROR); err != nil {
		t.Fatal(err)
	}

	if _, err := inst.SaveSnapshot(ctx, "fixture"); err != nil {
		t.Fatal(err)
	}
	// The only cluster is skipped, so the snapshot holds no gke files.
	var files []string
	_ = filepath.WalkDir(filepath.Join(inst.Dir, instance.SnapshotsDir, "fixture", "services", "gke"), func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			files = append(files, p)
		}
		return nil
	})
	if len(files) != 0 {
		t.Fatalf("gke snapshot files of the ERROR cluster %s = %v, want none", saved.Id, files)
	}

	if err := inst.RestoreSnapshot(ctx, "fixture"); err != nil {
		t.Fatal(err)
	}
	c := waitRunning(t, inst, name)
	if c.MasterAuth.GetClusterCaCertificate() == saved.MasterAuth.GetClusterCaCertificate() {
		t.Error("clusterCaCertificate kept by the restore of a fresh data plane")
	}
	e := masterAuthEnv(t, inst, c)
	if code, b := e.kube("GET", "/api/v1/namespaces/default/configmaps/saved", "", nil, nil); code != http.StatusNotFound {
		t.Errorf("configmap saved: %d %s, want 404", code, b)
	}
	checkNodesReady(t, e, 1)
}

// TestStateSnapshotOtherInstance restores a State snapshot into a later
// Instance with the same dir but a different Instance ID, then restarts
// it: the restored objects and those created since survive, so the
// restored datastore is not written again (#286). The restore does not
// wait for the cluster's Store-recovery bring-up to finish (#303).
func TestStateSnapshotOtherInstance(t *testing.T) {
	emutest.RequireRuntime(t)
	t.Parallel()
	emutest.HoldClusterSlot(t)
	ctx := context.Background()
	dir := t.TempDir()
	var ids []string
	t.Cleanup(func() {
		// The instances are not ephemeral: remove what they leave behind.
		cl, err := runtime.Detect()
		if err != nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		for _, id := range ids {
			_ = (&runtime.Manager{Client: cl, InstanceID: id}).Cleanup(ctx, true)
		}
	})
	start := func() *emutest.Instance {
		inst := startPersistent(t, dir)
		ids = append(ids, inst.ID)
		return inst
	}
	stop := func(inst *emutest.Instance) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := inst.Shutdown(ctx); err != nil {
			t.Fatalf("shutdown: %v", err)
		}
	}

	inst := start()
	cm := newClient(t, inst)
	op, err := cm.CreateCluster(ctx, &containerpb.CreateClusterRequest{Parent: parent, Cluster: &containerpb.Cluster{
		Name: "moved", NodePools: []*containerpb.NodePool{{Name: "np", InitialNodeCount: 1}},
	}})
	if err != nil {
		_ = inst.Shutdown(ctx)
		t.Fatal(err)
	}
	waitOp(t, cm, zone, op.Name)
	name := parent + "/clusters/moved"
	c, err := cm.GetCluster(ctx, &containerpb.GetClusterRequest{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	putConfigMap(t, inst, c, "saved")
	if _, err := inst.SaveSnapshot(ctx, "fixture"); err != nil {
		t.Fatal(err)
	}
	stop(inst)
	if err := os.Remove(filepath.Join(dir, instance.IDFile)); err != nil {
		t.Fatal(err)
	}

	inst = start()
	if inst.ID == ids[0] {
		t.Fatalf("Instance ID kept: %s", inst.ID)
	}
	// Restore deliberately during the Store-recovery bring-up, as a user
	// restoring right after a restart would. Nothing makes sure it is still
	// in flight; TestResetRestoreDuringBringUp checks that ordering reliably.
	if err := inst.RestoreSnapshot(ctx, "fixture"); err != nil {
		_ = inst.Shutdown(ctx)
		t.Fatal(err)
	}
	c = waitRunning(t, inst, name)
	e := masterAuthEnv(t, inst, c)
	checkConfigMap(t, e, "saved")
	checkNodesReady(t, e, 1)
	putConfigMap(t, inst, c, "after")
	stop(inst)

	inst = start()
	defer inst.Shutdown(context.Background())
	c = waitRunning(t, inst, name)
	e = masterAuthEnv(t, inst, c)
	checkConfigMap(t, e, "saved")
	checkConfigMap(t, e, "after")
}

// waitRunning waits for a cluster to be RUNNING after a restore or restart.
func waitRunning(t *testing.T, inst *emutest.Instance, name string) *containerpb.Cluster {
	t.Helper()
	cm := newClient(t, inst)
	for deadline := time.Now().Add(3 * time.Minute); ; time.Sleep(500 * time.Millisecond) {
		c, err := cm.GetCluster(context.Background(), &containerpb.GetClusterRequest{Name: name})
		if err == nil && c.Status == containerpb.Cluster_RUNNING {
			return c
		}
		if time.Now().After(deadline) {
			t.Fatalf("cluster not RUNNING after 3m: %v %v %q\n%s", err, c.GetStatus(), c.GetStatusMessage(), containerDiagnostics(inst.ID))
		}
	}
}

func putConfigMap(t *testing.T, inst *emutest.Instance, c *containerpb.Cluster, name string) {
	t.Helper()
	e := masterAuthEnv(t, inst, c)
	e.mustKube("POST", "/api/v1/namespaces/default/configmaps", map[string]any{"metadata": map[string]any{"name": name}, "data": map[string]string{"k": name}}, nil)
}

func checkConfigMap(t *testing.T, e *env, name string) {
	t.Helper()
	var cmap struct{ Data map[string]string }
	if code, b := e.kube("GET", "/api/v1/namespaces/default/configmaps/"+name, "", nil, &cmap); code != http.StatusOK || cmap.Data["k"] != name {
		t.Errorf("configmap %s: %d %s", name, code, b)
	}
}

// masterAuthEnv is a Kubernetes client trusting only the cluster's
// masterAuth.clusterCaCertificate.
func masterAuthEnv(t *testing.T, inst *emutest.Instance, c *containerpb.Cluster) *env {
	t.Helper()
	pem, err := base64.StdEncoding.DecodeString(c.MasterAuth.GetClusterCaCertificate())
	pool := x509.NewCertPool()
	if err != nil || !pool.AppendCertsFromPEM(pem) {
		t.Fatalf("clusterCaCertificate: %v", err)
	}
	adm, _ := adminClient(t, inst, parent+"/clusters/"+c.Name)
	tr := adm.Transport.(*http.Transport)
	tr.TLSClientConfig = &tls.Config{RootCAs: pool, Certificates: tr.TLSClientConfig.Certificates}
	return &env{t: t, inst: inst, c: c, ca: pool, adm: adm}
}

func checkNodesReady(t *testing.T, e *env, want int) {
	t.Helper()
	eventually(t, 2*time.Minute, "nodes Ready", func() bool {
		var l struct {
			Items []struct {
				Status struct {
					Conditions []struct{ Type, Status string }
				}
			}
		}
		e.mustKube("GET", "/api/v1/nodes", nil, &l)
		ready := 0
		for _, n := range l.Items {
			for _, c := range n.Status.Conditions {
				if c.Type == "Ready" && c.Status == "True" {
					ready++
				}
			}
		}
		return len(l.Items) == want && ready == want
	})
}

// checkArchive checks that the snapshot holds one archive for the cluster,
// of the k3s server's server/ directory only.
func checkArchive(t *testing.T, snap, id string) {
	t.Helper()
	var files []string
	_ = filepath.WalkDir(filepath.Join(snap, "services", "gke"), func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			files = append(files, p)
		}
		return nil
	})
	want := filepath.Join(snap, "services", "gke", "clusters", id+".tar.gz")
	if len(files) != 1 || files[0] != want {
		t.Fatalf("gke snapshot files = %v, want [%s]", files, want)
	}
	f, err := os.Open(want)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(zr)
	db := false
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if h.Name != "server" && !strings.HasPrefix(h.Name, "server/") {
			t.Errorf("archive entry outside server/: %s", h.Name)
		}
		db = db || strings.HasPrefix(h.Name, "server/db/state.db")
	}
	if !db {
		t.Error("archive has no server/db/state.db")
	}
}
