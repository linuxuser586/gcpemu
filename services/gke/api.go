package gke

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sort"
	"strings"

	"cloud.google.com/go/container/apiv1/containerpb"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// api implements containerpb.ClusterManagerServer (FR-GKE-001..003).
type api struct {
	containerpb.UnimplementedClusterManagerServer
	s *Service
}

// Default node config values (GKE's defaults).
const (
	defaultMachineType = "e2-medium"
	defaultDiskSizeGB  = 100
	defaultDiskType    = "pd-balanced"
	defaultImageType   = "COS_CONTAINERD"
	defaultPoolName    = "default-pool"
	defaultNodeCount   = 3
)

var defaultScopes = []string{
	"https://www.googleapis.com/auth/devstorage.read_only",
	"https://www.googleapis.com/auth/logging.write",
	"https://www.googleapis.com/auth/monitoring",
	"https://www.googleapis.com/auth/servicecontrol",
	"https://www.googleapis.com/auth/service.management.readonly",
	"https://www.googleapis.com/auth/trace.append",
}

// check runs the IAM check and project validation for a request.
func (a *api) check(ctx context.Context, r ref, perm, resource string) error {
	if err := a.s.env.EnsureProject(r.Project); err != nil {
		return err
	}
	if r.Location != "-" {
		if err := validateLocation(r.Location); err != nil {
			return err
		}
	}
	return a.s.env.Auth.Check(ctx, perm, resource)
}

// load returns the stored cluster record for r.
func (a *api) load(r ref) (*clusterRecord, error) {
	var rec *clusterRecord
	err := a.s.env.Store.View(func(tx store.Tx) error {
		var err error
		rec, err = getCluster(tx, r.key())
		return err
	})
	if errors.Is(err, store.ErrNotFound) {
		return nil, notFoundCluster(r)
	}
	return rec, err
}

// ---- clusters ----

func (a *api) ListClusters(ctx context.Context, req *containerpb.ListClustersRequest) (*containerpb.ListClustersResponse, error) {
	r, err := parseRef(req.Parent, req.ProjectId, req.Zone, "", "", 0)
	if err != nil {
		return nil, err
	}
	if err := a.check(ctx, r, "container.clusters.list", projectResource(r.Project)); err != nil {
		return nil, err
	}
	prefix := r.Project + "/"
	if r.Location != "-" {
		prefix += r.Location + "/"
	}
	resp := &containerpb.ListClustersResponse{}
	_ = a.s.env.Store.View(func(tx store.Tx) error {
		for _, rec := range listClusters(tx, prefix) {
			resp.Clusters = append(resp.Clusters, rec.cluster())
		}
		return nil
	})
	return resp, nil
}

func (a *api) GetCluster(ctx context.Context, req *containerpb.GetClusterRequest) (*containerpb.Cluster, error) {
	r, err := parseRef(req.Name, req.ProjectId, req.Zone, req.ClusterId, "", 1)
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
	return rec.cluster(), nil
}

func (a *api) CreateCluster(ctx context.Context, req *containerpb.CreateClusterRequest) (*containerpb.Operation, error) {
	r, err := parseRef(req.Parent, req.ProjectId, req.Zone, "", "", 0)
	if err != nil {
		return nil, err
	}
	if err := a.check(ctx, r, "container.clusters.create", projectResource(r.Project)); err != nil {
		return nil, err
	}
	if r.Location == "-" {
		return nil, apierr.InvalidArgument("Location \"-\" is not valid for cluster creation.")
	}
	c := req.Cluster
	if c == nil {
		return nil, apierr.InvalidArgument("Required field 'cluster' is missing.")
	}
	c = cloneCluster(c)
	if err := validateName("cluster", c.Name); err != nil {
		return nil, err
	}
	r.Cluster = c.Name
	if c.GetAutopilot().GetEnabled() {
		return nil, apierr.Unimplemented("Autopilot clusters are not supported by gcpemu (FR-GKE-012); create a Standard cluster.")
	}
	rec, err := a.s.newCluster(r, c)
	if err != nil {
		return nil, err
	}
	if err := a.s.checkAddons(rec.cluster()); err != nil {
		return nil, err
	}
	err = a.s.env.Store.Update(func(tx store.Tx) error {
		if store.Exists(tx, nsClusters, r.key()) {
			return apierr.AlreadyExists("Already exists: %s.", r.clusterName())
		}
		if err := a.s.checkNodeLimit(tx, "", len(rec.Int.Nodes)); err != nil {
			return err
		}
		return putCluster(tx, rec)
	})
	if err != nil {
		return nil, err
	}
	a.s.index(r.key(), rec.cluster().Id)
	op, err := a.s.startOp(r, containerpb.Operation_CREATE_CLUSTER, selfLink(r.Project, r.Location, "clusters/"+r.Cluster), true,
		func(ctx context.Context) error { return a.s.provision(ctx, r.key()) })
	if err != nil {
		_ = a.s.env.Store.Update(func(tx store.Tx) error { return tx.Delete(nsClusters, r.key()) })
		return nil, err
	}
	return op, nil
}

