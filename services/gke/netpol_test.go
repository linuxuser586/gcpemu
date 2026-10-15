package gke

import (
	"slices"
	"testing"

	"cloud.google.com/go/container/apiv1/containerpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// FR-GKE-011: NetworkPolicy is enforced with Dataplane V2 or the
// networkPolicy setting, and otherwise accepted but not enforced, as on a
// GKE cluster with the legacy datapath.
func TestNetworkPolicyEnforced(t *testing.T) {
	dpv2 := &containerpb.NetworkConfig{DatapathProvider: containerpb.DatapathProvider_ADVANCED_DATAPATH}
	legacy := &containerpb.NetworkConfig{DatapathProvider: containerpb.DatapathProvider_LEGACY_DATAPATH}
	tests := []struct {
		name string
		c    *containerpb.Cluster
		want bool
	}{
		{"defaults", &containerpb.Cluster{}, false},
		{"legacy datapath", &containerpb.Cluster{NetworkConfig: legacy}, false},
		{"dataplane v2", &containerpb.Cluster{NetworkConfig: dpv2}, true},
		{"network policy enabled", &containerpb.Cluster{NetworkPolicy: &containerpb.NetworkPolicy{Enabled: true, Provider: containerpb.NetworkPolicy_CALICO}}, true},
		{"network policy disabled", &containerpb.Cluster{NetworkConfig: legacy, NetworkPolicy: &containerpb.NetworkPolicy{Provider: containerpb.NetworkPolicy_CALICO}}, false},
	}
	for _, tt := range tests {
		if got := policyEnforced(tt.c); got != tt.want {
			t.Errorf("%s: policyEnforced = %v; want %v", tt.name, got, tt.want)
		}
		if got := slices.Contains(policyArgs(tt.c), "--disable-network-policy"); got == tt.want {
			t.Errorf("%s: policyArgs = %v", tt.name, policyArgs(tt.c))
		}
	}
}

// GKE rejects the networkPolicy setting on Dataplane V2, which enforces
// NetworkPolicy itself, and enables Dataplane V2 only at creation.
func TestNetworkPolicyValidation(t *testing.T) {
	dpv2 := func() *containerpb.Cluster {
		return &containerpb.Cluster{NetworkConfig: &containerpb.NetworkConfig{DatapathProvider: containerpb.DatapathProvider_ADVANCED_DATAPATH}}
	}
	c := dpv2()
	c.NetworkPolicy = &containerpb.NetworkPolicy{Enabled: true}
	if err := checkNetworkPolicy(c); status.Code(err) != codes.InvalidArgument {
		t.Errorf("network policy on Dataplane V2: %v", err)
	}
	c.NetworkPolicy.Enabled = false
	if err := checkNetworkPolicy(c); err != nil {
		t.Errorf("disabled network policy on Dataplane V2: %v", err)
	}
	if err := checkNetworkPolicy(&containerpb.Cluster{NetworkPolicy: &containerpb.NetworkPolicy{Enabled: true}}); err != nil {
		t.Errorf("network policy on the legacy datapath: %v", err)
	}

	r := ref{Project: "p"}
	update := func(c *containerpb.Cluster, to containerpb.DatapathProvider) error {
		return applyClusterUpdate(r, c, &containerpb.ClusterUpdate{DesiredDatapathProvider: to})
	}
	legacy := &containerpb.Cluster{NetworkConfig: &containerpb.NetworkConfig{}}
	if err := update(legacy, containerpb.DatapathProvider_ADVANCED_DATAPATH); status.Code(err) != codes.InvalidArgument {
		t.Errorf("enabling Dataplane V2: %v", err)
	}
	if err := update(dpv2(), containerpb.DatapathProvider_LEGACY_DATAPATH); status.Code(err) != codes.InvalidArgument {
		t.Errorf("disabling Dataplane V2: %v", err)
	}
	if err := update(legacy, containerpb.DatapathProvider_LEGACY_DATAPATH); err != nil {
		t.Errorf("unchanged legacy datapath: %v", err)
	}
	if err := update(dpv2(), containerpb.DatapathProvider_ADVANCED_DATAPATH); err != nil {
		t.Errorf("unchanged Dataplane V2: %v", err)
	}
}
