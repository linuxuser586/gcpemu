package gke

import (
	"context"
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
	// Private nodes come from the same rule as the data plane's: what
	// gcloud create-auto --enable-private-nodes sends, an explicit no, and
	// privateClusterConfig winning over a conflicting network default.
	put("my-project", "us-west1", &containerpb.Cluster{
		Name: "auto", CurrentMasterVersion: defaultVersion, Network: "default", Subnetwork: "default",
		NetworkConfig: &containerpb.NetworkConfig{DefaultEnablePrivateNodes: proto.Bool(true)},
		NodePools:     []*containerpb.NodePool{pool("default-pool", 1, &containerpb.NodeConfig{})},
	})
	put("my-project", "us-west1", &containerpb.Cluster{
		Name: "conflict", CurrentMasterVersion: defaultVersion, Network: "default", Subnetwork: "default",
		PrivateClusterConfig: &containerpb.PrivateClusterConfig{EnablePrivateNodes: true},
		NetworkConfig:        &containerpb.NetworkConfig{DefaultEnablePrivateNodes: proto.Bool(false)},
		NodePools:            []*containerpb.NodePool{pool("default-pool", 1, &containerpb.NodeConfig{})},
	})
	put("my-project", "us-west1", &containerpb.Cluster{
		Name: "public", CurrentMasterVersion: defaultVersion, Network: "default", Subnetwork: "default",
		NetworkConfig: &containerpb.NetworkConfig{DefaultEnablePrivateNodes: proto.Bool(false)},
		NodePools:     []*containerpb.NodePool{pool("default-pool", 1, &containerpb.NodeConfig{})},
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
    - project: my-project
      location: us-west1
      name: auto
      privateNodes: true
    - project: my-project
      location: us-west1
      name: conflict
      privateNodes: true
    - project: my-project
      location: us-west1
      name: public
`
	if string(b) != want {
		t.Fatalf("export =\n%s\nwant\n%s", b, want)
	}
	// The section decodes as the seed reads it.
	var f seedFile
	if err := yaml.Unmarshal(b, &f); err != nil || len(f.Clusters) != 6 || f.Clusters[2].NodePools[1].NodeCount != 3 {
		t.Fatalf("decode = %+v, %v", f, err)
	}
}

// TestSeedClusterPrivateNodes checks without a container runtime that the
// cluster ApplySeed creates from privateNodes: true is private by the same
// rule export reads, so the round trip keeps the nodes private.
func TestSeedClusterPrivateNodes(t *testing.T) {
	sc := seedCluster{Project: "my-project", Location: "us-central1-a", Name: "dev", PrivateNodes: true}
	if c := sc.cluster(); !clusterPrivateNodes(c) || !privateNodes(c, c.NodePools[0]) {
		t.Fatalf("privateNodes: true seeds %v", c)
	}
	sc.PrivateNodes = false
	if c := sc.cluster(); clusterPrivateNodes(c) {
		t.Fatalf("privateNodes: false seeds %v", c)
	}
}
