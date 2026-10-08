package compute_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	computev1 "google.golang.org/api/compute/v1"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
	snv1 "google.golang.org/api/servicenetworking/v1"

	"github.com/linuxuser586/gcpemu/emutest"
	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/project"
)

const proj = "test-project"

func newClients(t *testing.T, opts ...emutest.Option) (*emutest.Instance, *computev1.Service, *snv1.APIService) {
	t.Helper()
	inst := emutest.Start(t, []string{"compute"}, opts...)
	ctx := context.Background()
	c, err := computev1.NewService(ctx, option.WithEndpoint(inst.GatewayURL()+"/compute/v1/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	sn, err := snv1.NewService(ctx, option.WithEndpoint(inst.GatewayURL()+"/servicenetworking/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	return inst, c, sn
}

// waitDo returns a function that polls a compute operation to completion
// and fails on call or operation errors.
func waitDo(t *testing.T, c *computev1.Service) func(*computev1.Operation, error) *computev1.Operation {
	return func(op *computev1.Operation, err error) *computev1.Operation {
		t.Helper()
		return wait(t, c, op, err)
	}
}

func wait(t *testing.T, c *computev1.Service, op *computev1.Operation, err error) *computev1.Operation {
	t.Helper()
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	op = waitOp(t, c, op)
	if op.Error != nil {
		t.Fatalf("operation %s failed: %+v", op.Name, op.Error.Errors[0])
	}
	return op
}

func waitOp(t *testing.T, c *computev1.Service, op *computev1.Operation) *computev1.Operation {
	t.Helper()
	var err error
	for op.Status != "DONE" {
		switch {
		case op.Zone != "":
			op, err = c.ZoneOperations.Wait(proj, lastSeg(op.Zone), op.Name).Do()
		case op.Region != "":
			op, err = c.RegionOperations.Wait(proj, lastSeg(op.Region), op.Name).Do()
		default:
			op, err = c.GlobalOperations.Wait(proj, op.Name).Do()
		}
		if err != nil {
			t.Fatalf("wait: %v", err)
		}
	}
	return op
}

func lastSeg(s string) string { return s[strings.LastIndex(s, "/")+1:] }

func code(err error) int {
	var ge *googleapi.Error
	if errors.As(err, &ge) {
		return ge.Code
	}
	return 0
}

func reason(err error) string {
	var ge *googleapi.Error
	if errors.As(err, &ge) && len(ge.Errors) > 0 {
		return ge.Errors[0].Reason
	}
	return ""
}

func customNet(t *testing.T, c *computev1.Service, name string) {
	t.Helper()
	waitDo(t, c)(c.Networks.Insert(proj, &computev1.Network{Name: name, AutoCreateSubnetworks: false, ForceSendFields: []string{"AutoCreateSubnetworks"}}).Do())
}

func subnet(t *testing.T, c *computev1.Service, net, name, region, cidr string, secondary ...*computev1.SubnetworkSecondaryRange) {
	t.Helper()
	waitDo(t, c)(c.Subnetworks.Insert(proj, region, &computev1.Subnetwork{
		Name: name, Network: "global/networks/" + net, IpCidrRange: cidr, SecondaryIpRanges: secondary,
	}).Do())
}

func TestRegionsZonesProject(t *testing.T) {
	inst, c, _ := newClients(t)
	if got, want := inst.EnvVars()["CLOUDSDK_API_ENDPOINT_OVERRIDES_COMPUTE"], inst.GatewayURL()+"/compute/v1/"; got != want {
		t.Fatalf("gcloud override %q want %q", got, want)
	}
	r, err := c.Regions.Get(proj, "us-central1").Do()
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != "UP" || len(r.Zones) != 4 || !strings.HasPrefix(r.SelfLink, "https://www.googleapis.com/compute/v1/projects/"+proj+"/regions/us-central1") {
		t.Fatalf("region: %+v", r)
	}
	if _, err := c.Regions.Get(proj, "mars-north1").Do(); code(err) != http.StatusNotFound {
		t.Fatalf("bad region: %v", err)
	}
	z, err := c.Zones.Get(proj, "europe-west1-b").Do()
	if err != nil || !strings.HasSuffix(z.Region, "/regions/europe-west1") {
		t.Fatalf("zone: %+v %v", z, err)
	}
	if _, err := c.Zones.Get(proj, "europe-west1-a").Do(); code(err) != http.StatusNotFound {
		t.Fatalf("europe-west1-a does not exist: %v", err)
	}
	zl, err := c.Zones.List(proj).Filter("name = us-east1-*").Do()
	if err != nil || len(zl.Items) != 3 {
		t.Fatalf("zones filter: %v %d", err, len(zl.Items))
	}
	p, err := c.Projects.Get(proj).Do()
	if err != nil {
		t.Fatal(err)
	}
	if want := project.NumberString(proj) + "-compute@developer.gserviceaccount.com"; p.DefaultServiceAccount != want {
		t.Fatalf("default SA %q want %q", p.DefaultServiceAccount, want)
	}
}

func TestNetworksAndSubnetworks(t *testing.T) {
	_, c, _ := newClients(t)
	// Auto mode: one subnetwork per region with GCP's auto ranges.
	op := waitDo(t, c)(c.Networks.Insert(proj, &computev1.Network{Name: "auto", AutoCreateSubnetworks: true}).Do())
	if op.OperationType != "insert" || !strings.HasSuffix(op.TargetLink, "/global/networks/auto") || op.Progress != 100 {
		t.Fatalf("op: %+v", op)
	}
	sn, err := c.Subnetworks.Get(proj, "us-central1", "auto").Do()
	if err != nil || sn.IpCidrRange != "10.128.0.0/20" || sn.GatewayAddress != "10.128.0.1" {
		t.Fatalf("auto subnet: %+v %v", sn, err)
	}
	n, err := c.Networks.Get(proj, "auto").Do()
	if err != nil || len(n.Subnetworks) < 40 || n.Mtu != 1460 || n.RoutingConfig.RoutingMode != "REGIONAL" {
		t.Fatalf("network: %+v %v", n, err)
	}
	if _, err := time.Parse(time.RFC3339, n.CreationTimestamp); err != nil || !strings.HasSuffix(n.CreationTimestamp, "-07:00") {
		t.Fatalf("creationTimestamp %q", n.CreationTimestamp)
	}
	// Duplicate insert.
	if _, err := c.Networks.Insert(proj, &computev1.Network{Name: "auto"}).Do(); code(err) != http.StatusConflict || reason(err) != "alreadyExists" {
		t.Fatalf("dup: %v", err)
	}
	if _, err := c.Networks.Insert(proj, &computev1.Network{Name: "Bad_Name"}).Do(); code(err) != http.StatusBadRequest {
		t.Fatalf("bad name: %v", err)
	}

	customNet(t, c, "vpc")
	subnet(t, c, "vpc", "a", "us-central1", "10.10.0.0/24",
		&computev1.SubnetworkSecondaryRange{RangeName: "pods", IpCidrRange: "10.20.0.0/16"})
	// Overlap with the primary and secondary range of "a".
	for _, cidr := range []string{"10.10.0.0/25", "10.20.5.0/24"} {
		_, err := c.Subnetworks.Insert(proj, "europe-west1", &computev1.Subnetwork{Name: "b", Network: "global/networks/vpc", IpCidrRange: cidr}).Do()
		if code(err) != http.StatusBadRequest {
			t.Fatalf("overlap %s: %v", cidr, err)
		}
	}
	if _, err := c.Subnetworks.Insert(proj, "us-central1", &computev1.Subnetwork{Name: "c", Network: "global/networks/vpc", IpCidrRange: "10.30.0.1/24"}).Do(); code(err) != http.StatusBadRequest {
		t.Fatalf("unaligned: %v", err)
	}
	subnet(t, c, "vpc", "b", "europe-west1", "10.11.0.0/24")

	// List with filter and pagination.
	l, err := c.Networks.List(proj).Filter(`name = "vpc"`).Do()
	if err != nil || len(l.Items) != 1 || l.Items[0].Name != "vpc" || len(l.Items[0].Subnetworks) != 2 {
		t.Fatalf("filtered list: %+v %v", l, err)
	}
	pg, err := c.Networks.List(proj).MaxResults(1).Do()
	if err != nil || len(pg.Items) != 1 || pg.NextPageToken == "" {
		t.Fatalf("page 1: %v", err)
	}
	pg2, err := c.Networks.List(proj).MaxResults(1).PageToken(pg.NextPageToken).Do()
	if err != nil || len(pg2.Items) != 1 || pg2.Items[0].Name == pg.Items[0].Name {
		t.Fatalf("page 2: %v", err)
	}
	agg, err := c.Subnetworks.AggregatedList(proj).Filter("network eq .*/vpc").Do()
	if err != nil || len(agg.Items["regions/us-central1"].Subnetworks) != 1 || len(agg.Items["regions/europe-west1"].Subnetworks) != 1 {
		t.Fatalf("aggregated: %v %+v", err, agg)
	}

	// Patch with fingerprints (FR-CORE-022).
	cur, _ := c.Subnetworks.Get(proj, "us-central1", "a").Do()
	if _, err := c.Subnetworks.Patch(proj, "us-central1", "a", &computev1.Subnetwork{PrivateIpGoogleAccess: true, Fingerprint: "stale"}).Do(); code(err) != http.StatusPreconditionFailed {
		t.Fatalf("stale fingerprint: %v", err)
	}
	waitDo(t, c)(c.Subnetworks.Patch(proj, "us-central1", "a", &computev1.Subnetwork{PrivateIpGoogleAccess: true, Fingerprint: cur.Fingerprint}).Do())
	got, _ := c.Subnetworks.Get(proj, "us-central1", "a").Do()
	if !got.PrivateIpGoogleAccess || got.Fingerprint == cur.Fingerprint || got.IpCidrRange != "10.10.0.0/24" || len(got.SecondaryIpRanges) != 1 {
		t.Fatalf("patched: %+v", got)
	}
	waitDo(t, c)(c.Subnetworks.ExpandIpCidrRange(proj, "us-central1", "a", &computev1.SubnetworksExpandIpCidrRangeRequest{IpCidrRange: "10.10.0.0/22"}).Do())
	if got, _ := c.Subnetworks.Get(proj, "us-central1", "a").Do(); got.IpCidrRange != "10.10.0.0/22" {
		t.Fatalf("expand: %s", got.IpCidrRange)
	}

	// Resource-in-use rules (FR-CORE-026).
	_, err = c.Networks.Delete(proj, "vpc").Do()
	if code(err) != http.StatusBadRequest || reason(err) != "resourceInUseByAnotherResource" || !strings.Contains(err.Error(), "subnetworks/") {
		t.Fatalf("delete in-use network: %v", err)
	}
	waitDo(t, c)(c.Firewalls.Insert(proj, &computev1.Firewall{Name: "allow-ssh", Network: "global/networks/auto",
		Allowed: []*computev1.FirewallAllowed{{IPProtocol: "tcp", Ports: []string{"22"}}}}).Do())
	if _, err := c.Networks.Delete(proj, "auto").Do(); reason(err) != "resourceInUseByAnotherResource" {
		t.Fatalf("network used by firewall: %v", err)
	}
	if _, err := c.Subnetworks.Delete(proj, "us-central1", "auto").Do(); reason(err) != "resourceInUseByAnotherResource" {
		t.Fatalf("auto subnet delete: %v", err)
	}
	waitDo(t, c)(c.Firewalls.Delete(proj, "allow-ssh").Do())
	waitDo(t, c)(c.Networks.Delete(proj, "auto").Do())
	if _, err := c.Subnetworks.Get(proj, "us-central1", "auto").Do(); code(err) != http.StatusNotFound {
		t.Fatalf("auto subnets must go with the network: %v", err)
	}
	waitDo(t, c)(c.Subnetworks.Delete(proj, "us-central1", "a").Do())
	waitDo(t, c)(c.Subnetworks.Delete(proj, "europe-west1", "b").Do())
	waitDo(t, c)(c.Networks.Delete(proj, "vpc").Do())
	if _, err := c.Networks.Get(proj, "vpc").Do(); code(err) != http.StatusNotFound || !strings.Contains(err.Error(), "projects/"+proj+"/global/networks/vpc") {
		t.Fatalf("deleted: %v", err)
	}
}

func TestFirewallsAndRoutes(t *testing.T) {
	_, c, _ := newClients(t)
	customNet(t, c, "net")
	waitDo(t, c)(c.Firewalls.Insert(proj, &computev1.Firewall{Name: "fw", Network: "global/networks/net",
		Allowed: []*computev1.FirewallAllowed{{IPProtocol: "tcp", Ports: []string{"80", "443"}}}, TargetTags: []string{"web"}}).Do())
	fw, err := c.Firewalls.Get(proj, "fw").Do()
	if err != nil || fw.Direction != "INGRESS" || fw.Priority != 1000 || len(fw.SourceRanges) != 1 || fw.SourceRanges[0] != "0.0.0.0/0" {
		t.Fatalf("firewall: %+v %v", fw, err)
	}
	waitDo(t, c)(c.Firewalls.Patch(proj, "fw", &computev1.Firewall{Priority: 900, SourceRanges: []string{"10.0.0.0/8"}}).Do())
	fw, _ = c.Firewalls.Get(proj, "fw").Do()
	if fw.Priority != 900 || fw.SourceRanges[0] != "10.0.0.0/8" || len(fw.Allowed) != 1 {
		t.Fatalf("patched firewall: %+v", fw)
	}
	if _, err := c.Firewalls.Insert(proj, &computev1.Firewall{Name: "bad", Network: "global/networks/net"}).Do(); code(err) != http.StatusBadRequest {
		t.Fatalf("no allowed/denied: %v", err)
	}
	waitDo(t, c)(c.Routes.Insert(proj, &computev1.Route{Name: "r", Network: "global/networks/net", DestRange: "192.168.0.0/16", NextHopIp: "10.0.0.5"}).Do())
	rl, err := c.Routes.List(proj).Filter(`(network = "https://www.googleapis.com/compute/v1/projects/` + proj + `/global/networks/net") (destRange = "0.0.0.0/0")`).Do()
	if err != nil || len(rl.Items) != 1 || !strings.HasSuffix(rl.Items[0].NextHopGateway, "/global/gateways/default-internet-gateway") {
		t.Fatalf("default route: %v %+v", err, rl)
	}
	if _, err := c.Networks.Delete(proj, "net").Do(); reason(err) != "resourceInUseByAnotherResource" {
		t.Fatalf("network used by route: %v", err)
	}
	waitDo(t, c)(c.Routes.Delete(proj, "r").Do())
	waitDo(t, c)(c.Firewalls.Delete(proj, "fw").Do())
	waitDo(t, c)(c.Networks.Delete(proj, "net").Do())
}

func TestAddressesRoutersAndNAT(t *testing.T) {
	_, c, _ := newClients(t)
	customNet(t, c, "vpc")
	subnet(t, c, "vpc", "s1", "us-central1", "10.0.0.0/24")
	subnet(t, c, "vpc", "s2", "us-central1", "10.0.1.0/24")

	waitDo(t, c)(c.Addresses.Insert(proj, "us-central1", &computev1.Address{Name: "nat-ip"}).Do())
	a, err := c.Addresses.Get(proj, "us-central1", "nat-ip").Do()
	if err != nil || a.AddressType != "EXTERNAL" || a.Status != "RESERVED" || !strings.HasPrefix(a.Address, "34.") {
		t.Fatalf("address: %+v %v", a, err)
	}
	waitDo(t, c)(c.Addresses.Insert(proj, "us-central1", &computev1.Address{Name: "int", AddressType: "INTERNAL", Subnetwork: "regions/us-central1/subnetworks/s1"}).Do())
	in, _ := c.Addresses.Get(proj, "us-central1", "int").Do()
	if in.Address != "10.0.0.2" || in.Purpose != "GCE_ENDPOINT" {
		t.Fatalf("internal address: %+v", in)
	}

	waitDo(t, c)(c.Routers.Insert(proj, "us-central1", &computev1.Router{
		Name: "router", Network: "global/networks/vpc",
		Nats: []*computev1.RouterNat{{
			Name: "nat", NatIpAllocateOption: "MANUAL_ONLY", NatIps: []string{a.SelfLink},
			SourceSubnetworkIpRangesToNat: "LIST_OF_SUBNETWORKS",
			Subnetworks:                   []*computev1.RouterNatSubnetworkToNat{{Name: "regions/us-central1/subnetworks/s1", SourceIpRangesToNat: []string{"ALL_IP_RANGES"}}},
			LogConfig:                     &computev1.RouterNatLogConfig{Enable: true, Filter: "ERRORS_ONLY"},
			MinPortsPerVm:                 128,
		}},
	}).Do())
	rt, err := c.Routers.Get(proj, "us-central1", "router").Do()
	if err != nil || len(rt.Nats) != 1 || !strings.HasSuffix(rt.Nats[0].Subnetworks[0].Name, "/regions/us-central1/subnetworks/s1") || rt.Nats[0].MinPortsPerVm != 128 {
		t.Fatalf("router: %+v %v", rt, err)
	}
	if a, _ := c.Addresses.Get(proj, "us-central1", "nat-ip").Do(); a.Status != "IN_USE" || len(a.Users) != 1 {
		t.Fatalf("NAT IP not in use: %+v", a)
	}
	if _, err := c.Addresses.Delete(proj, "us-central1", "nat-ip").Do(); reason(err) != "resourceInUseByAnotherResource" {
		t.Fatalf("delete NAT IP: %v", err)
	}
	// A second NAT may not claim s1 again.
	_, err = c.Routers.Insert(proj, "us-central1", &computev1.Router{Name: "r2", Network: "global/networks/vpc",
		Nats: []*computev1.RouterNat{{Name: "n2", NatIpAllocateOption: "AUTO_ONLY", SourceSubnetworkIpRangesToNat: "ALL_SUBNETWORKS_ALL_IP_RANGES"}}}).Do()
	if code(err) != http.StatusBadRequest {
		t.Fatalf("conflicting NAT: %v", err)
	}
	// Validation.
	_, err = c.Routers.Insert(proj, "us-central1", &computev1.Router{Name: "r3", Network: "global/networks/vpc",
		Nats: []*computev1.RouterNat{{Name: "n3", NatIpAllocateOption: "MANUAL_ONLY", SourceSubnetworkIpRangesToNat: "ALL_SUBNETWORKS_ALL_IP_RANGES"}}}).Do()
	if code(err) != http.StatusBadRequest {
		t.Fatalf("MANUAL_ONLY without IPs: %v", err)
	}

	// Patch to AUTO_ONLY over all subnetworks: the reserved IP is freed.
	waitDo(t, c)(c.Routers.Patch(proj, "us-central1", "router", &computev1.Router{Nats: []*computev1.RouterNat{{
		Name: "nat", NatIpAllocateOption: "AUTO_ONLY", SourceSubnetworkIpRangesToNat: "ALL_SUBNETWORKS_ALL_IP_RANGES",
	}}}).Do())
	if a, _ := c.Addresses.Get(proj, "us-central1", "nat-ip").Do(); a.Status != "RESERVED" {
		t.Fatalf("NAT IP should be released: %+v", a)
	}
	st, err := c.Routers.GetRouterStatus(proj, "us-central1", "router").Do()
	if err != nil || len(st.Result.NatStatus) != 1 || len(st.Result.NatStatus[0].AutoAllocatedNatIps) != 1 {
		t.Fatalf("status: %+v %v", st, err)
	}
	if _, err := c.Routers.GetNatMappingInfo(proj, "us-central1", "router").Do(); err != nil {
		t.Fatalf("mapping info: %v", err)
	}
	agg, err := c.Routers.AggregatedList(proj).Do()
	if err != nil || len(agg.Items["regions/us-central1"].Routers) != 1 {
		t.Fatalf("routers aggregated: %v", err)
	}
	if _, err := c.Networks.Delete(proj, "vpc").Do(); reason(err) != "resourceInUseByAnotherResource" {
		t.Fatalf("network used: %v", err)
	}
	if _, err := c.Subnetworks.Delete(proj, "us-central1", "s1").Do(); reason(err) != "resourceInUseByAnotherResource" {
		t.Fatalf("subnet used by address: %v", err)
	}
	waitDo(t, c)(c.Routers.Delete(proj, "us-central1", "router").Do())
	waitDo(t, c)(c.Addresses.Delete(proj, "us-central1", "nat-ip").Do())
	waitDo(t, c)(c.Addresses.Delete(proj, "us-central1", "int").Do())

	// Labels with fingerprint.
	waitDo(t, c)(c.GlobalAddresses.Insert(proj, &computev1.Address{Name: "lb"}).Do())
	ga, _ := c.GlobalAddresses.Get(proj, "lb").Do()
	if !strings.HasPrefix(ga.Address, "34.") || ga.IpVersion != "IPV4" || ga.LabelFingerprint != "42WmSpB8rSM=" {
		t.Fatalf("global address: %+v", ga)
	}
	if _, err := c.GlobalAddresses.SetLabels(proj, "lb", &computev1.GlobalSetLabelsRequest{Labels: map[string]string{"a": "b"}, LabelFingerprint: "x"}).Do(); code(err) != http.StatusPreconditionFailed {
		t.Fatalf("label fingerprint: %v", err)
	}
	waitDo(t, c)(c.GlobalAddresses.SetLabels(proj, "lb", &computev1.GlobalSetLabelsRequest{Labels: map[string]string{"a": "b"}, LabelFingerprint: ga.LabelFingerprint}).Do())
}

func TestOperationsLatency(t *testing.T) {
	const latency = 300 * time.Millisecond
	inst, c, _ := newClients(t, func(cfg *config.Config) { cfg.LROLatency["compute"] = latency.String() })
	start := time.Now()
	op, err := c.Networks.Insert(proj, &computev1.Network{Name: "slow", AutoCreateSubnetworks: true}).Do()
	if err != nil {
		t.Fatal(err)
	}
	if op.Status != "RUNNING" {
		t.Fatalf("status %s", op.Status)
	}
	// The operation's timer starts after start, so before start+latency the
	// network cannot exist yet; a slower round trip (-race on a loaded
	// host) proves nothing either way.
	_, err = c.Networks.Get(proj, "slow").Do()
	if time.Since(start) < latency && code(err) != http.StatusNotFound {
		t.Fatalf("network must not exist before the operation completes: %v", err)
	}
	got, err := c.GlobalOperations.Get(proj, op.Name).Do()
	if err != nil || got.Name != op.Name {
		t.Fatalf("get op: %v", err)
	}
	done, err := c.GlobalOperations.Wait(proj, op.Name).Do()
	if err != nil || done.Status != "DONE" || done.EndTime == "" {
		t.Fatalf("wait: %+v %v", done, err)
	}
	if _, err := c.Networks.Get(proj, "slow").Do(); err != nil {
		t.Fatal(err)
	}
	l, err := c.GlobalOperations.List(proj).Do()
	if err != nil || len(l.Items) != 1 {
		t.Fatalf("list ops: %v", err)
	}
	agg, err := c.GlobalOperations.AggregatedList(proj).Do()
	if err != nil || len(agg.Items["global"].Operations) != 1 {
		t.Fatalf("aggregated ops: %v", err)
	}
	if err := c.GlobalOperations.Delete(proj, op.Name).Do(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.GlobalOperations.Get(proj, op.Name).Do(); code(err) != http.StatusNotFound {
		t.Fatalf("deleted op: %v", err)
	}
	// Errors after acceptance are reported in the operation: the delete is
	// accepted while the network is unused, but the subnetwork inserted
	// before it completes first.
	ins, err := c.Subnetworks.Insert(proj, "us-east1", &computev1.Subnetwork{Name: "x", Network: "global/networks/slow", IpCidrRange: "192.168.50.0/24"}).Do()
	if err != nil {
		t.Fatal(err)
	}
	del, err := c.Networks.Delete(proj, "slow").Do()
	if err != nil {
		t.Fatal(err)
	}
	if ins = waitOp(t, c, ins); ins.Error != nil {
		t.Fatalf("insert: %+v", ins.Error.Errors[0])
	}
	del = waitOp(t, c, del)
	if del.Error == nil || del.Error.Errors[0].Code != "RESOURCE_IN_USE_BY_ANOTHER_RESOURCE" || del.HttpErrorStatusCode != 400 {
		t.Fatalf("delete must fail in the operation: %+v", del)
	}

	// The admin API summarises them (FR-UI-012).
	ops := map[string]emu.OperationInfo{}
	for _, op := range inst.Operations("compute", "") {
		ops[op.Type] = op
	}
	sum, sub := ops["delete"], ops["insert"]
	if len(ops) != 2 {
		t.Fatalf("operations = %+v", ops)
	}
	if sum.Name != "projects/"+proj+"/global/operations/"+del.Name || sum.Project != proj || sum.Location != "global" ||
		sum.Type != "delete" || sum.Target != "projects/"+proj+"/global/networks/slow" || !sum.Done || sum.Status != "DONE" ||
		sum.Error == nil || sum.Error.Code != "RESOURCE_IN_USE_BY_ANOTHER_RESOURCE" ||
		sum.StartTime.IsZero() || sum.EndTime.Sub(sum.StartTime) < latency {
		t.Fatalf("delete summary = %+v %+v", sum, sum.Error)
	}
	if sub.Location != "us-east1" || sub.Target != "projects/"+proj+"/regions/us-east1/subnetworks/x" || sub.Error != nil {
		t.Fatalf("insert summary = %+v", sub)
	}
}

func TestNEGs(t *testing.T) {
	inst, c, _ := newClients(t)
	customNet(t, c, "vpc")
	subnet(t, c, "vpc", "s", "us-central1", "10.0.0.0/24")
	waitDo(t, c)(c.NetworkEndpointGroups.Insert(proj, "us-central1-a", &computev1.NetworkEndpointGroup{
		Name: "neg", Network: "global/networks/vpc", Subnetwork: "regions/us-central1/subnetworks/s", DefaultPort: 8080,
	}).Do())
	g, err := c.NetworkEndpointGroups.Get(proj, "us-central1-a", "neg").Do()
	if err != nil || g.NetworkEndpointType != "GCE_VM_IP_PORT" || g.Size != 0 {
		t.Fatalf("neg: %+v %v", g, err)
	}
	waitDo(t, c)(c.NetworkEndpointGroups.AttachNetworkEndpoints(proj, "us-central1-a", "neg", &computev1.NetworkEndpointGroupsAttachEndpointsRequest{
		NetworkEndpoints: []*computev1.NetworkEndpoint{{IpAddress: "10.0.0.5", Instance: "node-1"}, {IpAddress: "10.0.0.6", Port: 9090, Instance: "node-2"}},
	}).Do())
	eps, err := c.NetworkEndpointGroups.ListNetworkEndpoints(proj, "us-central1-a", "neg", &computev1.NetworkEndpointGroupsListEndpointsRequest{}).Do()
	if err != nil || len(eps.Items) != 2 || eps.Items[0].NetworkEndpoint.Port != 8080 {
		t.Fatalf("endpoints: %+v %v", eps, err)
	}
	waitDo(t, c)(c.NetworkEndpointGroups.DetachNetworkEndpoints(proj, "us-central1-a", "neg", &computev1.NetworkEndpointGroupsDetachEndpointsRequest{
		NetworkEndpoints: []*computev1.NetworkEndpoint{{IpAddress: "10.0.0.5", Port: 8080, Instance: "node-1"}},
	}).Do())
	if g, _ := c.NetworkEndpointGroups.Get(proj, "us-central1-a", "neg").Do(); g.Size != 1 {
		t.Fatalf("size after detach: %d", g.Size)
	}
	if _, err := c.Subnetworks.Delete(proj, "us-central1", "s").Do(); reason(err) != "resourceInUseByAnotherResource" {
		t.Fatalf("subnet used by NEG: %v", err)
	}

	// The emu.VPC NEG methods (GKE NEG sync, FR-GKE-008).
	svc, _ := inst.Env.Lookup("compute")
	vpc := svc.(emu.VPC)
	ctx := context.Background()
	if err := vpc.UpsertNEG(ctx, proj, "us-central1-b", "k8s1-neg", "projects/"+proj+"/global/networks/vpc", "projects/"+proj+"/regions/us-central1/subnetworks/s", 80, `{"cluster-uid":"x"}`); err != nil {
		t.Fatal(err)
	}
	if err := vpc.SetNEGEndpoints(ctx, proj, "us-central1-b", "k8s1-neg", []emu.NEGEndpoint{{IP: "10.0.0.9", Port: 80, Instance: "gke-node"}}); err != nil {
		t.Fatal(err)
	}
	g, err = c.NetworkEndpointGroups.Get(proj, "us-central1-b", "k8s1-neg").Do()
	if err != nil || g.Size != 1 || g.Description != `{"cluster-uid":"x"}` {
		t.Fatalf("synced neg: %+v %v", g, err)
	}
	agg, err := c.NetworkEndpointGroups.AggregatedList(proj).Do()
	if err != nil || len(agg.Items["zones/us-central1-b"].NetworkEndpointGroups) != 1 {
		t.Fatalf("aggregated negs: %v", err)
	}
	if err := vpc.DeleteNEG(ctx, proj, "us-central1-b", "k8s1-neg"); err != nil {
		t.Fatal(err)
	}
	waitDo(t, c)(c.NetworkEndpointGroups.Delete(proj, "us-central1-a", "neg").Do())
	waitDo(t, c)(c.Subnetworks.Delete(proj, "us-central1", "s").Do())
}

func TestRegionalAndGlobalNEGs(t *testing.T) {
	_, c, _ := newClients(t)
	customNet(t, c, "vpc")
	const region = "us-central1"

	// Regional: serverless (no endpoints), internet IP and PSC.
	if _, err := c.RegionNetworkEndpointGroups.Insert(proj, region, &computev1.NetworkEndpointGroup{
		Name: "run", NetworkEndpointType: "SERVERLESS",
	}).Do(); code(err) != http.StatusBadRequest {
		t.Fatalf("serverless NEG without a target: %v", err)
	}
	waitDo(t, c)(c.RegionNetworkEndpointGroups.Insert(proj, region, &computev1.NetworkEndpointGroup{
		Name: "run", NetworkEndpointType: "SERVERLESS", CloudRun: &computev1.NetworkEndpointGroupCloudRun{Service: "hello"},
	}).Do())
	g, err := c.RegionNetworkEndpointGroups.Get(proj, region, "run").Do()
	if err != nil || lastSeg(g.Region) != region || g.Zone != "" || g.Network != "" {
		t.Fatalf("serverless NEG: %+v %v", g, err)
	}
	if _, err := c.RegionNetworkEndpointGroups.AttachNetworkEndpoints(proj, region, "run", &computev1.RegionNetworkEndpointGroupsAttachEndpointsRequest{
		NetworkEndpoints: []*computev1.NetworkEndpoint{{IpAddress: "203.0.113.1", Port: 443}},
	}).Do(); code(err) != http.StatusBadRequest {
		t.Fatalf("endpoint on a serverless NEG: %v", err)
	}
	if _, err := c.RegionNetworkEndpointGroups.Insert(proj, region, &computev1.NetworkEndpointGroup{
		Name: "inet", NetworkEndpointType: "INTERNET_IP_PORT",
	}).Do(); code(err) != http.StatusBadRequest {
		t.Fatalf("regional internet NEG without a network: %v", err)
	}
	waitDo(t, c)(c.RegionNetworkEndpointGroups.Insert(proj, region, &computev1.NetworkEndpointGroup{
		Name: "inet", NetworkEndpointType: "INTERNET_IP_PORT", Network: "global/networks/vpc",
	}).Do())
	waitDo(t, c)(c.RegionNetworkEndpointGroups.AttachNetworkEndpoints(proj, region, "inet", &computev1.RegionNetworkEndpointGroupsAttachEndpointsRequest{
		NetworkEndpoints: []*computev1.NetworkEndpoint{{IpAddress: "203.0.113.1", Port: 443}},
	}).Do())
	if g, _ := c.RegionNetworkEndpointGroups.Get(proj, region, "inet").Do(); g.Size != 1 {
		t.Fatalf("regional internet NEG size: %d", g.Size)
	}
	if _, err := c.RegionNetworkEndpointGroups.AttachNetworkEndpoints(proj, region, "inet", &computev1.RegionNetworkEndpointGroupsAttachEndpointsRequest{
		NetworkEndpoints: []*computev1.NetworkEndpoint{{IpAddress: "203.0.113.2", Port: 443}},
	}).Do(); err != nil {
		t.Fatal(err)
	} else if op, _ := c.RegionNetworkEndpointGroups.Get(proj, region, "inet").Do(); op.Size != 1 {
		t.Fatalf("internet NEG took a second endpoint: %d", op.Size)
	}
	waitDo(t, c)(c.RegionNetworkEndpointGroups.Insert(proj, region, &computev1.NetworkEndpointGroup{
		Name: "psc", NetworkEndpointType: "PRIVATE_SERVICE_CONNECT", PscTargetService: region + "-cloudkms.googleapis.com",
	}).Do())
	l, err := c.RegionNetworkEndpointGroups.List(proj, region).Do()
	if err != nil || len(l.Items) != 3 {
		t.Fatalf("regional NEGs: %+v %v", l, err)
	}
	if _, err := c.Networks.Delete(proj, "vpc").Do(); reason(err) != "resourceInUseByAnotherResource" {
		t.Fatalf("network used by a regional NEG: %v", err)
	}

	// Global: internet FQDN NEG with one endpoint.
	if _, err := c.GlobalNetworkEndpointGroups.Insert(proj, &computev1.NetworkEndpointGroup{
		Name: "ext", NetworkEndpointType: "GCE_VM_IP_PORT",
	}).Do(); code(err) != http.StatusBadRequest {
		t.Fatalf("global zonal-type NEG: %v", err)
	}
	waitDo(t, c)(c.GlobalNetworkEndpointGroups.Insert(proj, &computev1.NetworkEndpointGroup{
		Name: "ext", NetworkEndpointType: "INTERNET_FQDN_PORT", DefaultPort: 443,
	}).Do())
	if _, err := c.GlobalNetworkEndpointGroups.AttachNetworkEndpoints(proj, "ext", &computev1.GlobalNetworkEndpointGroupsAttachEndpointsRequest{
		NetworkEndpoints: []*computev1.NetworkEndpoint{{IpAddress: "203.0.113.1"}},
	}).Do(); code(err) != http.StatusBadRequest {
		t.Fatalf("IP endpoint on an FQDN NEG: %v", err)
	}
	waitDo(t, c)(c.GlobalNetworkEndpointGroups.AttachNetworkEndpoints(proj, "ext", &computev1.GlobalNetworkEndpointGroupsAttachEndpointsRequest{
		NetworkEndpoints: []*computev1.NetworkEndpoint{{Fqdn: "origin.example.test"}},
	}).Do())
	eps, err := c.GlobalNetworkEndpointGroups.ListNetworkEndpoints(proj, "ext").Do()
	if err != nil || len(eps.Items) != 1 || eps.Items[0].NetworkEndpoint.Fqdn != "origin.example.test" || eps.Items[0].NetworkEndpoint.Port != 443 {
		t.Fatalf("global endpoints: %+v %v", eps, err)
	}
	agg, err := c.NetworkEndpointGroups.AggregatedList(proj).Do()
	if err != nil || len(agg.Items["global"].NetworkEndpointGroups) != 1 || len(agg.Items["regions/"+region].NetworkEndpointGroups) != 3 {
		t.Fatalf("aggregated NEGs: %+v %v", agg, err)
	}
	waitDo(t, c)(c.GlobalNetworkEndpointGroups.DetachNetworkEndpoints(proj, "ext", &computev1.GlobalNetworkEndpointGroupsDetachEndpointsRequest{
		NetworkEndpoints: []*computev1.NetworkEndpoint{{Fqdn: "origin.example.test", Port: 443}},
	}).Do())
	waitDo(t, c)(c.GlobalNetworkEndpointGroups.Delete(proj, "ext").Do())
	for _, n := range []string{"run", "inet", "psc"} {
		waitDo(t, c)(c.RegionNetworkEndpointGroups.Delete(proj, region, n).Do())
	}
	waitDo(t, c)(c.Networks.Delete(proj, "vpc").Do())
}

func TestNetworkPeering(t *testing.T) {
	_, c, _ := newClients(t)
	customNet(t, c, "a")
	customNet(t, c, "b")
	subnet(t, c, "a", "a", "us-central1", "10.0.0.0/24")
	subnet(t, c, "b", "b", "us-central1", "10.0.0.0/16")
	peer := func(n *computev1.Network) *computev1.NetworkPeering {
		t.Helper()
		if len(n.Peerings) != 1 {
			t.Fatalf("peerings of %s: %+v", n.Name, n.Peerings)
		}
		return n.Peerings[0]
	}
	if _, err := c.Networks.AddPeering(proj, "a", &computev1.NetworksAddPeeringRequest{NetworkPeering: &computev1.NetworkPeering{
		Name: "self", Network: "global/networks/a",
	}}).Do(); code(err) != http.StatusBadRequest {
		t.Fatalf("peering with itself: %v", err)
	}
	if _, err := c.Networks.AddPeering(proj, "a", &computev1.NetworksAddPeeringRequest{NetworkPeering: &computev1.NetworkPeering{
		Name: "x", Network: "global/networks/missing",
	}}).Do(); code(err) != http.StatusNotFound {
		t.Fatalf("peering with a missing network: %v", err)
	}
	waitDo(t, c)(c.Networks.AddPeering(proj, "a", &computev1.NetworksAddPeeringRequest{NetworkPeering: &computev1.NetworkPeering{
		Name: "a-b", Network: "projects/" + proj + "/global/networks/b", ExportCustomRoutes: true,
	}}).Do())
	n, _ := c.Networks.Get(proj, "a").Do()
	if p := peer(n); p.State != "INACTIVE" || !p.ExchangeSubnetRoutes || p.StackType != "IPV4_ONLY" || !p.ExportCustomRoutes {
		t.Fatalf("one-sided peering: %+v", p)
	}
	// The other side would activate it, but the subnetworks overlap.
	if _, err := c.Networks.AddPeering(proj, "b", &computev1.NetworksAddPeeringRequest{NetworkPeering: &computev1.NetworkPeering{
		Name: "b-a", Network: "global/networks/a",
	}}).Do(); code(err) != http.StatusBadRequest {
		t.Fatalf("overlapping peering: %v", err)
	}
	waitDo(t, c)(c.Subnetworks.Delete(proj, "us-central1", "b").Do())
	subnet(t, c, "b", "b", "us-central1", "10.1.0.0/16")
	// The legacy request form.
	waitDo(t, c)(c.Networks.AddPeering(proj, "b", &computev1.NetworksAddPeeringRequest{
		Name: "b-a", PeerNetwork: "global/networks/a", AutoCreateRoutes: true,
	}).Do())
	for _, name := range []string{"a", "b"} {
		n, _ := c.Networks.Get(proj, name).Do()
		if p := peer(n); p.State != "ACTIVE" || p.PeerMtu != 1460 {
			t.Fatalf("peering of %s: %+v", name, p)
		}
	}
	waitDo(t, c)(c.Networks.UpdatePeering(proj, "b", &computev1.NetworksUpdatePeeringRequest{NetworkPeering: &computev1.NetworkPeering{
		Name: "b-a", ImportCustomRoutes: true,
	}}).Do())
	if n, _ := c.Networks.Get(proj, "b").Do(); !peer(n).ImportCustomRoutes {
		t.Fatalf("updated peering: %+v", peer(n))
	}
	if _, err := c.Networks.Delete(proj, "a").Do(); reason(err) != "resourceInUseByAnotherResource" {
		t.Fatalf("network with a peering: %v", err)
	}
	waitDo(t, c)(c.Networks.RemovePeering(proj, "b", &computev1.NetworksRemovePeeringRequest{Name: "b-a"}).Do())
	if n, _ := c.Networks.Get(proj, "a").Do(); peer(n).State != "INACTIVE" {
		t.Fatalf("peering after the peer left: %+v", peer(n))
	}
	if _, err := c.Networks.RemovePeering(proj, "b", &computev1.NetworksRemovePeeringRequest{Name: "b-a"}).Do(); code(err) != http.StatusNotFound {
		t.Fatalf("removing a removed peering: %v", err)
	}
	waitDo(t, c)(c.Networks.RemovePeering(proj, "a", &computev1.NetworksRemovePeeringRequest{Name: "a-b"}).Do())
	waitDo(t, c)(c.Subnetworks.Delete(proj, "us-central1", "a").Do())
	waitDo(t, c)(c.Networks.Delete(proj, "a").Do())
}

func TestServiceNetworking(t *testing.T) {
	inst, c, sn := newClients(t)
	customNet(t, c, "vpc")
	subnet(t, c, "vpc", "s", "us-central1", "10.0.0.0/20")
	waitDo(t, c)(c.GlobalAddresses.Insert(proj, &computev1.Address{Name: "psa", AddressType: "INTERNAL", Purpose: "VPC_PEERING", PrefixLength: 16, Network: "global/networks/vpc"}).Do())
	ga, _ := c.GlobalAddresses.Get(proj, "psa").Do()
	if ga.Address == "" || ga.PrefixLength != 16 || ga.Address == "10.0.0.0" {
		t.Fatalf("PSA range must avoid the subnet: %+v", ga)
	}
	netName := "projects/" + project.NumberString(proj) + "/global/networks/vpc"
	ctx := context.Background()
	svc, _ := inst.Env.Lookup("compute")
	vpc := svc.(emu.VPC)
	if _, err := vpc.PrivateServicesNetwork(ctx, "projects/"+proj+"/global/networks/vpc"); err == nil {
		t.Fatal("PrivateServicesNetwork without a connection must fail")
	}
	op, err := sn.Services.Connections.Create("services/servicenetworking.googleapis.com", &snv1.Connection{Network: netName, ReservedPeeringRanges: []string{"psa"}}).Do()
	if err != nil || !op.Done || op.Error != nil {
		t.Fatalf("create connection: %+v %v", op, err)
	}
	if got, err := sn.Operations.Get(op.Name).Do(); err != nil || !got.Done {
		t.Fatalf("get op: %v", err)
	}
	l, err := sn.Services.Connections.List("services/servicenetworking.googleapis.com").Network(netName).Do()
	if err != nil || len(l.Connections) != 1 || l.Connections[0].Peering != "servicenetworking-googleapis-com" {
		t.Fatalf("list: %+v %v", l, err)
	}
	n, _ := c.Networks.Get(proj, "vpc").Do()
	if len(n.Peerings) != 1 || n.Peerings[0].State != "ACTIVE" {
		t.Fatalf("peerings: %+v", n.Peerings)
	}
	if _, err := c.GlobalAddresses.Delete(proj, "psa").Do(); reason(err) != "resourceInUseByAnotherResource" {
		t.Fatalf("psa range in use: %v", err)
	}
	if _, err := c.Networks.Delete(proj, "vpc").Do(); reason(err) != "resourceInUseByAnotherResource" {
		t.Fatalf("network peered: %v", err)
	}
	op, err = sn.Services.Connections.DeleteConnection("services/servicenetworking.googleapis.com/connections/servicenetworking-googleapis-com",
		&snv1.DeleteConnectionRequest{ConsumerNetwork: netName}).Do()
	if err != nil || !op.Done || op.Error != nil {
		t.Fatalf("delete connection: %+v %v", op, err)
	}
	waitDo(t, c)(c.GlobalAddresses.Delete(proj, "psa").Do())
}
