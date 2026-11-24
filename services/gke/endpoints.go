package gke

import (
	"crypto/sha256"
	"encoding/hex"

	"cloud.google.com/go/container/apiv1/containerpb"
	"google.golang.org/protobuf/proto"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/project"
)

// Control plane endpoints (#1229). A cluster reports its IP endpoints and
// its DNS endpoint as the user set them: ipEndpointsConfig.enabled and
// enablePublicEndpoint are kept, defaulting to GKE's (on, and public when
// the control plane has an external address). Whatever they say, the
// emulator serves the API server on its IP and LAN addresses, which its
// own kubeconfig uses. The private endpoint flag and the authorized
// networks each live in two configs, which change together.
//
// With dnsEndpointConfig set, the cluster reports a DNS endpoint in GKE's
// form, gke-<hash>-<project number>.<location>.gke.goog, from its create
// on, the hash fixed by the cluster's ID so that it never moves
// (dnsendpoint.go serves it, ADR 0091).

// dnsEndpointSuffix is the DNS zone of every cluster's DNS endpoint.
const dnsEndpointSuffix = ".gke.goog"

// dnsEndpoint is the DNS endpoint of cluster c in project.
func dnsEndpoint(proj string, c *containerpb.Cluster) string {
	h := sha256.Sum256([]byte(c.Id))
	return "gke-" + hex.EncodeToString(h[:16]) + "-" + project.NumberString(proj) + "." + c.Location + dnsEndpointSuffix
}

// nameDNSEndpoint reports c's DNS endpoint when it has one.
func nameDNSEndpoint(proj string, c *containerpb.Cluster) {
	if dns := c.GetControlPlaneEndpointsConfig().GetDnsEndpointConfig(); dns != nil {
		dns.Endpoint = dnsEndpoint(proj, c)
	}
}

// ipEndpoints returns c's ipEndpointsConfig, adding an empty one.
func ipEndpoints(c *containerpb.Cluster) *containerpb.ControlPlaneEndpointsConfig_IPEndpointsConfig {
	if c.ControlPlaneEndpointsConfig == nil {
		c.ControlPlaneEndpointsConfig = &containerpb.ControlPlaneEndpointsConfig{}
	}
	cp := c.ControlPlaneEndpointsConfig
	if cp.IpEndpointsConfig == nil {
		cp.IpEndpointsConfig = &containerpb.ControlPlaneEndpointsConfig_IPEndpointsConfig{}
	}
	return cp.IpEndpointsConfig
}

// recordEndpoints reports that c's API server is served at priv, its
// private address, and pub, its public one ("" when it has none).
func recordEndpoints(proj string, c *containerpb.Cluster, priv, pub string) {
	c.Endpoint = pub
	if c.Endpoint == "" || privateEndpoint(c) {
		c.Endpoint = priv
	}
	if c.PrivateClusterConfig != nil {
		c.PrivateClusterConfig.PrivateEndpoint = priv
		c.PrivateClusterConfig.PublicEndpoint = pub
	}
	ipc := ipEndpoints(c)
	if ipc.Enabled == nil {
		t := true
		ipc.Enabled = &t
	}
	if ipc.EnablePublicEndpoint == nil {
		on := pub != "" && !privateEndpoint(c)
		ipc.EnablePublicEndpoint = &on
	}
	ipc.PublicEndpoint = pub
	ipc.PrivateEndpoint = priv
	// OpenTofu reads the authorized networks from ipEndpointsConfig.
	switch {
	case ipc.AuthorizedNetworksConfig == nil && c.MasterAuthorizedNetworksConfig != nil:
		ipc.AuthorizedNetworksConfig = proto.Clone(c.MasterAuthorizedNetworksConfig).(*containerpb.MasterAuthorizedNetworksConfig)
	case c.MasterAuthorizedNetworksConfig == nil && ipc.AuthorizedNetworksConfig != nil:
		c.MasterAuthorizedNetworksConfig = proto.Clone(ipc.AuthorizedNetworksConfig).(*containerpb.MasterAuthorizedNetworksConfig)
	}
	nameDNSEndpoint(proj, c)
}

