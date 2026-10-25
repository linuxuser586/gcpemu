package gke

import (
	"cloud.google.com/go/container/apiv1/containerpb"

	"github.com/linuxuser586/gcpemu/internal/apierr"
)

// Autopilot clusters (FR-GKE-012). GKE owns their nodes: it sets Workload
// Identity, Dataplane V2, a release channel and Shielded nodes, refuses
// requests that contradict them, and reports the node pools it manages
// while refusing to let the user change them. Until node provisioning
// (FR-GKE-015) lands, the managed pool runs one node.

func autopilotOn(c *containerpb.Cluster) bool { return c.GetAutopilot().GetEnabled() }

// applyAutopilot validates an Autopilot create request and sets what GKE
// sets on one. It runs before newCluster fills in the Standard defaults.
func applyAutopilot(r ref, c *containerpb.Cluster) error {
	if isZone(r.Location) {
		return apierr.InvalidArgument("Autopilot clusters must be regional clusters.")
	}
	if len(c.NodePools) > 0 || c.InitialNodeCount != 0 {
		return apierr.InvalidArgument("Autopilot clusters do not support node pools; GKE manages them.")
	}
	if wi := c.WorkloadIdentityConfig; wi != nil && wi.WorkloadPool == "" {
		return autopilotErr("Workload Identity")
	}
	if c.GetShieldedNodes() != nil && !c.ShieldedNodes.Enabled {
		return autopilotErr("Shielded nodes")
	}
	if c.ReleaseChannel != nil && c.ReleaseChannel.Channel == containerpb.ReleaseChannel_UNSPECIFIED {
		return apierr.InvalidArgument("Autopilot clusters must be enrolled in a release channel.")
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
	if c.IpAllocationPolicy == nil {
		c.IpAllocationPolicy = &containerpb.IPAllocationPolicy{}
	}
	c.IpAllocationPolicy.UseIpAliases = true
	return nil
}

// autopilotCounts places the managed pool's one node in its first zone.
func autopilotCounts(np *containerpb.NodePool) []int {
	counts := make([]int, len(np.Locations))
	if len(counts) > 0 {
		counts[0] = 1
	}
	return counts
}

func autopilotErr(setting string) error {
	return apierr.InvalidArgument("%s cannot be disabled for Autopilot clusters.", setting)
}

// managedPools refuses a change to an Autopilot cluster's node pools.
func managedPools(c *containerpb.Cluster) error {
	if autopilotOn(c) {
		return apierr.FailedPrecondition("Autopilot clusters do not support mutating node pools.")
	}
	return nil
}

// checkAutopilotUpdate refuses a ClusterUpdate that would undo what GKE
// sets on an Autopilot cluster, and Autopilot settings on a Standard one.
func checkAutopilotUpdate(c *containerpb.Cluster, u *containerpb.ClusterUpdate) error {
	if !autopilotOn(c) {
		if u.DesiredAutopilotWorkloadPolicyConfig != nil {
			return apierr.FailedPrecondition("Workload policies can only be set on Autopilot clusters.")
		}
		return nil
	}
	if wi := u.DesiredWorkloadIdentityConfig; wi != nil && wi.WorkloadPool == "" {
		return autopilotErr("Workload Identity")
	}
	if sn := u.DesiredShieldedNodes; sn != nil && !sn.Enabled {
		return autopilotErr("Shielded nodes")
	}
	if rc := u.DesiredReleaseChannel; rc != nil && rc.Channel == containerpb.ReleaseChannel_UNSPECIFIED {
		return apierr.InvalidArgument("Autopilot clusters must be enrolled in a release channel.")
	}
	return nil
}