// newCluster validates a create request and fills in GKE's defaults.
func (s *Service) newCluster(r ref, c *containerpb.Cluster) (*clusterRecord, error) {
	if c.InitialNodeCount != 0 && len(c.NodePools) > 0 {
		return nil, apierr.InvalidArgument("Cluster.initial_node_count and Cluster.node_pools cannot both be specified.")
	}
	if c.GetMasterAuth().GetUsername() != "" || c.GetMasterAuth().GetPassword() != "" {
		return nil, apierr.InvalidArgument("Basic authentication was removed for GKE cluster versions >= 1.19.")
	}
	if wp := c.GetWorkloadIdentityConfig().GetWorkloadPool(); wp != "" && wp != r.Project+".svc.id.goog" {
		return nil, apierr.InvalidArgument("Workload pool %q is invalid; it must be %q.", wp, r.Project+".svc.id.goog")
	}
	ch := c.GetReleaseChannel().GetChannel()
	if c.ReleaseChannel == nil {
		c.ReleaseChannel = &containerpb.ReleaseChannel{Channel: containerpb.ReleaseChannel_REGULAR}
		ch = containerpb.ReleaseChannel_REGULAR
	}
	ver, err := resolveVersion(c.InitialClusterVersion, ch)
	if err != nil {
		return nil, err
	}
	now := s.now()
	region := regionOf(r.Location)
	if isZone(r.Location) {
		locs := []string{r.Location}
		for _, l := range c.Locations {
			if l != r.Location {
				locs = append(locs, l)
			}
		}
		c.Locations = locs
	} else if len(c.Locations) == 0 {
		c.Locations = defaultZones(region)
	}
	for _, l := range c.Locations {
		if !isZone(l) || regionOf(l) != region {
			return nil, apierr.InvalidArgument("Location %q is not a zone of region %q.", l, region)
		}
	}
	vpc := s.vpc()
	sub, err := vpc.ResolveSubnetwork(context.Background(), r.Project, region, networkOf(c), subnetworkOf(c))
	if err != nil {
		return nil, err
	}
	network := networkFromSubnet(r.Project, c, sub)

	c.Location = r.Location
	c.Zone = r.Location
	c.Id = s.env.IDs.Hex(16)
	c.SelfLink = selfLink(r.Project, r.Location, "clusters/"+c.Name)
	c.CreateTime = now
	c.Status = containerpb.Cluster_PROVISIONING
	c.InitialClusterVersion = ver
	c.CurrentMasterVersion = ver
	c.CurrentNodeVersion = ver
	if c.LoggingService == "" {
		c.LoggingService = "logging.googleapis.com/kubernetes"
	}
	if c.MonitoringService == "" {
		c.MonitoringService = "monitoring.googleapis.com/kubernetes"
	}
	c.Network = lastSeg(network)
	c.Subnetwork = lastSeg(sub)
	if c.NetworkConfig == nil {
		c.NetworkConfig = &containerpb.NetworkConfig{}
	}
	c.NetworkConfig.Network = network
	c.NetworkConfig.Subnetwork = sub
	c.NodeIpv4CidrSize = 24
	if c.IpAllocationPolicy == nil {
		c.IpAllocationPolicy = &containerpb.IPAllocationPolicy{UseIpAliases: true}
	}
	c.IpAllocationPolicy.StackType = containerpb.StackType_IPV4
	c.LabelFingerprint = labelFingerprint(c.ResourceLabels)
	if c.DefaultMaxPodsConstraint == nil {
		c.DefaultMaxPodsConstraint = &containerpb.MaxPodsConstraint{MaxPodsPerNode: 110}
	}
	if c.MasterAuth == nil {
		c.MasterAuth = &containerpb.MasterAuth{}
	}
	if c.MasterAuth.ClientCertificateConfig == nil {
		c.MasterAuth.ClientCertificateConfig = &containerpb.ClientCertificateConfig{}
	}
	if c.DatabaseEncryption == nil {
		c.DatabaseEncryption = &containerpb.DatabaseEncryption{State: containerpb.DatabaseEncryption_DECRYPTED}
	}

	if len(c.NodePools) == 0 {
		n := c.InitialNodeCount
		if n == 0 && c.NodeConfig == nil {
			n = defaultNodeCount
		}
		cfg := c.NodeConfig
		if cfg == nil {
			cfg = &containerpb.NodeConfig{}
		}
		c.NodePools = []*containerpb.NodePool{{Name: defaultPoolName, InitialNodeCount: n, Config: proto.Clone(cfg).(*containerpb.NodeConfig)}}
	}
	rec := &clusterRecord{Int: clusterInternal{
		Project: r.Project, Location: r.Location, Name: c.Name,
		Secret: s.env.IDs.Hex(16), Token: s.env.IDs.Hex(24), Subnetwork: sub,
	}}
	seen := map[string]bool{}
	for _, np := range c.NodePools {
		if seen[np.Name] {
			return nil, apierr.InvalidArgument("Node pool %q is specified more than once.", np.Name)
		}
		seen[np.Name] = true
		if err := s.fillPool(r, c, np, ""); err != nil {
			return nil, err
		}
		rec.Int.Nodes = append(rec.Int.Nodes, s.planNodes(c, np, nil, startCounts(np))...)
	}
	c.NodeConfig = proto.Clone(c.NodePools[0].Config).(*containerpb.NodeConfig)
	// initialNodeCount keeps the requested value, like GKE (the OpenTofu
	// provider would otherwise plan a replacement of clusters created with
	// remove_default_node_pool).
	c.CurrentNodeCount = int32(len(rec.Int.Nodes))
	c.InstanceGroupUrls = nil
	for _, np := range c.NodePools {
		c.InstanceGroupUrls = append(c.InstanceGroupUrls, np.InstanceGroupUrls...)
	}
	c.Etag = s.env.IDs.Hex(8)
	rec.setCluster(c)
	return rec, nil
}