// applyEndpointsUpdate changes c's control plane endpoints as u asks, in
// place, and reports them again at the addresses its API server is served
// at. It refuses only a public IP endpoint for a control plane that has no
// external address, which the emulator cannot add to a running cluster.
func applyEndpointsUpdate(proj string, c *containerpb.Cluster, u *containerpb.ClusterUpdate) error {
	desired := u.DesiredControlPlaneEndpointsConfig
	if u.DesiredEnablePrivateEndpoint == nil && u.DesiredPrivateClusterConfig == nil && desired == nil && u.DesiredMasterAuthorizedNetworksConfig == nil {
		return nil
	}
	ipc := ipEndpoints(c)
	priv, pub := ipc.PrivateEndpoint, ipc.PublicEndpoint
	setPublic := func(on bool) {
		ipc.EnablePublicEndpoint = &on
		if c.PrivateClusterConfig != nil {
			c.PrivateClusterConfig.EnablePrivateEndpoint = !on
		}
	}
	setNetworks := func(n *containerpb.MasterAuthorizedNetworksConfig) {
		c.MasterAuthorizedNetworksConfig = proto.Clone(n).(*containerpb.MasterAuthorizedNetworksConfig)
		ipc.AuthorizedNetworksConfig = proto.Clone(n).(*containerpb.MasterAuthorizedNetworksConfig)
	}
	if n := u.DesiredMasterAuthorizedNetworksConfig; n != nil {
		setNetworks(n)
	}
	if pc := u.DesiredPrivateClusterConfig; pc != nil {
		if c.PrivateClusterConfig == nil {
			c.PrivateClusterConfig = &containerpb.PrivateClusterConfig{}
		}
		setPublic(!pc.EnablePrivateEndpoint)
		if ga := pc.MasterGlobalAccessConfig; ga != nil {
			c.PrivateClusterConfig.MasterGlobalAccessConfig = proto.Clone(ga).(*containerpb.PrivateClusterMasterGlobalAccessConfig)
			on := ga.Enabled
			ipc.GlobalAccess = &on
		}
	}
	if v := u.DesiredEnablePrivateEndpoint; v != nil {
		setPublic(!*v)
	}
	if wantDNS := desired.GetDnsEndpointConfig(); wantDNS != nil {
		cp := c.ControlPlaneEndpointsConfig
		if cp.DnsEndpointConfig == nil {
			cp.DnsEndpointConfig = &containerpb.ControlPlaneEndpointsConfig_DNSEndpointConfig{}
		}
		dns := cp.DnsEndpointConfig
		if wantDNS.AllowExternalTraffic != nil {
			dns.AllowExternalTraffic = wantDNS.AllowExternalTraffic
		}
		if wantDNS.EnableK8STokensViaDns != nil {
			dns.EnableK8STokensViaDns = wantDNS.EnableK8STokensViaDns
		}
		if wantDNS.EnableK8SCertsViaDns != nil {
			dns.EnableK8SCertsViaDns = wantDNS.EnableK8SCertsViaDns
		}
	}
	if wantIP := desired.GetIpEndpointsConfig(); wantIP != nil {
		if wantIP.Enabled != nil {
			ipc.Enabled = wantIP.Enabled
		}
		if wantIP.EnablePublicEndpoint != nil {
			setPublic(*wantIP.EnablePublicEndpoint)
		}
		if wantIP.GlobalAccess != nil {
			ipc.GlobalAccess = wantIP.GlobalAccess
		}
		if wantIP.AuthorizedNetworksConfig != nil {
			setNetworks(wantIP.AuthorizedNetworksConfig)
		}
	}
	// A cluster not served yet has no addresses to keep: its bring-up
	// reports them, and fills the defaults left unset.
	if priv == "" {
		nameDNSEndpoint(proj, c)
		return nil
	}
	// With IP endpoints off, no public one is offered, whatever is asked.
	if pub == "" && !privateEndpoint(c) && (ipc.Enabled == nil || *ipc.Enabled) {
		return apierr.FailedPrecondition("gcpemu cannot add a public endpoint to cluster %s, whose control plane was created without an external address; recreate the cluster.", c.Name)
	}
	recordEndpoints(proj, c, priv, pub)
	return nil
}
