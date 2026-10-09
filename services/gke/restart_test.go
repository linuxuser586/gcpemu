package gke_test

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/container/apiv1/containerpb"

	"github.com/linuxuser586/gcpemu/emutest"
	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/instance"
	"github.com/linuxuser586/gcpemu/internal/runtime"
	"github.com/linuxuser586/gcpemu/services"
)

// startPersistent starts a --data-dir (non-ephemeral) instance in dir.
func startPersistent(t *testing.T, dir string) *emutest.Instance {
	t.Helper()
	cfg := config.Defaults()
	cfg.DataDir = dir
	cfg.Ephemeral = false // CI=true defaults to ephemeral
	cfg.Instance = "gke-restart"
	cfg.Services = []string{"gke"}
	cfg.LogLevel = "warn"
	cfg.Ports[config.AllPorts] = 0
	var out io.Writer = io.Discard
	if os.Getenv("GCPEMU_TEST_LOG") == "1" {
		out = os.Stderr
	}
	in, err := instance.New(&cfg, services.Factories(), out)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := in.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := in.WaitReady(ctx); err != nil {
		t.Fatal(err)
	}
	return &emutest.Instance{Instance: in}
}

// TestRestart checks FR-CORE-031: a cluster, its nodes and its Kubernetes
// state survive an emulator stop/start with the same data dir.
func TestRestart(t *testing.T) {
	emutest.RequireRuntime(t)
	t.Parallel()
	dir := t.TempDir()
	inst := startPersistent(t, dir)
	id := inst.ID
	t.Cleanup(func() {
		// The instance is not ephemeral: remove what it leaves behind.
		cl, err := runtime.Detect()
		if err != nil {
			return
		}
		m := &runtime.Manager{Client: cl, InstanceID: id}
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_ = m.Cleanup(ctx, true)
	})
	stopped := false
	defer func() {
		if !stopped {
			_ = inst.Shutdown(context.Background())
		}
	}()
	cm := newClient(t, inst)
	op, err := cm.CreateCluster(context.Background(), &containerpb.CreateClusterRequest{Parent: parent, Cluster: &containerpb.Cluster{
		Name: "keep", NodePools: []*containerpb.NodePool{
			{Name: "np", InitialNodeCount: 1},
			// An autoscaled pool without initialNodeCount starts at its minimum (#111).
			{Name: "auto", Autoscaling: &containerpb.NodePoolAutoscaling{Enabled: true, MinNodeCount: 1, MaxNodeCount: 1}},
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	waitOp(t, cm, zone, op.Name, 3*time.Minute)
	name := parent + "/clusters/keep"
	c, _ := cm.GetCluster(context.Background(), &containerpb.GetClusterRequest{Name: name})
	if c.CurrentNodeCount != 2 {
		t.Errorf("currentNodeCount = %d; want 2", c.CurrentNodeCount)
	}
	adm, pool := adminClient(t, inst, name)
	e := &env{t: t, inst: inst, c: c, ca: pool, adm: adm}
	e.mustKube("POST", "/api/v1/namespaces/default/configmaps", map[string]any{"metadata": map[string]any{"name": "persist"}, "data": map[string]string{"k": "v"}}, nil)

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := inst.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	stopped = true

	inst = startPersistent(t, dir)
	defer inst.Shutdown(context.Background())
	if inst.ID != id {
		t.Fatalf("instance ID changed: %s → %s", id, inst.ID)
	}
	cm = newClient(t, inst)
	for deadline := time.Now().Add(3 * time.Minute); ; time.Sleep(500 * time.Millisecond) {
		c, err = cm.GetCluster(context.Background(), &containerpb.GetClusterRequest{Name: name})
		if err == nil && c.Status == containerpb.Cluster_RUNNING {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("cluster not RUNNING 3m after restart: %v %v %q\n%s", err, c.GetStatus(), c.GetStatusMessage(), containerDiagnostics(id))
		}
	}
	adm, pool = adminClient(t, inst, name)
	e = &env{t: t, inst: inst, c: c, ca: pool, adm: adm}
	var cmap struct{ Data map[string]string }
	e.mustKube("GET", "/api/v1/namespaces/default/configmaps/persist", nil, &cmap)
	if cmap.Data["k"] != "v" {
		t.Errorf("configmap after restart = %v", cmap)
	}
	var nodes struct{ Items []any }
	e.mustKube("GET", "/api/v1/nodes", nil, &nodes)
	if len(nodes.Items) != 2 {
		t.Errorf("nodes after restart = %d", len(nodes.Items))
	}
	// Deleting removes the volumes too.
	op, err = cm.DeleteCluster(context.Background(), &containerpb.DeleteClusterRequest{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	waitOp(t, cm, zone, op.Name, 2*time.Minute)
	cl, _ := runtime.Detect()
	vols, _ := cl.ListVolumes(context.Background(), map[string]string{runtime.LabelInstance: id, runtime.LabelService: "gke"})
	if len(vols) != 0 {
		t.Errorf("volumes left after delete: %v", vols)
	}
}

// containerDiagnostics describes an instance's containers and the tails of
// their logs, for failures that only happen on CI hosts.
func containerDiagnostics(instanceID string) string {
	cl, err := runtime.Detect()
	if err != nil {
		return err.Error()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cs, err := cl.ListContainers(ctx, map[string]string{runtime.LabelInstance: instanceID})
	if err != nil {
		return err.Error()
	}
	var b strings.Builder
	for _, c := range cs {
		fmt.Fprintf(&b, "--- %s %s (%s, exit %d) %v\n", c.Name, c.Image, c.Status, c.ExitCode, c.IPs)
		logs, _ := cl.Logs(ctx, c.ID, 30)
		b.WriteString(logs)
		b.WriteString("\n")
	}
	return b.String()
}