// fillPool validates a node pool and fills in defaults. ver is the
// requested node version ("" = the cluster's master version).
func (s *Service) fillPool(r ref, c *containerpb.Cluster, np *containerpb.NodePool, ver string) error {
	if err := validateName("nodePool", np.Name); err != nil {
		return err
	}
	if np.InitialNodeCount < 0 {
		return apierr.InvalidArgument("Node pool %q: initial_node_count must be non-negative.", np.Name)
	}
	if np.Config == nil {
		np.Config = &containerpb.NodeConfig{}
	}
	cfg := np.Config
	if cfg.MachineType == "" {
		cfg.MachineType = defaultMachineType
	}
	if cfg.DiskSizeGb == 0 {
		cfg.DiskSizeGb = defaultDiskSizeGB
	}
	if cfg.DiskType == "" {
		cfg.DiskType = defaultDiskType
	}
	if cfg.ImageType == "" {
		cfg.ImageType = defaultImageType
	}
	if cfg.ServiceAccount == "" {
		cfg.ServiceAccount = "default"
	}
	if len(cfg.OauthScopes) == 0 {
		cfg.OauthScopes = append([]string(nil), defaultScopes...)
	}
	if cfg.Metadata == nil {
		cfg.Metadata = map[string]string{"disable-legacy-endpoints": "true"}
	}
	if cfg.WorkloadMetadataConfig == nil && c.GetWorkloadIdentityConfig().GetWorkloadPool() != "" {
		cfg.WorkloadMetadataConfig = &containerpb.WorkloadMetadataConfig{Mode: containerpb.WorkloadMetadataConfig_GKE_METADATA}
	}
	for _, t := range cfg.Taints {
		if t.Key == "" || t.Effect == containerpb.NodeTaint_EFFECT_UNSPECIFIED {
			return apierr.InvalidArgument("Node pool %q: taints need a key and an effect.", np.Name)
		}
	}
	if len(np.Locations) == 0 {
		np.Locations = append([]string(nil), c.Locations...)
	}
	for _, l := range np.Locations {
		if !isZone(l) || regionOf(l) != regionOf(c.Location) {
			return apierr.InvalidArgument("Node pool %q: location %q is not a zone of the cluster's region.", np.Name, l)
		}
	}
	if ver == "" {
		ver = np.Version
	}
	if ver == "" || ver == "-" {
		ver = c.CurrentMasterVersion
	} else {
		v, err := resolveVersion(ver, c.GetReleaseChannel().GetChannel())
		if err != nil {
			return err
		}
		if err := checkSkew(c.CurrentMasterVersion, v); err != nil {
			return err
		}
		ver = v
	}
	np.Version = ver
	np.Status = containerpb.NodePool_PROVISIONING
	np.SelfLink = selfLink(r.Project, c.Location, "clusters/"+c.Name+"/nodePools/"+np.Name)
	if np.Management == nil {
		np.Management = &containerpb.NodeManagement{AutoUpgrade: true, AutoRepair: true}
	}
	if np.UpgradeSettings == nil {
		np.UpgradeSettings = &containerpb.NodePool_UpgradeSettings{MaxSurge: 1, Strategy: containerpb.NodePoolUpdateStrategy_SURGE.Enum()}
	}
	if np.MaxPodsConstraint == nil {
		np.MaxPodsConstraint = proto.Clone(c.DefaultMaxPodsConstraint).(*containerpb.MaxPodsConstraint)
	}
	np.PodIpv4CidrSize = 24
	np.InstanceGroupUrls = nil
	h := shortHash(c.Id + "/" + np.Name)
	for _, z := range np.Locations {
		np.InstanceGroupUrls = append(np.InstanceGroupUrls,
			"https://www.googleapis.com/compute/v1/projects/"+r.Project+"/zones/"+z+"/instanceGroupManagers/gke-"+trunc(c.Name, 15)+"-"+trunc(np.Name, 15)+"-"+h+"-grp")
	}
	np.Etag = s.env.IDs.Hex(8)
	return nil
}

// startCounts returns the number of nodes a new pool starts in each of its
// locations. Like GKE, an autoscaled pool without initialNodeCount starts at
// its minimum: minNodeCount per zone, or totalMinNodeCount spread across the
// zones, and at least one node per zone (or in all, for a total).
func startCounts(np *containerpb.NodePool) []int {
	counts := make([]int, len(np.Locations))
	as := np.GetAutoscaling()
	switch {
	case np.InitialNodeCount > 0 || !as.GetEnabled():
		for i := range counts {
			counts[i] = int(np.InitialNodeCount)
		}
	case as.TotalMinNodeCount > 0 || as.TotalMaxNodeCount > 0:
		total := max(int(as.TotalMinNodeCount), 1)
		for i := range counts {
			counts[i] = total / len(counts)
			if i < total%len(counts) {
				counts[i]++
			}
		}
	default:
		for i := range counts {
			counts[i] = max(int(as.MinNodeCount), 1)
		}
	}
	return counts
}

