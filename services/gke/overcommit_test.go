package gke

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/container/apiv1/containerpb"

	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/runtime"
)

const mib = 1 << 20

// syncBuffer is a log sink safe for the Service's concurrent writers.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// overcommitAPI is an api on a fake runtime whose machine has memTotal
// bytes, with a Node limit of 20 and its log captured.
func overcommitAPI(t *testing.T, memTotal int64) (*api, *infoOnly, *syncBuffer) {
	t.Helper()
	a := newAPI(t)
	a.s.env.Config.GKEMaxNodes = 20
	rt := &infoOnly{info: runtime.Info{Name: "docker", Version: "29.0.0", MemTotal: memTotal}}
	a.s.env.Containers = rt
	logs := &syncBuffer{}
	a.s.env.Log = slog.New(slog.NewTextHandler(logs, nil))
	return a, rt, logs
}

// warnings counts the overcommit warnings logged.
func warnings(logs *syncBuffer) int {
	n := 0
	for _, l := range strings.Split(logs.String(), "\n") {
		if strings.Contains(l, "level=WARN") && strings.Contains(l, "overcommit the Runtime machine") {
			n++
		}
	}
	return n
}

// resizeRunning resizes pool to n nodes, waits for the operation and marks
// the cluster RUNNING again, as a finished bring-up would. Overcommit never
// fails the request.
func resizeRunning(t *testing.T, a *api, cluster, pool string, n int32) {
	t.Helper()
	ctx := context.Background()
	r, _ := parseRef(cluster, "", "", "", "", 1)
	op, err := a.SetNodePoolSize(ctx, &containerpb.SetNodePoolSizeRequest{Name: cluster + "/nodePools/" + pool, NodeCount: n})
	if err != nil {
		t.Fatalf("resize %s to %d: %v", pool, n, err)
	}
	waitOp(t, a, r.Project, op.Name)
	if err := a.s.updateCluster(r.key(), func(_ *clusterRecord, c *containerpb.Cluster) error {
		c.Status = containerpb.Cluster_RUNNING
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// overcommitOf is the Service's /_emu/v1/info overcommit report, or nil.
func overcommitOf(t *testing.T, a *api) *overcommit {
	t.Helper()
	return a.s.Info().(info).Overcommit
}

// deleteCluster deletes cluster and waits for its operation.
func deleteCluster(t *testing.T, a *api, cluster string) {
	t.Helper()
	r, _ := parseRef(cluster, "", "", "", "", 1)
	op, err := a.DeleteCluster(context.Background(), &containerpb.DeleteClusterRequest{Name: cluster})
	if err != nil {
		t.Fatalf("delete %s: %v", cluster, err)
	}
	waitOp(t, a, r.Project, op.Name)
}

// TestOvercommitWarnsOnceEachCrossing (#1070): on a 4 GiB Runtime machine,
// C clusters with N nodes need C × 768 MiB + N × 256 MiB + 2 GiB. Nodes of
// both clusters are summed: c1's 3 nodes (3584 MiB) and c2's 1 (3072 MiB)
// are each within the machine and together (4608 MiB) beyond it. Crossing
// logs one warning; more nodes log no more; dropping to the machine's
// memory (4096 MiB is not beyond it) and crossing again logs once more.
// Every request is accepted.
func TestOvercommitWarnsOnceEachCrossing(t *testing.T) {
	a, _, logs := overcommitAPI(t, 4096*mib)
	const parent = "projects/overcommit-test/locations/us-central1-a"
	pool := func(n int32) []*containerpb.NodePool {
		return []*containerpb.NodePool{{Name: "np", InitialNodeCount: n}}
	}
	want := func(step string, warned int, oc overcommit) {
		t.Helper()
		if n := warnings(logs); n != warned {
			t.Fatalf("%s: %d warnings, want %d\n%s", step, n, warned, logs)
		}
		oc.MachineBytes = 4096 * mib
		if got := overcommitOf(t, a); got == nil || *got != oc {
			t.Fatalf("%s: overcommit = %+v, want %+v", step, got, oc)
		}
	}

	createRunning(t, a, parent, &containerpb.Cluster{Name: "c1", NodePools: pool(3)})
	want("c1 alone", 0, overcommit{EstimateBytes: 3584 * mib, Nodes: 3})

	createRunning(t, a, parent, &containerpb.Cluster{Name: "c2", NodePools: pool(1)})
	want("c1 and c2", 1, overcommit{EstimateBytes: 4608 * mib, Nodes: 4, Overcommitted: true})
	l := logs.String()
	for _, w := range []string{"nodes=4", "clusters=2", `estimate="4.50 GiB"`, `machine="4.00 GiB"`, "GKE sizing", "not refused"} {
		if !strings.Contains(l, w) {
			t.Errorf("warning lacks %q:\n%s", w, l)
		}
	}

	resizeRunning(t, a, parent+"/clusters/c1", "np", 5)
	want("c1 grown", 1, overcommit{EstimateBytes: 5120 * mib, Nodes: 6, Overcommitted: true})

	deleteCluster(t, a, parent+"/clusters/c2")
	want("c2 deleted", 1, overcommit{EstimateBytes: 4096 * mib, Nodes: 5})

	createRunning(t, a, parent, &containerpb.Cluster{Name: "c3", NodePools: pool(1)})
	want("c3 created", 2, overcommit{EstimateBytes: 5120 * mib, Nodes: 6, Overcommitted: true})
}

// TestOvercommitUnknownMemory (#1070): a runtime that reports no memory
// skips the check: no report, no warning, and every request accepted.
func TestOvercommitUnknownMemory(t *testing.T) {
	a, _, logs := overcommitAPI(t, 0)
	const parent = "projects/overcommit-test/locations/us-central1-a"
	createRunning(t, a, parent, &containerpb.Cluster{Name: "c1", NodePools: []*containerpb.NodePool{{Name: "np", InitialNodeCount: 20}}})
	if n := warnings(logs); n != 0 {
		t.Errorf("%d warnings, want 0", n)
	}
	b, err := json.Marshal(a.s.Info())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "overcommit") {
		t.Errorf("Info() = %s; want no overcommit", b)
	}
}

// TestOvercommitWarnsOnRestart (#1070): a Persistent Instance that starts
// with nodes beyond its Runtime machine's memory warns once, though no
// node count changes.
func TestOvercommitWarnsOnRestart(t *testing.T) {
	a, rt, _ := overcommitAPI(t, 4096*mib)
	const parent = "projects/overcommit-test/locations/us-central1-a"
	for _, name := range []string{"c1", "c2"} {
		createRunning(t, a, parent, &containerpb.Cluster{Name: name, NodePools: []*containerpb.NodePool{{Name: "np", InitialNodeCount: 3}}})
	}
	if err := a.s.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}

	// A new Service on the same Store, as a Persistent Instance restarts.
	old := a.s.env
	logs := &syncBuffer{}
	log := slog.New(slog.NewTextHandler(logs, nil))
	s := New(&emu.Env{Config: old.Config, Store: old.Store, Clock: old.Clock, IDs: old.IDs, Log: log,
		Auth: old.Auth, Endpoints: emu.NewEndpoints(), Containers: rt}).(*Service)
	t.Cleanup(func() { _ = s.Stop(context.Background()) })
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Both clusters' restores run, and fail without a netplane.
	deadline := time.Now().Add(10 * time.Second)
	for !restored(s, parent+"/clusters/c1", parent+"/clusters/c2") {
		if time.Now().After(deadline) {
			t.Fatalf("clusters not restored:\n%s", logs)
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Stop waits for the restores' background work.
	if err := s.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := warnings(logs); n != 1 {
		t.Fatalf("%d warnings, want 1\n%s", n, logs)
	}
	if got := s.overcommitReport(); got == nil || !got.Overcommitted || got.Nodes != 6 {
		t.Errorf("overcommit = %+v", got)
	}
}

// restored reports whether every cluster's restore has ended: none is
// RECONCILING.
func restored(s *Service, clusters ...string) bool {
	for _, c := range clusters {
		r, _ := parseRef(c, "", "", "", "", 1)
		rec, err := s.loadKey(r.key())
		if err != nil || rec.cluster().Status == containerpb.Cluster_RECONCILING {
			return false
		}
	}
	return true
}

// connectedOnly is a runtime known only through emu.ConnectedRuntime, as
// the Instance's is: m is nil until something has connected it.
type connectedOnly struct {
	emu.Containers
	m *runtime.Manager
}

func (c *connectedOnly) Connected() *runtime.Manager { return c.m }

func (c *connectedOnly) Runtime(context.Context) (*runtime.Manager, error) {
	panic("estimating GKE overcommit connected the runtime")
}

// TestOvercommitNeverConnects (#1070): the estimate uses the runtime only
// once something else has connected it; until then the Runtime machine's
// memory is unknown.
func TestOvercommitNeverConnects(t *testing.T) {
	a := newAPI(t)
	c := &connectedOnly{}
	a.s.env.Containers = c
	a.s.reestimate(context.Background())
	if got := a.s.overcommitReport(); got != nil {
		t.Fatalf("not connected: overcommit = %+v, want none", got)
	}
	c.m = &runtime.Manager{Info: runtime.Info{MemTotal: 1 << 30}}
	a.s.reestimate(context.Background())
	if got := a.s.overcommitReport(); got == nil || !got.Overcommitted || got.MachineBytes != 1<<30 || got.EstimateBytes != headroomBytes {
		t.Fatalf("connected: overcommit = %+v", got)
	}
}
