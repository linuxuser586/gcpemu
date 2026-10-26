package gke

import (
	"cloud.google.com/go/container/apiv1/containerpb"
	"google.golang.org/protobuf/proto"

	"github.com/linuxuser586/gcpemu/internal/apierr"
)

// Autopilot clusters (FR-GKE-012). GKE owns their nodes: it sets Workload
// Identity, Dataplane V2, a release channel, Shielded nodes and vertical
// Pod autoscaling, refuses requests that contradict them, and reports the
// node pools it manages while refusing to let the user change them. It
// also turns managed Prometheus on unless the request turns it off
// (FR-GKE-018). The managed pool starts with one node; autoscale.go adds
// and removes nodes for the cluster's pods (FR-GKE-015), and
// restrictions.go applies Workload restrictions to them (FR-GKE-016).
//
// No Recording of an Autopilot cluster exists yet (ADR 0023), so the
// defaults and error messages below are GKE's as best known, not checked
// against one.

func autopilotOn(c *containerpb.Cluster) bool { return c.GetAutopilot().GetEnabled() }

// The messages for node pools in an Autopilot create request and a
// workload policy on a Standard cluster, which Seeding gives too.
const (
	autopilotNodePoolsMessage = "Autopilot clusters do not support node pools; GKE manages them."
	workloadPolicyMessage     = "Workload policies can only be set on Autopilot clusters."
)

// applyAutopilot validates an Autopilot create request and sets what GKE
// sets on one. It runs before newCluster fills in the Standard defaults.
func applyAutopilot(r ref, c *containerpb.Cluster) error {
	if isZone(r.Location) {
		return apierr.InvalidArgument("Autopilot clusters must be regional clusters.")
	}
	if !onlyGcloudPlaceholder(c.NodePools) || c.InitialNodeCount != 0 {
		return apierr.InvalidArgument(autopilotNodePoolsMessage)
	}
	c.NodePools = nil
	if err := checkKept(c.WorkloadIdentityConfig, c.ShieldedNodes, c.ReleaseChannel, c.VerticalPodAutoscaling); err != nil {
		return err
	}
	if dp := c.GetNetworkConfig().GetDatapathProvider(); dp != containerpb.DatapathProvider_DATAPATH_PROVIDER_UNSPECIFIED && dp != containerpb.DatapathProvider_ADVANCED_DATAPATH {
		return autopilotErr("Dataplane V2")
	}
	if c.GetNetworkPolicy().GetEnabled() {
		return apierr.InvalidArgument("Enabling NetworkPolicy for clusters with DatapathProvider=ADVANCED_DATAPATH is not allowed. Dataplane V2 will take care of network policy enforcement instead.")
	}
	// newCluster rejects a workload pool other than the Project's.
	if c.WorkloadIdentityConfig == nil {
		c.WorkloadIdentityConfig = &containerpb.WorkloadIdentityConfig{WorkloadPool: r.Project + ".svc.id.goog"}
	}
	if c.NetworkConfig == nil {
		c.NetworkConfig = &containerpb.NetworkConfig{}
	}
	c.NetworkConfig.DatapathProvider = containerpb.DatapathProvider_ADVANCED_DATAPATH
	c.ShieldedNodes = &containerpb.ShieldedNodes{Enabled: true}
	c.VerticalPodAutoscaling = &containerpb.VerticalPodAutoscaling{Enabled: true}
	c.DefaultMaxPodsConstraint = &containerpb.MaxPodsConstraint{MaxPodsPerNode: autopilotMaxPods}
	if c.IpAllocationPolicy == nil {
		c.IpAllocationPolicy = &containerpb.IPAllocationPolicy{}
	}
	c.IpAllocationPolicy.UseIpAliases = true
	// Managed Prometheus is on unless the request turns it off (FR-GKE-018).
	if c.MonitoringConfig == nil {
		c.MonitoringConfig = &containerpb.MonitoringConfig{}
	}
	if c.MonitoringConfig.ManagedPrometheusConfig == nil {
		c.MonitoringConfig.ManagedPrometheusConfig = &containerpb.ManagedPrometheusConfig{Enabled: true}
	}
	// The managed pool gets GKE's node config, not the request's.
	c.NodeConfig = nil
	return nil
}

// onlyGcloudPlaceholder reports whether pools is empty or the one
// default-pool that gcloud container clusters create-auto sends, which
// sets nothing but its node count, OAuth scopes and the cluster's labels.
func onlyGcloudPlaceholder(pools []*containerpb.NodePool) bool {
	if len(pools) == 0 {
		return true
	}
	if len(pools) > 1 || pools[0].Name != "default-pool" {
		return false
	}
	rest := proto.Clone(pools[0]).(*containerpb.NodePool)
	rest.Name, rest.InitialNodeCount = "", 0
	if rest.Config != nil {
		rest.Config.OauthScopes, rest.Config.ResourceLabels = nil, nil
		if proto.Size(rest.Config) == 0 {
			rest.Config = nil
		}
	}
	return proto.Size(rest) == 0
}