// zoneCounts returns n nodes for each of a pool's locations.
func zoneCounts(np *containerpb.NodePool, n int) []int {
	counts := make([]int, len(np.Locations))
	for i := range counts {
		counts[i] = n
	}
	return counts
}

// planNodes returns node records for a pool with counts[i] nodes in
// np.Locations[i], keeping existing nodes where possible.
func (s *Service) planNodes(c *containerpb.Cluster, np *containerpb.NodePool, existing []nodeRecord, counts []int) []nodeRecord {
	byZone := map[string][]nodeRecord{}
	for _, n := range existing {
		byZone[n.Zone] = append(byZone[n.Zone], n)
	}
	var out []nodeRecord
	h := shortHash(c.Id + "/" + np.Name)
	for zi, z := range np.Locations {
		cur := byZone[z]
		for i := 0; i < counts[zi]; i++ {
			if i < len(cur) {
				out = append(out, cur[i])
				continue
			}
			name := "gke-" + trunc(c.Name, 15) + "-" + trunc(np.Name, 15) + "-" + h + "-" + s.env.IDs.Hex(2)
			out = append(out, nodeRecord{Name: name, Pool: np.Name, Zone: z, Password: s.env.IDs.Hex(16)})
		}
	}
	return out
}

// checkNodeLimit enforces the per-instance node limit (FR-GKE-003):
// exclude is a cluster key whose current nodes are replaced by want.
func (s *Service) checkNodeLimit(tx store.Tx, exclude string, want int) error {
	used := 0
	for _, rec := range listClusters(tx, "") {
		if rec.key() != exclude {
			used += len(rec.Int.Nodes)
		}
	}
	if limit := maxNodes(); used+want > limit {
		return apierr.New(8, "gcpemu node limit exceeded: this request needs %d node(s) and %d of %d are in use (set %s to raise the limit; regional clusters run node_count nodes in each zone).",
			want, used, limit, maxNodesEnv).WithReason("container.googleapis.com", "QUOTA_EXCEEDED")
	}
	return nil
}

func (a *api) DeleteCluster(ctx context.Context, req *containerpb.DeleteClusterRequest) (*containerpb.Operation, error) {
	r, err := parseRef(req.Name, req.ProjectId, req.Zone, req.ClusterId, "", 1)
	if err != nil {
		return nil, err
	}
	if err := a.check(ctx, r, "container.clusters.delete", r.clusterResource()); err != nil {
		return nil, err
	}
	if _, err := a.load(r); err != nil {
		return nil, err
	}
	// Deleting cancels a running create, as on GKE.
	a.s.mu.Lock()
	if cur, ok := a.s.busy[r.key()]; ok {
		if cancel := a.s.opCancel[cur]; cancel != nil {
			cancel()
		}
	}
	a.s.mu.Unlock()
	if err := a.s.waitIdle(ctx, r.key()); err != nil {
		return nil, err
	}
	if err := a.s.updateCluster(r.key(), func(rec *clusterRecord, c *containerpb.Cluster) error {
		c.Status = containerpb.Cluster_STOPPING
		return nil
	}); err != nil {
		return nil, err
	}
	return a.s.startOp(r, containerpb.Operation_DELETE_CLUSTER, selfLink(r.Project, r.Location, "clusters/"+r.Cluster), true,
		func(ctx context.Context) error { return a.s.teardown(ctx, r.key()) })
}

func (a *api) UpdateCluster(ctx context.Context, req *containerpb.UpdateClusterRequest) (*containerpb.Operation, error) {
	r, err := parseRef(req.Name, req.ProjectId, req.Zone, req.ClusterId, "", 1)
	if err != nil {
		return nil, err
	}
	u := req.Update
	if u == nil {
		return nil, apierr.InvalidArgument("Required field 'update' is missing.")
	}
	if u.DesiredMasterVersion != "" {
		return a.upgradeMaster(ctx, r, u.DesiredMasterVersion, containerpb.Operation_UPGRADE_MASTER)
	}
	if u.DesiredNodeVersion != "" {
		pool := u.DesiredNodePoolId
		return a.upgradePools(ctx, r, pool, u.DesiredNodeVersion)
	}
	var work workFunc
	if u.DesiredGatewayApiConfig != nil || u.DesiredSecretManagerConfig != nil || u.DesiredSecretSyncConfig != nil {
		work = func(ctx context.Context) error { return a.s.reconcileAddons(ctx, r.key()) }
	}
	return a.mutate(ctx, r, containerpb.Operation_UPDATE_CLUSTER, u.Etag, func(rec *clusterRecord, c *containerpb.Cluster) error {
		if err := applyClusterUpdate(r, c, u); err != nil {
			return err
		}
		if work == nil {
			return nil
		}
		return a.s.checkAddons(c)
	}, work)
}

