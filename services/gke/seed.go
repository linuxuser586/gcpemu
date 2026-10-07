package gke

import (
	"context"
	"fmt"
	"time"

	"cloud.google.com/go/container/apiv1/containerpb"
	"gopkg.in/yaml.v3"

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
//
// Existing clusters are left untouched. Seeding waits until new clusters
// are RUNNING (or fails with their error).
type seedFile struct {
	Clusters []seedCluster `yaml:"clusters"`
}

type seedCluster struct {
	Project          string     `yaml:"project"`
	Location         string     `yaml:"location"`
	Name             string     `yaml:"name"`
	Version          string     `yaml:"version"`
	Network          string     `yaml:"network"`
	Subnetwork       string     `yaml:"subnetwork"`
	PrivateNodes     bool       `yaml:"privateNodes"`
	WorkloadIdentity bool       `yaml:"workloadIdentity"`
	NodePools        []seedPool `yaml:"nodePools"`
}

type seedPool struct {
	Name           string            `yaml:"name"`
	NodeCount      int32             `yaml:"nodeCount"`
	MachineType    string            `yaml:"machineType"`
	Labels         map[string]string `yaml:"labels"`
	ServiceAccount string            `yaml:"serviceAccount"`
}

// ApplySeed creates the seeded clusters that do not exist yet.
func (s *Service) ApplySeed(ctx context.Context, section *yaml.Node, baseDir string) error {
	var f seedFile
	if err := section.Decode(&f); err != nil {
		return err
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
		pools := sc.NodePools
		if len(pools) == 0 {
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
