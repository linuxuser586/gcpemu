package gke

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"cloud.google.com/go/container/apiv1/containerpb"
	"google.golang.org/grpc/codes"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/clock"
	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/store"
)

const apParent = "projects/ap-test/locations/us-central1"

// newAPI returns the container API of a Service with an in-memory store and
// no container runtime: clusters are stored, and their bring-up fails.
// It is Deterministic, so store-only operations are DONE when they return;
// cluster create is container-backed and still runs in the background
// (ADR 0020), which createRunning waits for.
func newAPI(t *testing.T) *api {
	t.Helper()
	cfg := config.Defaults()
	cfg.Deterministic = true
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := New(&emu.Env{Config: &cfg, Store: store.NewMemory(), Clock: clock.Real{}, IDs: emu.NewIDs(true), Log: log,
		Auth: emu.NewPolicyAuthorizer(config.IAMOff, log), Endpoints: emu.NewEndpoints()}).(*Service)
	t.Cleanup(func() { _ = s.Stop(context.Background()) })
	return &api{s: s}
}

// createRunning creates c under parent, waits for its create operation to
// end and marks the cluster RUNNING, as a bring-up would.
func createRunning(t *testing.T, a *api, parent string, c *containerpb.Cluster) *containerpb.Cluster {
	t.Helper()
	ctx := context.Background()
	op, err := a.CreateCluster(ctx, &containerpb.CreateClusterRequest{Parent: parent, Cluster: c})
	if err != nil {
		t.Fatalf("CreateCluster: %v", err)
	}
	r, _ := parseRef(parent, "", "", "", "", 0)
	r.Cluster = c.Name
	waitOp(t, a, r.Project, op.Name)
	if err := a.s.updateCluster(r.key(), func(_ *clusterRecord, c *containerpb.Cluster) error {
		c.Status = containerpb.Cluster_RUNNING
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	got, err := a.GetCluster(ctx, &containerpb.GetClusterRequest{Name: r.clusterName()})
	if err != nil {
		t.Fatalf("GetCluster: %v", err)
	}
	return got
}

// waitOp waits for the operation named name in project to end.
func waitOp(t *testing.T, a *api, project, name string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		got, err := a.s.getOp(project, name)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status == containerpb.Operation_DONE {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("operation not done: %v", got)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func autopilot(name string) *containerpb.Cluster {
	return &containerpb.Cluster{Name: name, Autopilot: &containerpb.Autopilot{Enabled: true}}
}

func wantCode(t *testing.T, what string, err error, code codes.Code) {
	t.Helper()
	if err == nil {
		t.Errorf("%s: accepted; want %v", what, code)
		return
	}
	if got := apierr.From(err).Code; got != code {
		t.Errorf("%s: %v; want %v", what, err, code)
	}
}

// TestAutopilotCreate is FR-GKE-012: GKE sets Workload Identity, Dataplane
// V2, a release channel and Shielded nodes on an Autopilot cluster, and
// reports the pool it manages, which runs one node until node provisioning
// (FR-GKE-015) adds more.
func TestAutopilotCreate(t *testing.T) {
	a := newAPI(t)
	c := autopilot("ap")
	// GKE owns the nodes, so a node config in the request is not theirs.
	c.NodeConfig = &containerpb.NodeConfig{MachineType: "e2-standard-8"}
	c = createRunning(t, a, apParent, c)
	if !c.GetAutopilot().GetEnabled() {
		t.Error("autopilot.enabled not reported")
	}
	if got := c.GetWorkloadIdentityConfig().GetWorkloadPool(); got != "ap-test.svc.id.goog" {
		t.Errorf("workloadPool = %q", got)
	}
	if got := c.GetNetworkConfig().GetDatapathProvider(); got != containerpb.DatapathProvider_ADVANCED_DATAPATH {
		t.Errorf("datapathProvider = %v", got)
	}
	if got := c.GetReleaseChannel().GetChannel(); got != containerpb.ReleaseChannel_REGULAR {
		t.Errorf("releaseChannel = %v", got)
	}
	if !c.GetShieldedNodes().GetEnabled() {
		t.Error("shieldedNodes not enabled")
	}
	if !c.GetVerticalPodAutoscaling().GetEnabled() {
		t.Error("verticalPodAutoscaling not enabled")
	}
	if c.InitialNodeCount != 0 {
		t.Errorf("initialNodeCount = %d", c.InitialNodeCount)
	}
	if c.CurrentNodeCount != 1 || len(c.NodePools) != 1 || c.NodePools[0].Name != "default-pool" {
		t.Fatalf("nodes = %d, pools = %v", c.CurrentNodeCount, c.NodePools)
	}
	if got := c.NodePools[0].GetConfig().GetWorkloadMetadataConfig().GetMode(); got != containerpb.WorkloadMetadataConfig_GKE_METADATA {
		t.Errorf("pool workloadMetadataConfig = %v", got)
	}
	if got := c.NodePools[0].GetConfig().GetMachineType(); got != "e2-medium" {
		t.Errorf("pool machineType = %q, want GKE's default", got)
	}
	pools, err := a.ListNodePools(context.Background(), &containerpb.ListNodePoolsRequest{Parent: apParent + "/clusters/ap"})
	if err != nil || len(pools.NodePools) != 1 {
		t.Errorf("ListNodePools = %v, %v", pools, err)
	}
	list, err := a.ListClusters(context.Background(), &containerpb.ListClustersRequest{Parent: "projects/ap-test/locations/-"})
	if err != nil || len(list.Clusters) != 1 || !list.Clusters[0].GetAutopilot().GetEnabled() || !list.Clusters[0].GetShieldedNodes().GetEnabled() {
		t.Errorf("ListClusters = %v, %v", list, err)
	}
}

// TestAutopilotCreateGcloud is FR-GKE-012: gcloud container clusters
// create-auto sends a default-pool with gcloud's node scopes and the
// cluster's labels, which GKE accepts and replaces with its own node pool.
func TestAutopilotCreateGcloud(t *testing.T) {
	a := newAPI(t)
	c := autopilot("ap")
	c.NodePools = []*containerpb.NodePool{{
		Name:             "default-pool",
		InitialNodeCount: 1,
		Config: &containerpb.NodeConfig{
			OauthScopes:    []string{"https://www.googleapis.com/auth/devstorage.read_only"},
			ResourceLabels: map[string]string{"team": "web"},
		},
	}}
	c = createRunning(t, a, apParent, c)
	if len(c.NodePools) != 1 {
		t.Fatalf("pools = %v, want GKE's own node pool", c.NodePools)
	}
	if cfg := c.NodePools[0].GetConfig(); cfg.GetMachineType() != "e2-medium" || len(cfg.GetOauthScopes()) == 0 {
		t.Errorf("pool config = %v, want GKE's own node pool's", cfg)
	}
	if c.CurrentNodeCount != 1 {
		t.Errorf("nodes = %d", c.CurrentNodeCount)
	}
}

// TestAutopilotCreateRejects is FR-GKE-012: an Autopilot cluster is
// regional, and a request that contradicts what GKE sets is refused.
func TestAutopilotCreateRejects(t *testing.T) {
	a := newAPI(t)
	tests := []struct {
		name   string
		parent string
		edit   func(c *containerpb.Cluster)
	}{
		{"zonal", "projects/ap-test/locations/us-central1-a", nil},
		{"Workload Identity off", apParent, func(c *containerpb.Cluster) {
			c.WorkloadIdentityConfig = &containerpb.WorkloadIdentityConfig{}
		}},
		{"other workload pool", apParent, func(c *containerpb.Cluster) {
			c.WorkloadIdentityConfig = &containerpb.WorkloadIdentityConfig{WorkloadPool: "other.svc.id.goog"}
		}},
		{"Shielded nodes off", apParent, func(c *containerpb.Cluster) { c.ShieldedNodes = &containerpb.ShieldedNodes{} }},
		{"no release channel", apParent, func(c *containerpb.Cluster) { c.ReleaseChannel = &containerpb.ReleaseChannel{} }},
		{"legacy datapath", apParent, func(c *containerpb.Cluster) {
			c.NetworkConfig = &containerpb.NetworkConfig{DatapathProvider: containerpb.DatapathProvider_LEGACY_DATAPATH}
		}},
		{"network policy", apParent, func(c *containerpb.Cluster) { c.NetworkPolicy = &containerpb.NetworkPolicy{Enabled: true} }},
		{"node pools", apParent, func(c *containerpb.Cluster) { c.NodePools = []*containerpb.NodePool{{Name: "p", InitialNodeCount: 1}} }},
		{"default-pool with a machine type", apParent, func(c *containerpb.Cluster) {
			c.NodePools = []*containerpb.NodePool{{Name: "default-pool", InitialNodeCount: 1, Config: &containerpb.NodeConfig{MachineType: "e2-small"}}}
		}},
		{"initial node count", apParent, func(c *containerpb.Cluster) { c.InitialNodeCount = 1 }},
		{"vertical Pod autoscaling off", apParent, func(c *containerpb.Cluster) {
			c.VerticalPodAutoscaling = &containerpb.VerticalPodAutoscaling{}
		}},
	}
	for _, tt := range tests {
		c := autopilot("ap")
		if tt.edit != nil {
			tt.edit(c)
		}
		_, err := a.CreateCluster(context.Background(), &containerpb.CreateClusterRequest{Parent: tt.parent, Cluster: c})
		wantCode(t, tt.name, err, codes.InvalidArgument)
	}
	_, err := a.CreateCluster(context.Background(), &containerpb.CreateClusterRequest{Parent: "projects/ap-test/locations/us-central1-a", Cluster: autopilot("ap")})
	if err == nil || apierr.From(err).Message != "Autopilot clusters must be regional clusters." {
		t.Errorf("zonal: %v", err)
	}
}

// TestAutopilotNodePools is FR-GKE-012: the node pools API reports the
// pool GKE manages, and refuses to create, change or delete pools.
func TestAutopilotNodePools(t *testing.T) {
	a := newAPI(t)
	ctx := context.Background()
	createRunning(t, a, apParent, autopilot("ap"))
	cl := apParent + "/clusters/ap"
	pool := cl + "/nodePools/default-pool"
	if np, err := a.GetNodePool(ctx, &containerpb.GetNodePoolRequest{Name: pool}); err != nil || np.Name != "default-pool" {
		t.Fatalf("GetNodePool = %v, %v", np, err)
	}
	_, err := a.CreateNodePool(ctx, &containerpb.CreateNodePoolRequest{Parent: cl, NodePool: &containerpb.NodePool{Name: "extra", InitialNodeCount: 1}})
	wantCode(t, "create", err, codes.FailedPrecondition)
	_, err = a.UpdateNodePool(ctx, &containerpb.UpdateNodePoolRequest{Name: pool, Labels: &containerpb.NodeLabels{Labels: map[string]string{"a": "b"}}})
	wantCode(t, "update", err, codes.FailedPrecondition)
	_, err = a.UpdateNodePool(ctx, &containerpb.UpdateNodePoolRequest{Name: pool, NodeVersion: "-"})
	wantCode(t, "upgrade", err, codes.FailedPrecondition)
	_, err = a.SetNodePoolSize(ctx, &containerpb.SetNodePoolSizeRequest{Name: pool, NodeCount: 2})
	wantCode(t, "set size", err, codes.FailedPrecondition)
	_, err = a.SetNodePoolAutoscaling(ctx, &containerpb.SetNodePoolAutoscalingRequest{Name: pool, Autoscaling: &containerpb.NodePoolAutoscaling{Enabled: true}})
	wantCode(t, "autoscaling", err, codes.FailedPrecondition)
	_, err = a.SetNodePoolManagement(ctx, &containerpb.SetNodePoolManagementRequest{Name: pool, Management: &containerpb.NodeManagement{}})
	wantCode(t, "management", err, codes.FailedPrecondition)
	_, err = a.UpdateCluster(ctx, &containerpb.UpdateClusterRequest{Name: cl, Update: &containerpb.ClusterUpdate{
		DesiredNodePoolId: "default-pool", DesiredNodePoolAutoscaling: &containerpb.NodePoolAutoscaling{Enabled: true}}})
	wantCode(t, "cluster update of a pool", err, codes.FailedPrecondition)
	_, err = a.DeleteNodePool(ctx, &containerpb.DeleteNodePoolRequest{Name: pool})
	wantCode(t, "delete", err, codes.FailedPrecondition)
	c, err := a.GetCluster(ctx, &containerpb.GetClusterRequest{Name: cl})
	if err != nil || len(c.NodePools) != 1 || c.CurrentNodeCount != 1 {
		t.Errorf("after refused calls: %v, %v", c, err)
	}
}

// TestAutopilotUpdate is FR-GKE-012: workloadPolicyConfig.allowNetAdmin is
// stored and updatable, and an update that would undo what GKE sets on an
// Autopilot cluster is refused.
func TestAutopilotUpdate(t *testing.T) {
	a := newAPI(t)
	ctx := context.Background()
	yes := true
	c := autopilot("ap")
	c.Autopilot.WorkloadPolicyConfig = &containerpb.WorkloadPolicyConfig{AllowNetAdmin: &yes}
	if got := createRunning(t, a, apParent, c); !got.GetAutopilot().GetWorkloadPolicyConfig().GetAllowNetAdmin() {
		t.Fatalf("allowNetAdmin not stored: %v", got.Autopilot)
	}
	cl := apParent + "/clusters/ap"
	no := false
	if _, err := a.UpdateCluster(ctx, &containerpb.UpdateClusterRequest{Name: cl, Update: &containerpb.ClusterUpdate{
		DesiredAutopilotWorkloadPolicyConfig: &containerpb.WorkloadPolicyConfig{AllowNetAdmin: &no}}}); err != nil {
		t.Fatalf("update allowNetAdmin: %v", err)
	}
	got, _ := a.GetCluster(ctx, &containerpb.GetClusterRequest{Name: cl})
	if wp := got.GetAutopilot().GetWorkloadPolicyConfig(); wp == nil || wp.AllowNetAdmin == nil || *wp.AllowNetAdmin || !got.Autopilot.Enabled {
		t.Errorf("after update: %v", got.Autopilot)
	}

	for name, u := range map[string]*containerpb.ClusterUpdate{
		"Workload Identity off":        {DesiredWorkloadIdentityConfig: &containerpb.WorkloadIdentityConfig{}},
		"Shielded nodes off":           {DesiredShieldedNodes: &containerpb.ShieldedNodes{}},
		"no release channel":           {DesiredReleaseChannel: &containerpb.ReleaseChannel{}},
		"vertical Pod autoscaling off": {DesiredVerticalPodAutoscaling: &containerpb.VerticalPodAutoscaling{}},
		"legacy datapath":              {DesiredDatapathProvider: containerpb.DatapathProvider_LEGACY_DATAPATH},
	} {
		_, err := a.UpdateCluster(ctx, &containerpb.UpdateClusterRequest{Name: cl, Update: u})
		wantCode(t, name, err, codes.InvalidArgument)
	}
	_, err := a.SetNetworkPolicy(ctx, &containerpb.SetNetworkPolicyRequest{Name: cl, NetworkPolicy: &containerpb.NetworkPolicy{Enabled: true}})
	wantCode(t, "network policy", err, codes.InvalidArgument)

	// A Standard cluster has no Autopilot settings to update.
	createRunning(t, a, apParent, &containerpb.Cluster{Name: "std", InitialNodeCount: 1})
	_, err = a.UpdateCluster(ctx, &containerpb.UpdateClusterRequest{Name: apParent + "/clusters/std", Update: &containerpb.ClusterUpdate{
		DesiredAutopilotWorkloadPolicyConfig: &containerpb.WorkloadPolicyConfig{AllowNetAdmin: &yes}}})
	wantCode(t, "Standard allowNetAdmin", err, codes.InvalidArgument)
	got, _ = a.GetCluster(ctx, &containerpb.GetClusterRequest{Name: apParent + "/clusters/std"})
	if got.Autopilot != nil || got.ShieldedNodes != nil {
		t.Errorf("Standard cluster got Autopilot settings: %v %v", got.Autopilot, got.ShieldedNodes)
	}
}

// TestStandardCompatibilityAuditing is FR-GKE-017: a Standard cluster
// stores workloadPolicyConfig.autopilotCompatibilityAuditingEnabled and
// still refuses every other workload policy.
func TestStandardCompatibilityAuditing(t *testing.T) {
	a := newAPI(t)
	ctx := context.Background()
	createRunning(t, a, apParent, &containerpb.Cluster{Name: "std", InitialNodeCount: 1})
	cl := apParent + "/clusters/std"
	yes := true
	if _, err := a.UpdateCluster(ctx, &containerpb.UpdateClusterRequest{Name: cl, Update: &containerpb.ClusterUpdate{
		DesiredAutopilotWorkloadPolicyConfig: &containerpb.WorkloadPolicyConfig{AutopilotCompatibilityAuditingEnabled: &yes}}}); err != nil {
		t.Fatalf("enable auditing: %v", err)
	}
	got, _ := a.GetCluster(ctx, &containerpb.GetClusterRequest{Name: cl})
	if !got.GetAutopilot().GetWorkloadPolicyConfig().GetAutopilotCompatibilityAuditingEnabled() || got.Autopilot.Enabled {
		t.Errorf("after enabling auditing: %v", got.Autopilot)
	}
	_, err := a.UpdateCluster(ctx, &containerpb.UpdateClusterRequest{Name: cl, Update: &containerpb.ClusterUpdate{
		DesiredAutopilotWorkloadPolicyConfig: &containerpb.WorkloadPolicyConfig{AutopilotCompatibilityAuditingEnabled: &yes, AllowNetAdmin: &yes}}})
	wantCode(t, "Standard allowNetAdmin with auditing", err, codes.InvalidArgument)
	no := false
	if _, err := a.UpdateCluster(ctx, &containerpb.UpdateClusterRequest{Name: cl, Update: &containerpb.ClusterUpdate{
		DesiredAutopilotWorkloadPolicyConfig: &containerpb.WorkloadPolicyConfig{AutopilotCompatibilityAuditingEnabled: &no}}}); err != nil {
		t.Fatalf("disable auditing: %v", err)
	}
	got, _ = a.GetCluster(ctx, &containerpb.GetClusterRequest{Name: cl})
	if wp := got.GetAutopilot().GetWorkloadPolicyConfig(); wp.AutopilotCompatibilityAuditingEnabled == nil || *wp.AutopilotCompatibilityAuditingEnabled || wp.AllowNetAdmin != nil {
		t.Errorf("after disabling auditing: %v", got.Autopilot)
	}
}
