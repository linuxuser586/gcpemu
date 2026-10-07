package gke_test

import (
	"context"
	"os"
	goruntime "runtime"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/container/apiv1/containerpb"

	"github.com/linuxuser586/gcpemu/emutest"
)

// kubeNodes lists the cluster's nodes.
func (e *env) kubeNodes() []struct {
	Metadata struct{ Labels map[string]string }
	Spec     struct {
		Taints []struct{ Key, Value, Effect string }
	}
	Status struct {
		NodeInfo struct {
			KubeletVersion, Architecture string
		}
	}
} {
	var l struct {
		Items []struct {
			Metadata struct{ Labels map[string]string }
			Spec     struct {
				Taints []struct{ Key, Value, Effect string }
			}
			Status struct {
				NodeInfo struct {
					KubeletVersion, Architecture string
				}
			}
		}
	}
	e.mustKube("GET", "/api/v1/nodes", nil, &l)
	return l.Items
}

// TestRegionalPoolsAndRoles covers FR-GKE-001 (regional clusters),
// FR-GKE-003 (node labels and taints, updates, node arch = host arch) and
// FR-GKE-004 (container.admin and container.developer).
func TestRegionalPoolsAndRoles(t *testing.T) {
	emutest.RequireRuntime(t)
	t.Parallel()
	inst := emutest.Start(t, []string{"gke"}, emutest.WithIAMMode("enforce"))
	cm := newClient(t, inst)
	ctx := context.Background()
	const region = "us-central1"
	rparent := "projects/" + project + "/locations/" + region
	op, err := cm.CreateCluster(ctx, &containerpb.CreateClusterRequest{Parent: rparent, Cluster: &containerpb.Cluster{
		Name: "reg", Locations: []string{"us-central1-a", "us-central1-b"},
		NodePools: []*containerpb.NodePool{{Name: "np", InitialNodeCount: 1, Config: &containerpb.NodeConfig{
			Labels: map[string]string{"tier": "a"},
			Taints: []*containerpb.NodeTaint{{Key: "dedicated", Value: "infra", Effect: containerpb.NodeTaint_NO_SCHEDULE}},
		}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	waitOp(t, cm, region, op.Name, 3*time.Minute)
	name := rparent + "/clusters/reg"
	c, err := cm.GetCluster(ctx, &containerpb.GetClusterRequest{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	if c.Location != region || c.CurrentNodeCount != 2 || len(c.NodePools[0].InstanceGroupUrls) != 2 {
		t.Fatalf("regional cluster: location %s, nodes %d, groups %v", c.Location, c.CurrentNodeCount, c.NodePools[0].InstanceGroupUrls)
	}
	adm, pool := adminClient(t, inst, name)
	e := &env{t: t, inst: inst, cm: cm, c: c, ca: pool, adm: adm}

	zones := map[string]bool{}
	for _, n := range e.kubeNodes() {
		zones[n.Metadata.Labels["topology.kubernetes.io/zone"]] = true
		if n.Metadata.Labels["kubernetes.io/arch"] != goruntime.GOARCH || n.Status.NodeInfo.Architecture != goruntime.GOARCH {
			t.Errorf("node arch %q/%q, host %s", n.Metadata.Labels["kubernetes.io/arch"], n.Status.NodeInfo.Architecture, goruntime.GOARCH)
		}
		if n.Metadata.Labels["tier"] != "a" || len(n.Spec.Taints) != 1 || n.Spec.Taints[0].Key != "dedicated" ||
			n.Spec.Taints[0].Value != "infra" || n.Spec.Taints[0].Effect != "NoSchedule" {
			t.Errorf("node labels %v, taints %v", n.Metadata.Labels, n.Spec.Taints)
		}
	}
	if !zones["us-central1-a"] || !zones["us-central1-b"] {
		t.Errorf("node zones = %v", zones)
	}

	// Label and taint updates reach the nodes.
	op, err = cm.UpdateNodePool(ctx, &containerpb.UpdateNodePoolRequest{Name: name + "/nodePools/np",
		Labels: &containerpb.NodeLabels{Labels: map[string]string{"tier": "b"}},
		Taints: &containerpb.NodeTaints{Taints: []*containerpb.NodeTaint{{Key: "spot", Value: "true", Effect: containerpb.NodeTaint_PREFER_NO_SCHEDULE}}}})
	if err != nil {
		t.Fatal(err)
	}
	waitOp(t, cm, region, op.Name, 3*time.Minute)
	eventually(t, 60*time.Second, "updated labels and taints on every node", func() bool {
		for _, n := range e.kubeNodes() {
			if n.Metadata.Labels["tier"] != "b" || len(n.Spec.Taints) != 1 || n.Spec.Taints[0].Key != "spot" || n.Spec.Taints[0].Effect != "PreferNoSchedule" {
				return false
			}
		}
		return true
	})

	// IAM roles map to Kubernetes access.
	devEmail, devTok := saToken(t, inst, "kube-developer")
	admEmail, admTok := saToken(t, inst, "kube-admin")
	policy := map[string]any{"policy": map[string]any{"bindings": []map[string]any{
		{"role": "roles/container.developer", "members": []string{"serviceAccount:" + devEmail}},
		{"role": "roles/container.admin", "members": []string{"serviceAccount:" + admEmail}},
	}}}
	if code := gw(t, inst, "POST", "/cloudresourcemanager/v1/projects/"+project+":setIamPolicy", policy, nil); code != 200 {
		t.Fatalf("setIamPolicy: %d", code)
	}
	role := func(n string) map[string]any {
		return map[string]any{"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "ClusterRole", "metadata": map[string]any{"name": n},
			"rules": []map[string]any{{"apiGroups": []string{""}, "resources": []string{"pods"}, "verbs": []string{"get"}}}}
	}
	eventually(t, 20*time.Second, "developer access", func() bool {
		code, _ := e.kube("GET", "/api/v1/namespaces/default/pods", devTok, nil, nil)
		return code == 200
	})
	if code, _ := e.kube("GET", "/api/v1/namespaces/default/secrets", devTok, nil, nil); code != 200 {
		t.Errorf("developer reading secrets: %d", code)
	}
	if code, b := e.kube("POST", "/apis/rbac.authorization.k8s.io/v1/clusterroles", devTok, role("dev-made"), nil); code != 403 {
		t.Errorf("developer creating a ClusterRole: %d %s", code, b)
	}
	if code, b := e.kube("POST", "/apis/rbac.authorization.k8s.io/v1/clusterroles", admTok, role("admin-made"), nil); code != 201 {
		t.Errorf("admin creating a ClusterRole: %d %s", code, b)
	}
}

// TestUpgrades is FR-GKE-002: getServerConfig offers the three most recent
// minors, and the control plane and node pools upgrade by version change.
// The older k3s image is pulled from Docker Hub (GCPEMU_NET_TESTS=1).
func TestUpgrades(t *testing.T) {
	emutest.RequireRuntime(t)
	t.Parallel()
	inst := emutest.Start(t, []string{"gke"})
	cm := newClient(t, inst)
	ctx := context.Background()
	sc, err := cm.GetServerConfig(ctx, &containerpb.GetServerConfigRequest{Name: parent})
	if err != nil {
		t.Fatal(err)
	}
	minors := func(vs []string) map[string]bool {
		m := map[string]bool{}
		for _, v := range vs {
			p := strings.SplitN(v, ".", 3)
			m[p[0]+"."+p[1]] = true
		}
		return m
	}
	if m := minors(sc.ValidMasterVersions); len(m) != 3 {
		t.Errorf("validMasterVersions %v span minors %v, want 3", sc.ValidMasterVersions, m)
	}
	if m := minors(sc.ValidNodeVersions); len(m) != 3 {
		t.Errorf("validNodeVersions %v span minors %v, want 3", sc.ValidNodeVersions, m)
	}
	if len(sc.Channels) < 3 || sc.DefaultClusterVersion == "" {
		t.Errorf("channels %v, default %q", sc.Channels, sc.DefaultClusterVersion)
	}
	if _, err := cm.UpdateMaster(ctx, &containerpb.UpdateMasterRequest{Name: parent + "/clusters/missing", MasterVersion: "1.37.1-gke.100"}); err == nil {
		t.Error("upgrading a missing cluster succeeded")
	}

	if os.Getenv("GCPEMU_NET_TESTS") != "1" {
		t.Skip("set GCPEMU_NET_TESTS=1 to pull an older k3s image and upgrade")
	}
	op, err := cm.CreateCluster(ctx, &containerpb.CreateClusterRequest{Parent: parent, Cluster: &containerpb.Cluster{
		Name: "up", InitialClusterVersion: "1.36.4-gke.100", NodePools: []*containerpb.NodePool{{Name: "np", InitialNodeCount: 1}}}})
	if err != nil {
		t.Fatal(err)
	}
	waitOp(t, cm, zone, op.Name, 5*time.Minute)
	name := parent + "/clusters/up"
	c, _ := cm.GetCluster(ctx, &containerpb.GetClusterRequest{Name: name})
	adm, pool := adminClient(t, inst, name)
	e := &env{t: t, inst: inst, cm: cm, c: c, ca: pool, adm: adm}
	kubeVersion := func() string {
		var v struct{ GitVersion string }
		e.mustKube("GET", "/version", nil, &v)
		return v.GitVersion
	}
	if v := kubeVersion(); !strings.HasPrefix(v, "v1.36.4") {
		t.Fatalf("initial server version %s", v)
	}
	if _, err := cm.UpdateMaster(ctx, &containerpb.UpdateMasterRequest{Name: name, MasterVersion: "1.35.9-gke.100"}); err == nil {
		t.Error("downgrade accepted")
	}

	op, err = cm.UpdateMaster(ctx, &containerpb.UpdateMasterRequest{Name: name, MasterVersion: "1.36.5-gke.100"})
	if err != nil {
		t.Fatal(err)
	}
	if op.OperationType != containerpb.Operation_UPGRADE_MASTER {
		t.Errorf("operation type %v", op.OperationType)
	}
	waitOp(t, cm, zone, op.Name, 5*time.Minute)
	c, _ = cm.GetCluster(ctx, &containerpb.GetClusterRequest{Name: name})
	if c.CurrentMasterVersion != "1.36.5-gke.100" {
		t.Errorf("currentMasterVersion %s", c.CurrentMasterVersion)
	}
	eventually(t, 2*time.Minute, "API server at v1.36.5", func() bool { return strings.HasPrefix(kubeVersion(), "v1.36.5") })

	op, err = cm.UpdateNodePool(ctx, &containerpb.UpdateNodePoolRequest{Name: name + "/nodePools/np", NodeVersion: "1.36.5-gke.100"})
	if err != nil {
		t.Fatal(err)
	}
	waitOp(t, cm, zone, op.Name, 5*time.Minute)
	np, _ := cm.GetNodePool(ctx, &containerpb.GetNodePoolRequest{Name: name + "/nodePools/np"})
	if np.Version != "1.36.5-gke.100" {
		t.Errorf("pool version %s", np.Version)
	}
	eventually(t, 2*time.Minute, "nodes at v1.36.5", func() bool {
		ns := e.kubeNodes()
		for _, n := range ns {
			if !strings.HasPrefix(n.Status.NodeInfo.KubeletVersion, "v1.36.5") {
				return false
			}
		}
		return len(ns) == 1
	})
}