// applyClusterUpdate applies the store-only parts of a ClusterUpdate.
func applyClusterUpdate(r ref, c *containerpb.Cluster, u *containerpb.ClusterUpdate) error {
	if u.DesiredWorkloadIdentityConfig != nil {
		wp := u.DesiredWorkloadIdentityConfig.WorkloadPool
		if wp != "" && wp != r.Project+".svc.id.goog" {
			return apierr.InvalidArgument("Workload pool %q is invalid; it must be %q.", wp, r.Project+".svc.id.goog")
		}
		if wp == "" {
			c.WorkloadIdentityConfig = nil
		} else {
			c.WorkloadIdentityConfig = u.DesiredWorkloadIdentityConfig
		}
	}
	if u.DesiredNodePoolAutoscaling != nil {
		np := findPool(c, u.DesiredNodePoolId)
		if np == nil {
			return notFoundPool(ref{Project: r.Project, Location: r.Location, Cluster: r.Cluster, Pool: u.DesiredNodePoolId})
		}
		np.Autoscaling = u.DesiredNodePoolAutoscaling
	}
	if len(u.DesiredLocations) > 0 {
		for _, l := range u.DesiredLocations {
			if !isZone(l) || regionOf(l) != regionOf(c.Location) {
				return apierr.InvalidArgument("Location %q is not a zone of the cluster's region.", l)
			}
		}
		c.Locations = u.DesiredLocations
	}
	if u.DesiredLoggingService != "" {
		c.LoggingService = u.DesiredLoggingService
	}
	if u.DesiredMonitoringService != "" {
		c.MonitoringService = u.DesiredMonitoringService
	}
	if u.DesiredAddonsConfig != nil {
		c.AddonsConfig = u.DesiredAddonsConfig
	}
	if u.DesiredReleaseChannel != nil {
		c.ReleaseChannel = u.DesiredReleaseChannel
	}
	if u.DesiredMasterAuthorizedNetworksConfig != nil {
		c.MasterAuthorizedNetworksConfig = u.DesiredMasterAuthorizedNetworksConfig
	}
	if u.DesiredLoggingConfig != nil {
		c.LoggingConfig = u.DesiredLoggingConfig
	}
	if u.DesiredMonitoringConfig != nil {
		c.MonitoringConfig = u.DesiredMonitoringConfig
	}
	if u.DesiredNotificationConfig != nil {
		c.NotificationConfig = u.DesiredNotificationConfig
	}
	if u.DesiredVerticalPodAutoscaling != nil {
		c.VerticalPodAutoscaling = u.DesiredVerticalPodAutoscaling
	}
	if u.DesiredShieldedNodes != nil {
		c.ShieldedNodes = u.DesiredShieldedNodes
	}
	if u.DesiredBinaryAuthorization != nil {
		c.BinaryAuthorization = u.DesiredBinaryAuthorization
	}
	if u.DesiredClusterAutoscaling != nil {
		c.Autoscaling = u.DesiredClusterAutoscaling
	}
	if u.DesiredDatabaseEncryption != nil {
		c.DatabaseEncryption = u.DesiredDatabaseEncryption
	}
	if u.DesiredResourceUsageExportConfig != nil {
		c.ResourceUsageExportConfig = u.DesiredResourceUsageExportConfig
	}
	if u.DesiredCostManagementConfig != nil {
		c.CostManagementConfig = u.DesiredCostManagementConfig
	}
	if u.DesiredSecurityPostureConfig != nil {
		c.SecurityPostureConfig = u.DesiredSecurityPostureConfig
	}
	if u.DesiredGatewayApiConfig != nil {
		c.NetworkConfig.GatewayApiConfig = u.DesiredGatewayApiConfig
	}
	if u.DesiredSecretManagerConfig != nil {
		c.SecretManagerConfig = u.DesiredSecretManagerConfig
	}
	if u.DesiredSecretSyncConfig != nil {
		c.SecretSyncConfig = u.DesiredSecretSyncConfig
	}
	if u.DesiredDnsConfig != nil {
		c.NetworkConfig.DnsConfig = u.DesiredDnsConfig
	}
	if u.DesiredDatapathProvider != containerpb.DatapathProvider_DATAPATH_PROVIDER_UNSPECIFIED {
		c.NetworkConfig.DatapathProvider = u.DesiredDatapathProvider
	}
	if u.DesiredFleet != nil {
		c.Fleet = u.DesiredFleet
	}
	if u.DesiredEnablePrivateEndpoint != nil || u.DesiredPrivateClusterConfig != nil || u.DesiredControlPlaneEndpointsConfig != nil {
		return apierr.Unimplemented("gcpemu does not support changing control plane endpoint access of an existing cluster; recreate the cluster.")
	}
	return nil
}

