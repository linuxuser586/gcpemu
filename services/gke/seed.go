package gke

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"time"

	"cloud.google.com/go/container/apiv1/containerpb"
	"google.golang.org/protobuf/proto"
	"gopkg.in/yaml.v3"

	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// seedFile is the "gke" section of a seed file (FR-CORE-011):
//
//	gke:
//	  clusters:
//	    - project: my-project
//	      location: us-central1-a
//	      name: dev
//	      version: "1.36"            # optional; default channel version
//	      network: default            # optional
//	      subnetwork: default         # optional
//	      privateNodes: false
//	      workloadIdentity: true      # workloadPool = PROJECT.svc.id.goog
//	      nodePools:                  # default: one default-pool of 1 node
//	        - name: default-pool
//	          nodeCount: 1
//	          machineType: e2-standard-4
//	          labels: {team: web}
//	          serviceAccount: nodes@my-project.iam.gserviceaccount.com
//	    - project: my-project
//	      location: us-central1       # Autopilot clusters are regional
//	      name: auto
//	      autopilot: true             # default: a Standard cluster
//	      allowNetAdmin: true         # Autopilot only
//
// An Autopilot cluster (FR-GKE-012) takes no nodePools, as GKE owns its
// node pools, and always has Workload Identity, so workloadIdentity
// changes nothing on one. Existing clusters are left untouched.
// Seeding waits until new clusters are RUNNING (or fails with their error).
type seedFile struct {
	Clusters []seedCluster `yaml:"clusters"`
}

type seedCluster struct {
	Project          string     `yaml:"project"`
	Location         string     `yaml:"location"`
	Name             string     `yaml:"name"`
	Version          string     `yaml:"version,omitempty"`
	Network          string     `yaml:"network,omitempty"`
	Subnetwork       string     `yaml:"subnetwork,omitempty"`
	PrivateNodes     bool       `yaml:"privateNodes,omitempty"`
	WorkloadIdentity bool       `yaml:"workloadIdentity,omitempty"`
	Autopilot        bool       `yaml:"autopilot,omitempty"`
	AllowNetAdmin    bool       `yaml:"allowNetAdmin,omitempty"`
	NodePools        []seedPool `yaml:"nodePools,omitempty"`
}

type seedPool struct {
	Name           string            `yaml:"name"`
	NodeCount      int32             `yaml:"nodeCount,omitempty"`
	MachineType    string            `yaml:"machineType,omitempty"`
	Labels         map[string]string `yaml:"labels,omitempty"`
	ServiceAccount string            `yaml:"serviceAccount,omitempty"`
}

var _ emu.SeedExporter = (*Service)(nil)

// ExportSeed writes the clusters back as the "gke" section (FR-CORE-013),
// ordered by project, location and name, with the fields that differ from
// what the seed defaults to: the current master version, network, private
// nodes, Workload Identity and each node pool's size, machine type, labels
// and service account. An Autopilot cluster exports its Cluster mode and
// NET_ADMIN setting in place of Workload Identity and node pools, which
// GKE sets. Kubernetes objects are data plane and are not exported, nor
// is anything else the seed file cannot declare (release channel, node
// pool versions, autoscaling, taints...). A pool resized to
// zero nodes comes back with one, as the seed creates every pool with at
// least one node, and a cluster without node pools gets the default pool.
func (s *Service) ExportSeed(ctx context.Context) (any, error) {
	var recs []*clusterRecord
	_ = s.env.Store.View(func(tx store.Tx) error { recs = listClusters(tx, ""); return nil })
	if len(recs) == 0 {
		return nil, nil
	}
	sort.Slice(recs, func(i, j int) bool {
		a, b := recs[i].Int, recs[j].Int
		if a.Project != b.Project {
			return a.Project < b.Project
		}
		if a.Location != b.Location {
			return a.Location < b.Location
		}
		return a.Name < b.Name
	})
	var f seedFile
	for _, rec := range recs {
		c := rec.cluster()
		sc := seedCluster{
			Project: rec.Int.Project, Location: rec.Int.Location, Name: rec.Int.Name,
			PrivateNodes: c.GetPrivateClusterConfig().GetEnablePrivateNodes(),
		}
		if v := c.GetCurrentMasterVersion(); v != defaultVersion {
			sc.Version = v
		}
		if c.GetNetwork() != "default" {
			sc.Network = c.GetNetwork()
		}
		if c.GetSubnetwork() != "default" {
			sc.Subnetwork = c.GetSubnetwork()
		}
		// GKE sets an Autopilot cluster's Workload Identity and owns its
		// node pools (FR-GKE-012).
		if autopilotOn(c) {
			sc.Autopilot = true
			sc.AllowNetAdmin = c.GetAutopilot().GetWorkloadPolicyConfig().GetAllowNetAdmin()
			f.Clusters = append(f.Clusters, sc)
			continue
		}
		sc.WorkloadIdentity = c.GetWorkloadIdentityConfig().GetWorkloadPool() != ""
		for _, np := range c.GetNodePools() {
			sp := seedPool{Name: np.GetName(), Labels: np.GetConfig().GetLabels()}
			if n := np.GetInitialNodeCount(); n > 1 {
				sp.NodeCount = n
			}
			if mt := np.GetConfig().GetMachineType(); mt != defaultMachineType {
				sp.MachineType = mt
			}
			if sa := np.GetConfig().GetServiceAccount(); sa != "default" {
				sp.ServiceAccount = sa
			}
			sc.NodePools = append(sc.NodePools, sp)
		}
		// The seed's default: one default-pool of one node.
		if len(sc.NodePools) == 1 && reflect.DeepEqual(sc.NodePools[0], seedPool{Name: defaultPoolName}) {
			sc.NodePools = nil
		}
		f.Clusters = append(f.Clusters, sc)
	}
	return f, nil
}

