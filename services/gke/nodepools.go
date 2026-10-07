package gke

import (
	"context"

	"cloud.google.com/go/container/apiv1/containerpb"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// Node pools (FR-GKE-003). Node pool reads need container.clusters.get and
// mutations container.clusters.update, as on GKE.

func (a *api) ListNodePools(ctx context.Context, req *containerpb.ListNodePoolsRequest) (*containerpb.ListNodePoolsResponse, error) {
	r, err := parseRef(req.Parent, req.ProjectId, req.Zone, req.ClusterId, "", 1)
	if err != nil {
		return nil, err
	}
	if err := a.check(ctx, r, "container.clusters.get", r.clusterResource()); err != nil {
		return nil, err
	}
	rec, err := a.load(r)
	if err != nil {
		return nil, err
	}
	return &containerpb.ListNodePoolsResponse{NodePools: rec.cluster().NodePools}, nil
}

func (a *api) GetNodePool(ctx context.Context, req *containerpb.GetNodePoolRequest) (*containerpb.NodePool, error) {
	r, err := parseRef(req.Name, req.ProjectId, req.Zone, req.ClusterId, req.NodePoolId, 2)
	if err != nil {
		return nil, err
	}
	if err := a.check(ctx, r, "container.clusters.get", r.clusterResource()); err != nil {
		return nil, err
	}
	rec, err := a.load(r)
	if err != nil {
		return nil, err
	}
	np := findPool(rec.cluster(), r.Pool)
	if np == nil {
		return nil, notFoundPool(r)
	}
	return np, nil
}

func (a *api) CreateNodePool(ctx context.Context, req *containerpb.CreateNodePoolRequest) (*containerpb.Operation, error) {
	r, err := parseRef(req.Parent, req.ProjectId, req.Zone, req.ClusterId, "", 1)
	if err != nil {
		return nil, err
	}
	if req.NodePool == nil {
		return nil, apierr.InvalidArgument("Required field 'node_pool' is missing.")
	}
	np := cloneCluster(&containerpb.Cluster{NodePools: []*containerpb.NodePool{req.NodePool}}).NodePools[0]
	r.Pool = np.Name
	others := a.s.nodesElsewhere(r.key())
	return a.mutate(ctx, r, containerpb.Operation_CREATE_NODE_POOL, "", func(rec *clusterRecord, c *containerpb.Cluster) error {
		if findPool(c, np.Name) != nil {
			return apierr.AlreadyExists("Already exists: %s.", r.poolName())
		}
		if err := a.s.fillPool(r, c, np, ""); err != nil {
			return err
		}
		nodes := a.s.planNodes(c, np, nil)
		if err := a.s.limitFor(others, len(rec.Int.Nodes)+len(nodes)); err != nil {
			return err
		}
		rec.Int.Nodes = append(rec.Int.Nodes, nodes...)
		c.NodePools = append(c.NodePools, np)
		refreshCounts(rec, c)
		return nil
	}, func(ctx context.Context) error {
		return a.s.reconcilePool(ctx, r.key(), np.Name)
	})
}

func (a *api) DeleteNodePool(ctx context.Context, req *containerpb.DeleteNodePoolRequest) (*containerpb.Operation, error) {
	r, err := parseRef(req.Name, req.ProjectId, req.Zone, req.ClusterId, req.NodePoolId, 2)
	if err != nil {
		return nil, err
	}
	var gone []nodeRecord
	return a.mutate(ctx, r, containerpb.Operation_DELETE_NODE_POOL, "", func(rec *clusterRecord, c *containerpb.Cluster) error {
		np := findPool(c, r.Pool)
		if np == nil {
			return notFoundPool(r)
		}
		np.Status = containerpb.NodePool_STOPPING
		gone = rec.nodesOf(r.Pool)
		return nil
	}, func(ctx context.Context) error {
		if err := a.s.removeNodes(ctx, r.key(), gone); err != nil {
			return err
		}
		return a.s.updateCluster(r.key(), func(rec *clusterRecord, c *containerpb.Cluster) error {
			var keep []*containerpb.NodePool
			for _, p := range c.NodePools {
				if p.Name != r.Pool {
					keep = append(keep, p)
				}
			}
			c.NodePools = keep
			var nodes []nodeRecord
			for _, n := range rec.Int.Nodes {
				if n.Pool != r.Pool {
					nodes = append(nodes, n)
				}
			}
			rec.Int.Nodes = nodes
			refreshCounts(rec, c)
			return nil
		})
	})
}

func (a *api) SetNodePoolSize(ctx context.Context, req *containerpb.SetNodePoolSizeRequest) (*containerpb.Operation, error) {
	r, err := parseRef(req.Name, req.ProjectId, req.Zone, req.ClusterId, req.NodePoolId, 2)
	if err != nil {
		return nil, err
	}
	if req.NodeCount < 0 {
		return nil, apierr.InvalidArgument("node_count must be non-negative.")
	}
	var gone []nodeRecord
	others := a.s.nodesElsewhere(r.key())
	return a.mutate(ctx, r, containerpb.Operation_SET_NODE_POOL_SIZE, "", func(rec *clusterRecord, c *containerpb.Cluster) error {
		np := findPool(c, r.Pool)
		if np == nil {
			return notFoundPool(r)
		}
		// GKE reports the pool size through its instance groups; the
		// emulator has none, so initialNodeCount tracks the current
		// per-zone size.
		np.InitialNodeCount = req.NodeCount
		cur := rec.nodesOf(r.Pool)
		next := a.s.planNodes(c, np, cur)
		keep := map[string]bool{}
		for _, n := range next {
			keep[n.Name] = true
		}
		gone = nil
		for _, n := range cur {
			if !keep[n.Name] {
				gone = append(gone, n)
			}
		}
		var nodes []nodeRecord
		for _, n := range rec.Int.Nodes {
			if n.Pool != r.Pool {
				nodes = append(nodes, n)
			}
		}
		nodes = append(nodes, next...)
		if err := a.s.limitFor(others, len(nodes)); err != nil {
			return err
		}
		// Removed nodes stay recorded until their containers are gone.
		rec.Int.Nodes = append(nodes, gone...)
		refreshCounts(rec, c)
		return nil
	}, func(ctx context.Context) error {
		if err := a.s.removeNodes(ctx, r.key(), gone); err != nil {
			return err
		}
		if err := a.s.dropNodes(r.key(), gone); err != nil {
			return err
		}
		return a.s.reconcilePool(ctx, r.key(), r.Pool)
	})
}

func (a *api) UpdateNodePool(ctx context.Context, req *containerpb.UpdateNodePoolRequest) (*containerpb.Operation, error) {
	r, err := parseRef(req.Name, req.ProjectId, req.Zone, req.ClusterId, req.NodePoolId, 2)
	if err != nil {
		return nil, err
	}
	if req.NodeVersion != "" {
		return a.upgradePools(ctx, r, r.Pool, req.NodeVersion)
	}
	var relabel bool
	return a.mutate(ctx, r, containerpb.Operation_UPDATE_CLUSTER, req.Etag, func(rec *clusterRecord, c *containerpb.Cluster) error {
		np := findPool(c, r.Pool)
		if np == nil {
			return notFoundPool(r)
		}
		if req.Etag != "" && req.Etag != np.Etag {
			return apierr.Aborted("Node pool etag %q does not match the current etag.", req.Etag)
		}
		cfg := np.Config
		if req.Labels != nil {
			cfg.Labels = req.Labels.Labels
			relabel = true
		}
		if req.Taints != nil {
			for _, t := range req.Taints.Taints {
				if t.Key == "" || t.Effect == containerpb.NodeTaint_EFFECT_UNSPECIFIED {
					return apierr.InvalidArgument("Taints need a key and an effect.")
				}
			}
			cfg.Taints = req.Taints.Taints
			relabel = true
		}
		if req.ResourceLabels != nil {
			cfg.ResourceLabels = req.ResourceLabels.Labels
		}
		if req.WorkloadMetadataConfig != nil {
			cfg.WorkloadMetadataConfig = req.WorkloadMetadataConfig
		}
		if req.Tags != nil {
			cfg.Tags = req.Tags.Tags
		}
		if req.MachineType != "" {
			cfg.MachineType = req.MachineType
		}
		if req.DiskType != "" {
			cfg.DiskType = req.DiskType
		}
		if req.DiskSizeGb != 0 {
			cfg.DiskSizeGb = int32(req.DiskSizeGb)
		}
		if req.ImageType != "" {
			cfg.ImageType = req.ImageType
		}
		if req.KubeletConfig != nil {
			cfg.KubeletConfig = req.KubeletConfig
		}
		if req.LinuxNodeConfig != nil {
			cfg.LinuxNodeConfig = req.LinuxNodeConfig
		}
		if req.UpgradeSettings != nil {
			np.UpgradeSettings = req.UpgradeSettings
		}
		if len(req.Locations) > 0 {
			return apierr.Unimplemented("gcpemu does not support changing node pool locations; create a new node pool.")
		}
		np.Etag = a.s.env.IDs.Hex(8)
		return nil
	}, func(ctx context.Context) error {
		if !relabel {
			return nil
		}
		return a.s.syncPoolLabels(ctx, r.key(), r.Pool)
	})
}

func (a *api) SetNodePoolAutoscaling(ctx context.Context, req *containerpb.SetNodePoolAutoscalingRequest) (*containerpb.Operation, error) {
	r, err := parseRef(req.Name, req.ProjectId, req.Zone, req.ClusterId, req.NodePoolId, 2)
	if err != nil {
		return nil, err
	}
	return a.mutate(ctx, r, containerpb.Operation_UPDATE_CLUSTER, "", func(rec *clusterRecord, c *containerpb.Cluster) error {
		np := findPool(c, r.Pool)
		if np == nil {
			return notFoundPool(r)
		}
		np.Autoscaling = req.Autoscaling
		return nil
	}, nil)
}

func (a *api) SetNodePoolManagement(ctx context.Context, req *containerpb.SetNodePoolManagementRequest) (*containerpb.Operation, error) {
	r, err := parseRef(req.Name, req.ProjectId, req.Zone, req.ClusterId, req.NodePoolId, 2)
	if err != nil {
		return nil, err
	}
	return a.mutate(ctx, r, containerpb.Operation_SET_NODE_POOL_MANAGEMENT, "", func(rec *clusterRecord, c *containerpb.Cluster) error {
		np := findPool(c, r.Pool)
		if np == nil {
			return notFoundPool(r)
		}
		np.Management = req.Management
		return nil
	}, nil)
}

func (a *api) CompleteNodePoolUpgrade(ctx context.Context, req *containerpb.CompleteNodePoolUpgradeRequest) (*emptypb.Empty, error) {
	return nil, apierr.Unimplemented("Blue-green node pool upgrades are not supported by gcpemu.")
}

// limitFor checks the node limit for a cluster that will run want nodes;
// others is the number of nodes other clusters run (nodesElsewhere).
func (s *Service) limitFor(others, want int) error {
	if limit := maxNodes(); others+want > limit {
		return apierr.New(8, "gcpemu node limit exceeded: this request needs %d node(s) and %d of %d are in use (set %s to raise the limit).",
			want, others, limit, maxNodesEnv).WithReason("container.googleapis.com", "QUOTA_EXCEEDED")
	}
	return nil
}

// nodesElsewhere counts the nodes of every cluster but key. It must be
// called outside a store transaction.
func (s *Service) nodesElsewhere(key string) int {
	n := 0
	_ = s.env.Store.View(func(tx store.Tx) error {
		for _, rec := range listClusters(tx, "") {
			if rec.key() != key {
				n += len(rec.Int.Nodes)
			}
		}
		return nil
	})
	return n
}

// refreshCounts updates the cluster's node counts and instance groups.
func refreshCounts(rec *clusterRecord, c *containerpb.Cluster) {
	n := 0
	c.InstanceGroupUrls = nil
	for _, np := range c.NodePools {
		n += poolNodeCount(np)
		c.InstanceGroupUrls = append(c.InstanceGroupUrls, np.InstanceGroupUrls...)
	}
	c.CurrentNodeCount = int32(n)
}

// dropNodes removes node records from the stored cluster.
func (s *Service) dropNodes(key string, gone []nodeRecord) error {
	if len(gone) == 0 {
		return nil
	}
	drop := map[string]bool{}
	for _, n := range gone {
		drop[n.Name] = true
	}
	return s.updateCluster(key, func(rec *clusterRecord, c *containerpb.Cluster) error {
		var nodes []nodeRecord
		for _, n := range rec.Int.Nodes {
			if !drop[n.Name] {
				nodes = append(nodes, n)
			}
		}
		rec.Int.Nodes = nodes
		return nil
	})
}
