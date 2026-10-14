package compute_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	computev1 "google.golang.org/api/compute/v1"

	"github.com/linuxuser586/gcpemu/emutest"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// seedWorkloads records subnetwork sp of network vpc as realised on a
// container network holding workloads vm-0.. at ips, as the runtime would.
func seedWorkloads(t *testing.T, inst *emutest.Instance, sp, cidr string, ips ...string) {
	t.Helper()
	name := "gcpemu-test-nodes"
	st, _ := json.Marshal(map[string]any{"name": name, "subnet": cidr, "range": cidr,
		"network": "projects/" + proj + "/global/networks/vpc", "region": "us-central1", "subnetwork": sp})
	err := inst.Env.Store.Update(func(tx store.Tx) error {
		if err := tx.Put("compute/vpcnets", sp, st); err != nil {
			return err
		}
		for i, ip := range ips {
			vm := fmt.Sprintf("projects/%s/zones/us-central1-a/instances/vm-%d", proj, i)
			if err := tx.Put("compute/ipalloc", name+"/ip/"+ip, []byte(vm)); err != nil {
				return err
			}
			if err := tx.Put("compute/ipalloc", name+"/owner/"+vm, []byte(ip)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// mappings renders getNatMappingInfo as "vm ports total|drain total" lines.
func mappings(t *testing.T, c *computev1.Service) string {
	t.Helper()
	m, err := c.Routers.GetNatMappingInfo(proj, "us-central1", "router").Do()
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, r := range m.Result {
		for _, im := range r.InterfaceNatMappings {
			fmt.Fprintf(&b, "%s %s %s %d|%s %d\n", lastSeg(r.InstanceName), im.SourceVirtualIp,
				strings.Join(im.NatIpPortRanges, ","), im.NumTotalNatPorts, strings.Join(im.DrainNatIpPortRanges, ","), im.NumTotalDrainNatPorts)
		}
	}
	return b.String()
}

// TestNatPortReporting is FR-NAT-003: getNatMappingInfo and
// getRouterStatus report the port blocks the NAT allocated, including
// blocks on drain IPs, the workloads left without ports when the NAT IPs
// are exhausted and the NAT IPs they need, and AUTO_ONLY NATs gaining the
// IPs their workloads need.
func TestNatPortReporting(t *testing.T) {
	inst, c, _ := newClients(t)
	customNet(t, c, "vpc")
	subnet(t, c, "vpc", "nodes", "us-central1", "10.30.0.0/24")
	seedWorkloads(t, inst, "projects/"+proj+"/regions/us-central1/subnetworks/nodes", "10.30.0.0/24", "10.30.0.2", "10.30.0.3", "10.30.0.4")
	var ips []string
	for _, n := range []string{"nat-a", "nat-b"} {
		waitDo(t, c)(c.Addresses.Insert(proj, "us-central1", &computev1.Address{Name: n}).Do())
		a, err := c.Addresses.Get(proj, "us-central1", n).Do()
		if err != nil {
			t.Fatal(err)
		}
		ips = append(ips, a.Address)
	}
	waitDo(t, c)(c.Routers.Insert(proj, "us-central1", &computev1.Router{
		Name: "router", Network: "global/networks/vpc",
		Nats: []*computev1.RouterNat{{
			Name: "nat", NatIpAllocateOption: "MANUAL_ONLY", SourceSubnetworkIpRangesToNat: "ALL_SUBNETWORKS_ALL_IP_RANGES",
			NatIps: []string{"regions/us-central1/addresses/nat-a"}, DrainNatIps: []string{"regions/us-central1/addresses/nat-b"},
			MinPortsPerVm: 32768,
		}},
	}).Do())

	want := fmt.Sprintf("vm-0 10.30.0.2 %[1]s:1024-33791 32768|%[2]s:1024-33791 32768\nvm-1 10.30.0.3  0| 0\nvm-2 10.30.0.4  0| 0\n", ips[0], ips[1])
	if got := mappings(t, c); got != want {
		t.Fatalf("mapping info:\n%s\nwant:\n%s", got, want)
	}
	st, err := c.Routers.GetRouterStatus(proj, "us-central1", "router").Do()
	if err != nil {
		t.Fatal(err)
	}
	ns := st.Result.NatStatus[0]
	if ns.MinExtraNatIpsNeeded != 2 || ns.NumVmEndpointsWithNatMappings != 1 ||
		strings.Join(ns.UserAllocatedNatIps, ",") != ips[0] || strings.Join(ns.DrainUserAllocatedNatIps, ",") != ips[1] {
		t.Fatalf("status: %+v", ns)
	}

	// AUTO_ONLY: the NAT gains an IP for each workload's block.
	waitDo(t, c)(c.Routers.Patch(proj, "us-central1", "router", &computev1.Router{Nats: []*computev1.RouterNat{{
		Name: "nat", NatIpAllocateOption: "AUTO_ONLY", SourceSubnetworkIpRangesToNat: "ALL_SUBNETWORKS_ALL_IP_RANGES", MinPortsPerVm: 32768,
	}}}).Do())
	st, err = c.Routers.GetRouterStatus(proj, "us-central1", "router").Do()
	if err != nil {
		t.Fatal(err)
	}
	ns = st.Result.NatStatus[0]
	if ns.MinExtraNatIpsNeeded != 0 || ns.NumVmEndpointsWithNatMappings != 3 || len(ns.AutoAllocatedNatIps) != 3 {
		t.Fatalf("AUTO_ONLY status: %+v", ns)
	}
	got := mappings(t, c)
	for i, ip := range ns.AutoAllocatedNatIps {
		if line := fmt.Sprintf("vm-%d 10.30.0.%d %s:1024-33791 32768| 0\n", i, i+2, ip); !strings.Contains(got, line) {
			t.Fatalf("AUTO_ONLY mapping info:\n%s\nwant line %q", got, line)
		}
	}
}
