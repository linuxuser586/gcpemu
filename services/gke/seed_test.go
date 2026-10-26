package gke

import (
	"context"
	"strings"
	"testing"

	"cloud.google.com/go/container/apiv1/containerpb"
	"google.golang.org/protobuf/proto"
	"gopkg.in/yaml.v3"

	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// TestExportSeedUnit checks FR-CORE-013 without a container runtime:
// cluster records written to the Store export as the "gke" section with
// only what differs from the seed's defaults.
func TestExportSeedUnit(t *testing.T) {
	s := lanTestService(config.IAMOff)
	if sec, err := s.ExportSeed(context.Background()); err != nil || sec != nil {
		t.Fatalf("empty export = %v, %v", sec, err)
	}
	put := func(project, location string, c *containerpb.Cluster) {
		rec := &clusterRecord{Int: clusterInternal{Project: project, Location: location, Name: c.Name}}
		rec.setCluster(c)
		if err := s.env.Store.Update(func(tx store.Tx) error { return putCluster(tx, rec) }); err != nil {
			t.Fatal(err)
		}
	}
	pool := func(name string, n int32, cfg *containerpb.NodeConfig) *containerpb.NodePool {
		if cfg.MachineType == "" {
			cfg.MachineType = defaultMachineType
		}
		if cfg.ServiceAccount == "" {
			cfg.ServiceAccount = "default"
		}
		cfg.DiskSizeGb, cfg.ImageType = defaultDiskSizeGB, defaultImageType
		return &containerpb.NodePool{Name: name, InitialNodeCount: n, Version: defaultVersion, Config: cfg}
	}
	put("my-project", "us-central1-a", &containerpb.Cluster{
		Name: "prod", Id: "abc", Status: containerpb.Cluster_RUNNING,
		CurrentMasterVersion: "1.35.9-gke.100", Network: "vpc", Subnetwork: "nodes",
		PrivateClusterConfig:   &containerpb.PrivateClusterConfig{EnablePrivateNodes: true},
		WorkloadIdentityConfig: &containerpb.WorkloadIdentityConfig{WorkloadPool: "my-project.svc.id.goog"},
		NodePools: []*containerpb.NodePool{
			pool("default-pool", 1, &containerpb.NodeConfig{}),
			pool("big", 3, &containerpb.NodeConfig{MachineType: "e2-standard-4", Labels: map[string]string{"team": "web"},
				ServiceAccount: "nodes@my-project.iam.gserviceaccount.com"}),
		},
	})
	put("my-project", "europe-west1", &containerpb.Cluster{
		Name: "dev", CurrentMasterVersion: defaultVersion, Network: "default", Subnetwork: "default",
		NodePools: []*containerpb.NodePool{pool("default-pool", 1, &containerpb.NodeConfig{})},
	})
	put("a-project", "us-east1-b", &containerpb.Cluster{
		Name: "dev", CurrentMasterVersion: defaultVersion, Network: "default", Subnetwork: "default",
		NodePools: []*containerpb.NodePool{pool("default-pool", 2, &containerpb.NodeConfig{})},
	})

	sec, err := s.ExportSeed(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	b, err := yaml.Marshal(sec)
	if err != nil {
		t.Fatal(err)
	}
	want := `clusters:
    - project: a-project
      location: us-east1-b
      name: dev
      nodePools:
        - name: default-pool
          nodeCount: 2
    - project: my-project
      location: europe-west1
      name: dev
    - project: my-project
      location: us-central1-a
      name: prod
      version: 1.35.9-gke.100
      network: vpc
      subnetwork: nodes
      privateNodes: true
      workloadIdentity: true
      nodePools:
        - name: default-pool
        - name: big
          nodeCount: 3
          machineType: e2-standard-4
          labels:
            team: web
          serviceAccount: nodes@my-project.iam.gserviceaccount.com
`
	if string(b) != want {
		t.Fatalf("export =\n%s\nwant\n%s", b, want)
	}
	// The section decodes as the seed reads it.
	var f seedFile
	if err := yaml.Unmarshal(b, &f); err != nil || len(f.Clusters) != 3 || f.Clusters[2].NodePools[1].NodeCount != 3 {
		t.Fatalf("decode = %+v, %v", f, err)
	}
}

// TestExportSeedAutopilot checks that an Autopilot cluster exports its
// Cluster mode and NET_ADMIN setting, and none of what GKE sets on it:
// its own node pools and Workload Identity.
func TestExportSeedAutopilot(t *testing.T) {
	s := lanTestService(config.IAMOff)
	put := func(c *containerpb.Cluster) {
		rec := &clusterRecord{Int: clusterInternal{Project: "my-project", Location: "us-central1", Name: c.Name}}
		rec.setCluster(c)
		if err := s.env.Store.Update(func(tx store.Tx) error { return putCluster(tx, rec) }); err != nil {
			t.Fatal(err)
		}
	}
	pools := func() []*containerpb.NodePool {
		return []*containerpb.NodePool{
			{Name: "default-pool", Version: defaultVersion, Config: &containerpb.NodeConfig{MachineType: "ek-standard-8", ServiceAccount: "default"}},
			{Name: "nap-spot", InitialNodeCount: 2, Version: defaultVersion, Config: &containerpb.NodeConfig{Spot: true, ServiceAccount: "default"}},
		}
	}
	put(&containerpb.Cluster{
		Name: "auto", CurrentMasterVersion: defaultVersion, Network: "default", Subnetwork: "default",
		Autopilot:              &containerpb.Autopilot{Enabled: true},
		WorkloadIdentityConfig: &containerpb.WorkloadIdentityConfig{WorkloadPool: "my-project.svc.id.goog"},
		NodePools:              pools(),
	})
	put(&containerpb.Cluster{
		Name: "netadmin", CurrentMasterVersion: "1.35.9-gke.100", Network: "vpc", Subnetwork: "nodes",
		Autopilot:              &containerpb.Autopilot{Enabled: true, WorkloadPolicyConfig: &containerpb.WorkloadPolicyConfig{AllowNetAdmin: proto.Bool(true)}},
		PrivateClusterConfig:   &containerpb.PrivateClusterConfig{EnablePrivateNodes: true},
		WorkloadIdentityConfig: &containerpb.WorkloadIdentityConfig{WorkloadPool: "my-project.svc.id.goog"},
		NodePools:              pools(),
	})

	sec, err := s.ExportSeed(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	b, err := yaml.Marshal(sec)
	if err != nil {
		t.Fatal(err)
	}
	want := `clusters:
    - project: my-project
      location: us-central1
      name: auto
      autopilot: true
    - project: my-project
      location: us-central1
      name: netadmin
      version: 1.35.9-gke.100
      network: vpc
      subnetwork: nodes
      privateNodes: true
      autopilot: true
      allowNetAdmin: true
`
	if string(b) != want {
		t.Fatalf("export =\n%s\nwant\n%s", b, want)
	}
}

// seedNode parses src as a seed file's "gke" section.
func seedNode(t *testing.T, src string) *yaml.Node {
	t.Helper()
	var n yaml.Node
	if err := yaml.Unmarshal([]byte(src), &n); err != nil {
		t.Fatal(err)
	}
	return n.Content[0]
}

// TestApplySeedAutopilot checks that an entry with autopilot: true seeds
// an Autopilot cluster, with GKE's own node pool and the NET_ADMIN
// setting Workload restrictions read. Without a container runtime the
// cluster's bring-up fails, which Seeding reports; the cluster stays.
func TestApplySeedAutopilot(t *testing.T) {
	a := newAPI(t)
	err := a.s.ApplySeed(context.Background(), seedNode(t, `
clusters:
  - project: ap-test
    location: us-central1
    name: auto
    workloadIdentity: true
    autopilot: true
    allowNetAdmin: true
`), "")
	if err == nil {
		t.Fatal("ApplySeed succeeded without a container runtime")
	}
	c, gerr := a.GetCluster(context.Background(), &containerpb.GetClusterRequest{Name: apParent + "/clusters/auto"})
	if gerr != nil {
		t.Fatalf("GetCluster: %v (ApplySeed: %v)", gerr, err)
	}
	if !autopilotOn(c) {
		t.Fatalf("cluster is not Autopilot (ApplySeed: %v)", err)
	}
	if !c.GetAutopilot().GetWorkloadPolicyConfig().GetAllowNetAdmin() {
		t.Error("allowNetAdmin not set")
	}
	if c.GetWorkloadIdentityConfig().GetWorkloadPool() != "ap-test.svc.id.goog" {
		t.Errorf("workload pool = %q", c.GetWorkloadIdentityConfig().GetWorkloadPool())
	}
	if np := c.GetNodePools(); len(np) != 1 || np[0].GetName() != defaultPoolName || np[0].GetInitialNodeCount() != 0 {
		t.Errorf("node pools = %v, want GKE's own default-pool", np)
	}
}

// TestApplySeedAutopilotRefused checks the entries Seeding refuses for an
// Autopilot cluster, with the API's messages. Node pools and a workload
// policy on a Standard cluster are refused before any cluster is made.
func TestApplySeedAutopilotRefused(t *testing.T) {
	const first = `
clusters:
  - project: ap-test
    location: us-central1
    name: first
    autopilot: true
`
	for _, tc := range []struct {
		name, entry, want string
		created           bool
	}{
		{"node pools", `
  - project: ap-test
    location: us-central1
    name: bad
    autopilot: true
    nodePools:
      - name: default-pool
`, "clusters[1] (projects/ap-test/locations/us-central1/clusters/bad): Autopilot clusters do not support node pools; GKE manages them.", false},
		{"NET_ADMIN on a Standard cluster", `
  - project: ap-test
    location: us-central1
    name: bad
    allowNetAdmin: true
`, "clusters[1] (projects/ap-test/locations/us-central1/clusters/bad): Workload policies can only be set on Autopilot clusters.", false},
		{"zonal", `
  - project: ap-test
    location: us-central1-a
    name: bad
    autopilot: true
`, "Autopilot clusters must be regional clusters.", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newAPI(t)
			err := a.s.ApplySeed(context.Background(), seedNode(t, first+tc.entry), "")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ApplySeed = %v, want %q", err, tc.want)
			}
			_, gerr := a.GetCluster(context.Background(), &containerpb.GetClusterRequest{Name: apParent + "/clusters/first"})
			if created := gerr == nil; created != tc.created {
				t.Errorf("first cluster created = %v, want %v", created, tc.created)
			}
		})
	}
}

// TestApplySeedAutopilotNetAdmin checks that a seeded Autopilot cluster's
// Workload restrictions follow its allowNetAdmin: a pod that adds
// NET_ADMIN is admitted with it and refused without it.
func TestApplySeedAutopilotNetAdmin(t *testing.T) {
	a := newAPI(t)
	// Without a container runtime the clusters' bring-up fails; they stay.
	_ = a.s.ApplySeed(context.Background(), seedNode(t, `
clusters:
  - project: ap-test
    location: us-central1
    name: netadmin
    autopilot: true
    allowNetAdmin: true
  - project: ap-test
    location: us-central1
    name: plain
    autopilot: true
`), "")
	netAdmin := `{"metadata":{"name":"p"},"spec":{"containers":[{"name":"c","securityContext":{"capabilities":{"add":["NET_ADMIN"]}}}]}}`
	for name, want := range map[string]bool{"netadmin": true, "plain": false} {
		r := admit(t, a.s, clusterKey("ap-test", "us-central1", name), hookRestrict, podReview("dev@example.com", "default", netAdmin))
		if r.Allowed != want {
			t.Errorf("NET_ADMIN pod on %s: allowed %v; want %v (%+v)", name, r.Allowed, want, r.Status)
		}
	}
}