// checkAutopilotUpdate refuses a ClusterUpdate that would undo what GKE
// sets on an Autopilot cluster, and Autopilot settings on a Standard one
// but for Autopilot compatibility auditing (FR-GKE-017).
func checkAutopilotUpdate(c *containerpb.Cluster, u *containerpb.ClusterUpdate) error {
	if !autopilotOn(c) {
		if wp := u.DesiredAutopilotWorkloadPolicyConfig; wp != nil && !auditingOnly(wp) {
			return apierr.InvalidArgument(workloadPolicyMessage)
		}
		return nil
	}
	return checkKept(u.DesiredWorkloadIdentityConfig, u.DesiredShieldedNodes, u.DesiredReleaseChannel, u.DesiredVerticalPodAutoscaling)
}

// auditingOnly reports whether wp sets Autopilot compatibility auditing
// and nothing else.
func auditingOnly(wp *containerpb.WorkloadPolicyConfig) bool {
	rest := proto.Clone(wp).(*containerpb.WorkloadPolicyConfig)
	rest.AutopilotCompatibilityAuditingEnabled = nil
	return wp.AutopilotCompatibilityAuditingEnabled != nil && proto.Size(rest) == 0
}

// checkKept refuses, in a create request or a ClusterUpdate, turning off
// a setting GKE keeps on for Autopilot clusters; nil leaves it as it is.
func checkKept(wi *containerpb.WorkloadIdentityConfig, sn *containerpb.ShieldedNodes, rc *containerpb.ReleaseChannel, vpa *containerpb.VerticalPodAutoscaling) error {
	switch {
	case wi != nil && wi.WorkloadPool == "":
		return autopilotErr("Workload Identity")
	case sn != nil && !sn.Enabled:
		return autopilotErr("Shielded nodes")
	case vpa != nil && !vpa.Enabled:
		return autopilotErr("Vertical Pod autoscaling")
	case rc != nil && rc.Channel == containerpb.ReleaseChannel_UNSPECIFIED:
		return apierr.InvalidArgument("Autopilot clusters must be enrolled in a release channel.")
	}
	return nil
}

func autopilotErr(setting string) error {
	return apierr.InvalidArgument("%s cannot be disabled for Autopilot clusters.", setting)
}

// autopilotCounts places the managed pool's one node in its first zone.
func autopilotCounts(np *containerpb.NodePool) []int {
	counts := make([]int, len(np.Locations))
	if len(counts) > 0 {
		counts[0] = 1
	}
	return counts
}

// autopilotMaxPods is the pod limit of an Autopilot node.
const autopilotMaxPods = 32

// machineShape is a machine type Autopilot provisions nodes as (ADR
// 0031): its vCPUs and memory before GKE's reservations.
type machineShape struct {
	Name  string
	CPUs  int64
	MemKi int64
}

// autopilotShapes are the general-purpose shapes Autopilot provisions,
// smallest first; a cluster's first node is the smallest.
var autopilotShapes = []machineShape{
	{"ek-standard-2", 2, 8 << 20},
	{"ek-standard-4", 4, 16 << 20},
	{"ek-standard-8", 8, 32 << 20},
	{"ek-standard-16", 16, 64 << 20},
	{"ek-standard-32", 32, 128 << 20},
}

// shapeOf returns the shape of an Autopilot node; nodes recorded before
// they had one are the smallest.
func shapeOf(n nodeRecord) machineShape {
	for _, sh := range autopilotShapes {
		if sh.Name == n.MachineType {
			return sh
		}
	}
	return autopilotShapes[0]
}

// evictionKi is the kubelet's hard memory eviction threshold, 100 MiB as
// on GKE.
const evictionKi = 100 << 10

// nodeAllocatable is what an Autopilot node advertises (ADR 0031).
type nodeAllocatable struct {
	CPUMilli int64 `json:"cpuMilli"`
	MemoryKi int64 `json:"memoryKi"`
}

// allocatable is the shape's capacity less GKE's reservations: 6% of the
// first core, 1% of the next, 0.5% of the next 2 and 0.25% of the rest;
// 25% of the first 4 GiB of memory, 20% of the next 4 GiB, 10% of the
// next 8 GiB, 6% of the next 112 GiB and 2% of the rest, and the eviction
// threshold.
func (m machineShape) allocatable() nodeAllocatable {
	cpu := 60 + 10*min(max(m.CPUs-1, 0), 1) + 5*min(max(m.CPUs-2, 0), 2) + 25*max(m.CPUs-4, 0)/10
	var memRes int64
	left := m.MemKi
	for _, t := range []struct{ ki, pct int64 }{{4 << 20, 25}, {4 << 20, 20}, {8 << 20, 10}, {112 << 20, 6}, {left, 2}} {
		part := min(left, t.ki)
		memRes += part * t.pct / 100
		left -= part
	}
	return nodeAllocatable{CPUMilli: m.CPUs*1000 - cpu, MemoryKi: m.MemKi - memRes - evictionKi}
}

// refusePoolChange refuses a change to an Autopilot cluster's node pools.
func refusePoolChange(c *containerpb.Cluster) error {
	if autopilotOn(c) {
		return apierr.FailedPrecondition("Autopilot clusters do not support mutating node pools.")
	}
	return nil
}
