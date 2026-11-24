package gke

import (
	"context"
	"regexp"
	"testing"

	"cloud.google.com/go/container/apiv1/containerpb"
	"google.golang.org/grpc/codes"
)

// dnsEndpointName is GKE's form of a cluster's DNS-based endpoint, for a
// cluster in us-central1 of Project ap-test.
var dnsEndpointName = regexp.MustCompile(`^gke-[0-9a-f]{32}-[0-9]+\.us-central1\.gke\.goog$`)

// serveAt records the addresses a bring-up serves cluster name's API server
// on, as startServer does once it is ready, and returns the cluster.
func serveAt(t *testing.T, a *api, name, priv, pub string) *containerpb.Cluster {
	t.Helper()
	r, _ := parseRef(apParent, "", "", "", "", 0)
	r.Cluster = name
	if err := a.s.updateCluster(r.key(), func(_ *clusterRecord, c *containerpb.Cluster) error {
		recordEndpoints(r.Project, c, priv, pub)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	got, err := a.GetCluster(context.Background(), &containerpb.GetClusterRequest{Name: r.clusterName()})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// TestEndpointsReportedAsSet is #1229: a cluster made with IP endpoints
// off and the DNS endpoint on reports them so, with a DNS endpoint in
// GKE's form that stays the same, so that OpenTofu plans clean.
func TestEndpointsReportedAsSet(t *testing.T) {
	a := newAPI(t)
	f, tr := false, true
	createRunning(t, a, apParent, &containerpb.Cluster{Name: "dns", InitialNodeCount: 1,
		ControlPlaneEndpointsConfig: &containerpb.ControlPlaneEndpointsConfig{
			DnsEndpointConfig: &containerpb.ControlPlaneEndpointsConfig_DNSEndpointConfig{AllowExternalTraffic: &tr},
			IpEndpointsConfig: &containerpb.ControlPlaneEndpointsConfig_IPEndpointsConfig{Enabled: &f},
		}})
	got := serveAt(t, a, "dns", "127.0.0.1:30001", "127.0.0.1:30001")
	cp := got.GetControlPlaneEndpointsConfig()
	if ipc := cp.GetIpEndpointsConfig(); ipc.Enabled == nil || *ipc.Enabled {
		t.Errorf("ipEndpointsConfig.enabled = %v, want false", ipc.Enabled)
	}
	dns := cp.GetDnsEndpointConfig()
	if !dnsEndpointName.MatchString(dns.GetEndpoint()) {
		t.Errorf("dnsEndpointConfig.endpoint = %q, want gke-<hash>-<project number>.us-central1.gke.goog", dns.GetEndpoint())
	}
	if !dns.GetAllowExternalTraffic() {
		t.Error("dnsEndpointConfig.allowExternalTraffic = false, want true as set")
	}
	again := serveAt(t, a, "dns", "127.0.0.1:30002", "127.0.0.1:30002")
	if e := again.GetControlPlaneEndpointsConfig().GetDnsEndpointConfig().GetEndpoint(); e != dns.GetEndpoint() {
		t.Errorf("DNS endpoint moved from %q to %q when the API server was served again", dns.GetEndpoint(), e)
	}

	// A cluster that names neither gets GKE's default: IP endpoints on.
	a = newAPI(t)
	createRunning(t, a, apParent, &containerpb.Cluster{Name: "plain", InitialNodeCount: 1})
	plain := serveAt(t, a, "plain", "127.0.0.1:30003", "")
	if ipc := plain.GetControlPlaneEndpointsConfig().GetIpEndpointsConfig(); !ipc.GetEnabled() || ipc.GetEnablePublicEndpoint() {
		t.Errorf("default ipEndpointsConfig = %v, want enabled without a public endpoint", ipc)
	}
	if plain.Endpoint != "127.0.0.1:30003" {
		t.Errorf("endpoint = %q, want the private address", plain.Endpoint)
	}
}

// TestEndpointsUpdateInPlace is #1229: an existing cluster takes a change
// of its control plane endpoints in place, as GKE does, keeping the
// addresses its API server is served at; only a public endpoint for a
// control plane without an external address is refused.
func TestEndpointsUpdateInPlace(t *testing.T) {
	a := newAPI(t)
	ctx := context.Background()
	createRunning(t, a, apParent, &containerpb.Cluster{Name: "c", InitialNodeCount: 1})
	serveAt(t, a, "c", "10.0.0.2:443", "127.0.0.1:30001")
	cl := apParent + "/clusters/c"
	update := func(what string, u *containerpb.ClusterUpdate) *containerpb.Cluster {
		t.Helper()
		op, err := a.UpdateCluster(ctx, &containerpb.UpdateClusterRequest{Name: cl, Update: u})
		if err != nil {
			t.Fatalf("%s: UpdateCluster: %v", what, err)
		}
		waitOp(t, a, "ap-test", op.Name)
		got, err := a.GetCluster(ctx, &containerpb.GetClusterRequest{Name: cl})
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	f, tr := false, true
	got := update("DNS endpoint on, IP endpoints off", &containerpb.ClusterUpdate{DesiredControlPlaneEndpointsConfig: &containerpb.ControlPlaneEndpointsConfig{
		DnsEndpointConfig: &containerpb.ControlPlaneEndpointsConfig_DNSEndpointConfig{AllowExternalTraffic: &tr},
		IpEndpointsConfig: &containerpb.ControlPlaneEndpointsConfig_IPEndpointsConfig{Enabled: &f},
	}})
	cp := got.GetControlPlaneEndpointsConfig()
	if cp.GetIpEndpointsConfig().Enabled == nil || cp.GetIpEndpointsConfig().GetEnabled() {
		t.Errorf("ipEndpointsConfig.enabled = %v, want false", cp.GetIpEndpointsConfig().Enabled)
	}
	if !dnsEndpointName.MatchString(cp.GetDnsEndpointConfig().GetEndpoint()) || !cp.GetDnsEndpointConfig().GetAllowExternalTraffic() {
		t.Errorf("dnsEndpointConfig = %v, want an endpoint in GKE's form allowing external traffic", cp.GetDnsEndpointConfig())
	}
	if ipc := cp.GetIpEndpointsConfig(); ipc.GetPrivateEndpoint() != "10.0.0.2:443" || ipc.GetPublicEndpoint() != "127.0.0.1:30001" {
		t.Errorf("IP endpoint addresses = %q %q, want those the API server is served at", ipc.GetPrivateEndpoint(), ipc.GetPublicEndpoint())
	}

	got = update("private endpoint", &containerpb.ClusterUpdate{DesiredEnablePrivateEndpoint: &tr})
	if got.Endpoint != "10.0.0.2:443" || got.GetControlPlaneEndpointsConfig().GetIpEndpointsConfig().GetEnablePublicEndpoint() {
		t.Errorf("after enabling the private endpoint: endpoint %q, ipEndpointsConfig %v; want the private address and no public endpoint",
			got.Endpoint, got.GetControlPlaneEndpointsConfig().GetIpEndpointsConfig())
	}
	if !got.GetControlPlaneEndpointsConfig().GetDnsEndpointConfig().GetAllowExternalTraffic() {
		t.Error("an update that names only the private endpoint dropped the DNS endpoint")
	}

	nets := &containerpb.MasterAuthorizedNetworksConfig{Enabled: true, CidrBlocks: []*containerpb.MasterAuthorizedNetworksConfig_CidrBlock{{CidrBlock: "10.1.0.0/16"}}}
	got = update("authorized networks", &containerpb.ClusterUpdate{DesiredControlPlaneEndpointsConfig: &containerpb.ControlPlaneEndpointsConfig{
		IpEndpointsConfig: &containerpb.ControlPlaneEndpointsConfig_IPEndpointsConfig{Enabled: &tr, AuthorizedNetworksConfig: nets},
	}})
	if b := got.GetMasterAuthorizedNetworksConfig().GetCidrBlocks(); len(b) != 1 || b[0].GetCidrBlock() != "10.1.0.0/16" {
		t.Errorf("masterAuthorizedNetworksConfig = %v, want 10.1.0.0/16 as ipEndpointsConfig set it", got.GetMasterAuthorizedNetworksConfig())
	}

	a = newAPI(t)
	createRunning(t, a, apParent, &containerpb.Cluster{Name: "c", InitialNodeCount: 1})
	serveAt(t, a, "c", "10.0.0.2:443", "")
	_, err := a.UpdateCluster(ctx, &containerpb.UpdateClusterRequest{Name: cl, Update: &containerpb.ClusterUpdate{
		DesiredControlPlaneEndpointsConfig: &containerpb.ControlPlaneEndpointsConfig{
			IpEndpointsConfig: &containerpb.ControlPlaneEndpointsConfig_IPEndpointsConfig{EnablePublicEndpoint: &tr},
		}}})
	wantCode(t, "a public endpoint for a control plane without an external address", err, codes.FailedPrecondition)
}

// TestEndpointsOffWithPublicFlag is #1229: turning IP endpoints off is
// accepted whatever enablePublicEndpoint the same update sends, as
// OpenTofu sends the whole config, on a control plane without an external
// address.
func TestEndpointsOffWithPublicFlag(t *testing.T) {
	a := newAPI(t)
	createRunning(t, a, apParent, &containerpb.Cluster{Name: "c", InitialNodeCount: 1})
	serveAt(t, a, "c", "10.0.0.2:443", "")
	f, tr := false, true
	_, err := a.UpdateCluster(context.Background(), &containerpb.UpdateClusterRequest{Name: apParent + "/clusters/c", Update: &containerpb.ClusterUpdate{
		DesiredControlPlaneEndpointsConfig: &containerpb.ControlPlaneEndpointsConfig{
			DnsEndpointConfig: &containerpb.ControlPlaneEndpointsConfig_DNSEndpointConfig{AllowExternalTraffic: &tr},
			IpEndpointsConfig: &containerpb.ControlPlaneEndpointsConfig_IPEndpointsConfig{Enabled: &f, EnablePublicEndpoint: &tr},
		}}})
	if err != nil {
		t.Fatalf("UpdateCluster: %v", err)
	}
}

// TestEndpointsBeforeBringUp is #1229: a cluster reports its DNS endpoint
// from its create, and an update before its API server is served leaves
// the defaults the bring-up fills, such as a public endpoint, to it.
func TestEndpointsBeforeBringUp(t *testing.T) {
	a := newAPI(t)
	tr := true
	got := createRunning(t, a, apParent, &containerpb.Cluster{Name: "c", InitialNodeCount: 1,
		ControlPlaneEndpointsConfig: &containerpb.ControlPlaneEndpointsConfig{
			DnsEndpointConfig: &containerpb.ControlPlaneEndpointsConfig_DNSEndpointConfig{AllowExternalTraffic: &tr},
		}})
	if e := got.GetControlPlaneEndpointsConfig().GetDnsEndpointConfig().GetEndpoint(); !dnsEndpointName.MatchString(e) {
		t.Errorf("DNS endpoint after create = %q, want one in GKE's form", e)
	}
	op, err := a.UpdateCluster(context.Background(), &containerpb.UpdateClusterRequest{Name: apParent + "/clusters/c", Update: &containerpb.ClusterUpdate{
		DesiredMasterAuthorizedNetworksConfig: &containerpb.MasterAuthorizedNetworksConfig{Enabled: true}}})
	if err != nil {
		t.Fatal(err)
	}
	waitOp(t, a, "ap-test", op.Name)
	got = serveAt(t, a, "c", "10.0.0.2:443", "127.0.0.1:30001")
	if !got.GetControlPlaneEndpointsConfig().GetIpEndpointsConfig().GetEnablePublicEndpoint() || got.Endpoint != "127.0.0.1:30001" {
		t.Errorf("after bring-up: endpoint %q, ipEndpointsConfig %v; want the public endpoint", got.Endpoint, got.GetControlPlaneEndpointsConfig().GetIpEndpointsConfig())
	}
}
