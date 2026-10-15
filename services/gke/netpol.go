package gke

import (
	"context"

	"cloud.google.com/go/container/apiv1/containerpb"

	"github.com/linuxuser586/gcpemu/internal/apierr"
)

// NetworkPolicy enforcement (FR-GKE-011). GKE enforces NetworkPolicy
// objects with Dataplane V2 (datapathProvider ADVANCED_DATAPATH), where it
// is always on, or with the networkPolicy setting on the legacy datapath;
// otherwise it accepts them and enforces nothing. k3s enforces them with
// kube-router's controller on every agent, which reads the server's
// --disable-network-policy when it starts.

// policyEnforced reports whether c enforces NetworkPolicy objects.
func policyEnforced(c *containerpb.Cluster) bool {
	return dataplaneV2(c) || c.GetNetworkPolicy().GetEnabled()
}

func dataplaneV2(c *containerpb.Cluster) bool {
	return c.GetNetworkConfig().GetDatapathProvider() == containerpb.DatapathProvider_ADVANCED_DATAPATH
}

// policyArgs returns the k3s server arguments for c's enforcement.
func policyArgs(c *containerpb.Cluster) []string {
	if policyEnforced(c) {
		return nil
	}
	return []string{"--disable-network-policy"}
}

// checkNetworkPolicy rejects the networkPolicy setting on Dataplane V2,
// as GKE does.
func checkNetworkPolicy(c *containerpb.Cluster) error {
	if dataplaneV2(c) && c.GetNetworkPolicy().GetEnabled() {
		return apierr.InvalidArgument("Enabling NetworkPolicy for clusters with DatapathProvider=ADVANCED_DATAPATH is not allowed. Dataplane V2 will take care of network policy enforcement instead.")
	}
	return nil
}

// setDatapathProvider applies a ClusterUpdate's datapath provider: GKE
// enables Dataplane V2 only when it creates a cluster.
func setDatapathProvider(c *containerpb.Cluster, to containerpb.DatapathProvider) error {
	if to == containerpb.DatapathProvider_DATAPATH_PROVIDER_UNSPECIFIED {
		return nil
	}
	if (to == containerpb.DatapathProvider_ADVANCED_DATAPATH) != dataplaneV2(c) {
		return apierr.InvalidArgument("The datapath provider of an existing cluster cannot be changed; Dataplane V2 is chosen when the cluster is created.")
	}
	c.NetworkConfig.DatapathProvider = to
	return nil
}

// restartForPolicy applies a change of enforcement: the server restarts
// with its new arguments, then each pool's nodes are recreated.
func (s *Service) restartForPolicy(ctx context.Context, key string) error {
	rec, err := s.loadKey(key)
	if err != nil {
		return err
	}
	c := rec.cluster()
	// At the version it runs: upgradeServer recreates the server container.
	if err := s.upgradeServer(ctx, key, c.CurrentMasterVersion); err != nil {
		return err
	}
	for _, np := range c.NodePools {
		if err := s.recreatePool(ctx, key, np.Name); err != nil {
			return err
		}
	}
	return nil
}