// ApplySeed creates the seeded clusters that do not exist yet.
func (s *Service) ApplySeed(ctx context.Context, section *yaml.Node, baseDir string) error {
	var f seedFile
	if err := section.Decode(&f); err != nil {
		return err
	}
	// Refuse, before creating anything, the Autopilot entries the API
	// would let through (FR-GKE-012): it takes the one default-pool gcloud
	// sends with an Autopilot cluster, and a workload policy on a cluster
	// that is not one.
	for i, sc := range f.Clusters {
		r := ref{Project: sc.Project, Location: sc.Location, Cluster: sc.Name}
		switch {
		case sc.Autopilot && len(sc.NodePools) > 0:
			return fmt.Errorf("clusters[%d] (%s): Autopilot clusters do not support node pools; GKE manages them.", i, r.clusterName())
		case sc.AllowNetAdmin && !sc.Autopilot:
			return fmt.Errorf("clusters[%d] (%s): Workload policies can only be set on Autopilot clusters.", i, r.clusterName())
		}
	}
	type pending struct{ project, op, name string }
	var ops []pending
	for i, sc := range f.Clusters {
		r := ref{Project: sc.Project, Location: sc.Location, Cluster: sc.Name}
		var exists bool
		_ = s.env.Store.View(func(tx store.Tx) error { exists = store.Exists(tx, nsClusters, r.key()); return nil })
		if exists {
			continue
		}
		c := &containerpb.Cluster{
			Name: sc.Name, InitialClusterVersion: sc.Version,
			Network: sc.Network, Subnetwork: sc.Subnetwork,
		}
		if sc.PrivateNodes {
			c.PrivateClusterConfig = &containerpb.PrivateClusterConfig{EnablePrivateNodes: true}
		}
		if sc.WorkloadIdentity {
			c.WorkloadIdentityConfig = &containerpb.WorkloadIdentityConfig{WorkloadPool: sc.Project + ".svc.id.goog"}
		}
		if sc.Autopilot {
			c.Autopilot = &containerpb.Autopilot{Enabled: true}
			if sc.AllowNetAdmin {
				c.Autopilot.WorkloadPolicyConfig = &containerpb.WorkloadPolicyConfig{AllowNetAdmin: proto.Bool(true)}
			}
		}
		pools := sc.NodePools
		if len(pools) == 0 && !sc.Autopilot {
			pools = []seedPool{{Name: defaultPoolName, NodeCount: 1}}
		}
		for _, p := range pools {
			if p.Name == "" {
				p.Name = defaultPoolName
			}
			c.NodePools = append(c.NodePools, &containerpb.NodePool{
				Name: p.Name, InitialNodeCount: max(p.NodeCount, 1),
				Config: &containerpb.NodeConfig{MachineType: p.MachineType, Labels: p.Labels, ServiceAccount: p.ServiceAccount},
			})
		}
		op, err := s.api.CreateCluster(ctx, &containerpb.CreateClusterRequest{Parent: r.parent(), Cluster: c})
		if err != nil {
			return fmt.Errorf("clusters[%d] (%s): %w", i, r.clusterName(), err)
		}
		ops = append(ops, pending{sc.Project, op.Name, r.clusterName()})
	}
	for _, p := range ops {
		for {
			op, err := s.getOp(p.project, p.op)
			if err != nil {
				return err
			}
			if op.Status == containerpb.Operation_DONE {
				if op.Error != nil {
					return fmt.Errorf("%s: %s", p.name, op.Error.Message)
				}
				break
			}
			select {
			case <-ctx.Done():
				return fmt.Errorf("%s: %w", p.name, ctx.Err())
			case <-time.After(500 * time.Millisecond):
			}
		}
	}
	return nil
}