// mutate applies fn to a cluster under an operation of type typ. work, if
// non-nil, runs as the operation's data-plane part with the cluster in
// RECONCILING.
func (a *api) mutate(ctx context.Context, r ref, typ containerpb.Operation_Type, etag string,
	fn func(rec *clusterRecord, c *containerpb.Cluster) error, work workFunc) (*containerpb.Operation, error) {
	if err := a.check(ctx, r, "container.clusters.update", r.clusterResource()); err != nil {
		return nil, err
	}
	if _, err := a.load(r); err != nil {
		return nil, err
	}
	a.s.mu.Lock()
	cur, busy := a.s.busy[r.key()]
	a.s.mu.Unlock()
	if busy {
		return nil, apierr.FailedPrecondition("Cluster is running incompatible operation %s.", cur).
			WithReason("container.googleapis.com", "CLUSTER_ALREADY_HAS_OPERATION")
	}
	err := a.s.updateCluster(r.key(), func(rec *clusterRecord, c *containerpb.Cluster) error {
		if etag != "" && etag != c.Etag {
			return apierr.Aborted("Cluster etag %q does not match the current etag.", etag)
		}
		if c.Status != containerpb.Cluster_RUNNING && c.Status != containerpb.Cluster_RECONCILING {
			return apierr.FailedPrecondition("Cluster %s is not running (status %s).", r.Cluster, c.Status)
		}
		if err := fn(rec, c); err != nil {
			return err
		}
		if work != nil {
			c.Status = containerpb.Cluster_RECONCILING
		}
		c.Etag = a.s.env.IDs.Hex(8)
		return nil
	})
	if err != nil {
		return nil, err
	}
	target := selfLink(r.Project, r.Location, "clusters/"+r.Cluster)
	if r.Pool != "" {
		target += "/nodePools/" + r.Pool
	}
	return a.s.startOp(r, typ, target, true, func(ctx context.Context) error {
		if work == nil {
			return nil
		}
		err := work(ctx)
		_ = a.s.updateCluster(r.key(), func(rec *clusterRecord, c *containerpb.Cluster) error {
			if c.Status == containerpb.Cluster_RECONCILING {
				c.Status = containerpb.Cluster_RUNNING
			}
			return nil
		})
		return err
	})
}

// updateCluster applies fn to the stored cluster atomically.
func (s *Service) updateCluster(key string, fn func(rec *clusterRecord, c *containerpb.Cluster) error) error {
	return s.env.Store.Update(func(tx store.Tx) error {
		rec, err := getCluster(tx, key)
		if err != nil {
			return err
		}
		c := rec.cluster()
		if err := fn(rec, c); err != nil {
			return err
		}
		rec.setCluster(c)
		return putCluster(tx, rec)
	})
}

func (a *api) UpdateMaster(ctx context.Context, req *containerpb.UpdateMasterRequest) (*containerpb.Operation, error) {
	r, err := parseRef(req.Name, req.ProjectId, req.Zone, req.ClusterId, "", 1)
	if err != nil {
		return nil, err
	}
	return a.upgradeMaster(ctx, r, req.MasterVersion, containerpb.Operation_UPGRADE_MASTER)
}

// upgradeMaster changes the control plane version: the server container is
// recreated with the new image, keeping its state volume (FR-GKE-002).
func (a *api) upgradeMaster(ctx context.Context, r ref, version string, typ containerpb.Operation_Type) (*containerpb.Operation, error) {
	var target string
	return a.mutate(ctx, r, typ, "", func(rec *clusterRecord, c *containerpb.Cluster) error {
		v, err := resolveVersion(version, c.GetReleaseChannel().GetChannel())
		if err != nil {
			return err
		}
		if compareVersions(v, c.CurrentMasterVersion) < 0 {
			return apierr.InvalidArgument("Master version %q is older than the current version %q; downgrades are not supported.", v, c.CurrentMasterVersion)
		}
		if minorDiff(c.CurrentMasterVersion, v) > 1 {
			return apierr.InvalidArgument("Master cannot be upgraded more than one minor version at a time (from %q to %q).", c.CurrentMasterVersion, v)
		}
		target = v
		return nil
	}, func(ctx context.Context) error {
		if err := a.s.upgradeServer(ctx, r.key(), target); err != nil {
			return err
		}
		return a.s.updateCluster(r.key(), func(rec *clusterRecord, c *containerpb.Cluster) error {
			c.CurrentMasterVersion = target
			return nil
		})
	})
}

// upgradePools changes the node version of one pool (or all when pool is
// empty or "-"), recreating its node containers.
func (a *api) upgradePools(ctx context.Context, r ref, pool, version string) (*containerpb.Operation, error) {
	var target string
	var pools []string
	if pool == "-" {
		pool = ""
	}
	r.Pool = pool
	return a.mutate(ctx, r, containerpb.Operation_UPGRADE_NODES, "", func(rec *clusterRecord, c *containerpb.Cluster) error {
		v := version
		if v == "-" {
			v = c.CurrentMasterVersion
		}
		v, err := resolveVersion(v, c.GetReleaseChannel().GetChannel())
		if err != nil {
			return err
		}
		if err := checkSkew(c.CurrentMasterVersion, v); err != nil {
			return err
		}
		for _, np := range c.NodePools {
			if pool == "" || np.Name == pool {
				pools = append(pools, np.Name)
				np.Version = v
			}
		}
		if len(pools) == 0 {
			return notFoundPool(r)
		}
		target = v
		return nil
	}, func(ctx context.Context) error {
		for _, p := range pools {
			if err := a.s.recreatePool(ctx, r.key(), p); err != nil {
				return err
			}
		}
		return a.s.updateCluster(r.key(), func(rec *clusterRecord, c *containerpb.Cluster) error {
			c.CurrentNodeVersion = lowestNodeVersion(c)
			_ = target
			return nil
		})
	})
}

func lowestNodeVersion(c *containerpb.Cluster) string {
	low := ""
	for _, np := range c.NodePools {
		if low == "" || compareVersions(np.Version, low) < 0 {
			low = np.Version
		}
	}
	if low == "" {
		return c.CurrentMasterVersion
	}
	return low
}

func minorDiff(a, b string) int {
	pa, pb := versionNums(a), versionNums(b)
	if len(pa) < 2 || len(pb) < 2 {
		return 0
	}
	return (pb[0]-pa[0])*100 + pb[1] - pa[1]
}

// store-only setters (each is an UPDATE operation on GKE).

func (a *api) SetLabels(ctx context.Context, req *containerpb.SetLabelsRequest) (*containerpb.Operation, error) {
	r, err := parseRef(req.Name, req.ProjectId, req.Zone, req.ClusterId, "", 1)
	if err != nil {
		return nil, err
	}
	return a.mutate(ctx, r, containerpb.Operation_SET_LABELS, "", func(rec *clusterRecord, c *containerpb.Cluster) error {
		if req.LabelFingerprint != "" && req.LabelFingerprint != c.LabelFingerprint {
			return apierr.FailedPrecondition("Labels could not be set due to fingerprint mismatch.")
		}
		c.ResourceLabels = req.ResourceLabels
		c.LabelFingerprint = labelFingerprint(c.ResourceLabels)
		return nil
	}, nil)
}

func (a *api) SetLoggingService(ctx context.Context, req *containerpb.SetLoggingServiceRequest) (*containerpb.Operation, error) {
	r, err := parseRef(req.Name, req.ProjectId, req.Zone, req.ClusterId, "", 1)
	if err != nil {
		return nil, err
	}
	return a.mutate(ctx, r, containerpb.Operation_UPDATE_CLUSTER, "", func(rec *clusterRecord, c *containerpb.Cluster) error {
		c.LoggingService = req.LoggingService
		return nil
	}, nil)
}

func (a *api) SetMonitoringService(ctx context.Context, req *containerpb.SetMonitoringServiceRequest) (*containerpb.Operation, error) {
	r, err := parseRef(req.Name, req.ProjectId, req.Zone, req.ClusterId, "", 1)
	if err != nil {
		return nil, err
	}
	return a.mutate(ctx, r, containerpb.Operation_UPDATE_CLUSTER, "", func(rec *clusterRecord, c *containerpb.Cluster) error {
		c.MonitoringService = req.MonitoringService
		return nil
	}, nil)
}

func (a *api) SetAddonsConfig(ctx context.Context, req *containerpb.SetAddonsConfigRequest) (*containerpb.Operation, error) {
	r, err := parseRef(req.Name, req.ProjectId, req.Zone, req.ClusterId, "", 1)
	if err != nil {
		return nil, err
	}
	return a.mutate(ctx, r, containerpb.Operation_UPDATE_CLUSTER, "", func(rec *clusterRecord, c *containerpb.Cluster) error {
		c.AddonsConfig = req.AddonsConfig
		return nil
	}, nil)
}

func (a *api) SetLocations(ctx context.Context, req *containerpb.SetLocationsRequest) (*containerpb.Operation, error) {
	r, err := parseRef(req.Name, req.ProjectId, req.Zone, req.ClusterId, "", 1)
	if err != nil {
		return nil, err
	}
	return a.mutate(ctx, r, containerpb.Operation_UPDATE_CLUSTER, "", func(rec *clusterRecord, c *containerpb.Cluster) error {
		return applyClusterUpdate(r, c, &containerpb.ClusterUpdate{DesiredLocations: req.Locations})
	}, nil)
}

func (a *api) SetLegacyAbac(ctx context.Context, req *containerpb.SetLegacyAbacRequest) (*containerpb.Operation, error) {
	r, err := parseRef(req.Name, req.ProjectId, req.Zone, req.ClusterId, "", 1)
	if err != nil {
		return nil, err
	}
	return a.mutate(ctx, r, containerpb.Operation_UPDATE_CLUSTER, "", func(rec *clusterRecord, c *containerpb.Cluster) error {
		c.LegacyAbac = &containerpb.LegacyAbac{Enabled: req.Enabled}
		return nil
	}, nil)
}

func (a *api) SetNetworkPolicy(ctx context.Context, req *containerpb.SetNetworkPolicyRequest) (*containerpb.Operation, error) {
	r, err := parseRef(req.Name, req.ProjectId, req.Zone, req.ClusterId, "", 1)
	if err != nil {
		return nil, err
	}
	return a.mutate(ctx, r, containerpb.Operation_SET_NETWORK_POLICY, "", func(rec *clusterRecord, c *containerpb.Cluster) error {
		c.NetworkPolicy = req.NetworkPolicy
		return nil
	}, nil)
}

func (a *api) SetMaintenancePolicy(ctx context.Context, req *containerpb.SetMaintenancePolicyRequest) (*containerpb.Operation, error) {
	r, err := parseRef(req.Name, req.ProjectId, req.Zone, req.ClusterId, "", 1)
	if err != nil {
		return nil, err
	}
	return a.mutate(ctx, r, containerpb.Operation_SET_MAINTENANCE_POLICY, "", func(rec *clusterRecord, c *containerpb.Cluster) error {
		c.MaintenancePolicy = req.MaintenancePolicy
		return nil
	}, nil)
}

// ---- operations ----

func (a *api) ListOperations(ctx context.Context, req *containerpb.ListOperationsRequest) (*containerpb.ListOperationsResponse, error) {
	r, err := parseRef(req.Parent, req.ProjectId, req.Zone, "", "", 0)
	if err != nil {
		return nil, err
	}
	if err := a.check(ctx, r, "container.operations.list", projectResource(r.Project)); err != nil {
		return nil, err
	}
	ops := a.s.listOps(r.Project, r.Location)
	sort.Slice(ops, func(i, j int) bool { return ops[i].StartTime < ops[j].StartTime })
	return &containerpb.ListOperationsResponse{Operations: ops}, nil
}

func (a *api) GetOperation(ctx context.Context, req *containerpb.GetOperationRequest) (*containerpb.Operation, error) {
	p, loc, name := req.ProjectId, req.Zone, req.OperationId
	if req.Name != "" {
		var err error
		if p, loc, name, err = opName(req.Name); err != nil {
			return nil, err
		}
	}
	if p == "" || name == "" {
		return nil, apierr.InvalidArgument("Required field 'name' is missing.")
	}
	r := ref{Project: p, Location: loc}
	if err := a.check(ctx, r, "container.operations.get", projectResource(p)); err != nil {
		return nil, err
	}
	op, err := a.s.getOp(p, name)
	if err != nil {
		return nil, err
	}
	if loc != "-" && loc != "" && op.Location != loc {
		return nil, apierr.NotFound("Not found: operation %s.", name)
	}
	return op, nil
}

func (a *api) CancelOperation(ctx context.Context, req *containerpb.CancelOperationRequest) (*emptypb.Empty, error) {
	p, loc, name := req.ProjectId, req.Zone, req.OperationId
	if req.Name != "" {
		var err error
		if p, loc, name, err = opName(req.Name); err != nil {
			return nil, err
		}
	}
	r := ref{Project: p, Location: loc}
	if err := a.check(ctx, r, "container.operations.cancel", projectResource(p)); err != nil {
		return nil, err
	}
	if err := a.s.cancelOp(p, name); err != nil {
		return nil, err
	}
	return &emptypb.Empty{}, nil
}

// ---- server config / JWKS ----

func (a *api) GetServerConfig(ctx context.Context, req *containerpb.GetServerConfigRequest) (*containerpb.ServerConfig, error) {
	r, err := parseRef(req.Name, req.ProjectId, req.Zone, "", "", 0)
	if err != nil {
		return nil, err
	}
	if err := a.check(ctx, r, "container.clusters.list", projectResource(r.Project)); err != nil {
		return nil, err
	}
	return serverConfig(), nil
}

func (a *api) GetJSONWebKeys(ctx context.Context, req *containerpb.GetJSONWebKeysRequest) (*containerpb.GetJSONWebKeysResponse, error) {
	r, err := parseRef(req.Parent, "", "", "", "", 1)
	if err != nil {
		return nil, err
	}
	if err := a.s.env.EnsureProject(r.Project); err != nil {
		return nil, err
	}
	if _, err := a.load(r); err != nil {
		return nil, err
	}
	keys, err := a.s.clusterJWKS(ctx, r.key())
	if err != nil {
		return nil, err
	}
	return &containerpb.GetJSONWebKeysResponse{Keys: keys}, nil
}

// ---- helpers ----

func labelFingerprint(l map[string]string) string {
	keys := make([]string, 0, len(l))
	for k := range l {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := sha256.New()
	for _, k := range keys {
		h.Write([]byte(k + "=" + l[k] + "\n"))
	}
	return hex.EncodeToString(h.Sum(nil))[:8]
}

func shortHash(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:4])
}

func trunc(s string, n int) string {
	if len(s) > n {
		s = strings.TrimRight(s[:n], "-")
	}
	return s
}

func lastSeg(s string) string {
	if i := strings.LastIndexByte(s, '/'); i >= 0 {
		return s[i+1:]
	}
	return s
}

// networkOf / subnetworkOf return the network and subnetwork a create
// request names (top-level or networkConfig fields).
func networkOf(c *containerpb.Cluster) string {
	if c.GetNetworkConfig().GetNetwork() != "" {
		return c.NetworkConfig.Network
	}
	return c.Network
}

func subnetworkOf(c *containerpb.Cluster) string {
	if c.GetNetworkConfig().GetSubnetwork() != "" {
		return c.NetworkConfig.Subnetwork
	}
	if c.Subnetwork != "" {
		return c.Subnetwork
	}
	return c.GetIpAllocationPolicy().GetSubnetworkName()
}

// networkFromSubnet returns "projects/P/global/networks/N" for the
// cluster: the requested network, or "default".
func networkFromSubnet(projectID string, c *containerpb.Cluster, sub string) string {
	n := networkOf(c)
	if n == "" {
		n = "default"
	}
	if strings.Contains(n, "/") {
		i := strings.Index(n, "projects/")
		if i >= 0 {
			return n[i:]
		}
	}
	return "projects/" + projectID + "/global/networks/" + lastSeg(n)
}

var _ emu.Service = (*Service)(nil)
