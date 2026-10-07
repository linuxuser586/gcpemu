package compute_test

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	computev1 "google.golang.org/api/compute/v1"
	"google.golang.org/api/option"
	snv1 "google.golang.org/api/servicenetworking/v1"

	"github.com/linuxuser586/gcpemu/emutest"
	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/runtime"
	"github.com/linuxuser586/gcpemu/internal/store"
)

const probeImage = "rancher/k3s:v1.36.5-k3s1"

// workload starts a container on net with its default route via gw, as a
// private GKE node would be.
func workload(t *testing.T, inst *emutest.Instance, net emu.SubnetNet, ip, gw string) (*runtime.Manager, string) {
	t.Helper()
	ctx := context.Background()
	rt, err := inst.Env.Containers.Runtime(ctx)
	if err != nil {
		t.Fatal(err)
	}
	id, err := rt.CreateContainer(ctx, runtime.ContainerSpec{
		Name:       rt.Name("test", "vm", ip),
		Image:      probeImage,
		Entrypoint: []string{"sh", "-c", "ip route replace default via " + gw + " && exec sleep 3600"},
		Labels:     rt.Labels("compute-test", "", "probe"),
		CapAdd:     []string{"NET_ADMIN"},
		Networks:   []runtime.Attachment{{Network: net.Name, IP: ip}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rt.RemoveContainer(context.Background(), id, true) })
	if err := rt.StartContainer(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := rt.WaitRunning(ctx, id); err != nil {
		t.Fatal(err)
	}
	return rt, id
}

// fetch runs an HTTP request to an internet IP from the workload and
// returns wget's output (server headers on success).
func fetch(t *testing.T, rt *runtime.Manager, id string) string {
	t.Helper()
	res, err := rt.Exec(context.Background(), id, []string{"sh", "-c", "wget -S -O /dev/null -T 3 http://1.1.1.1/ 2>&1; true"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return string(res.Stdout) + string(res.Stderr)
}

func connected(out string) bool { return strings.Contains(out, "HTTP/1.") }

// eventually polls cond for up to 20s.
func eventually(t *testing.T, what string, cond func() (bool, string)) {
	t.Helper()
	var last string
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(300 * time.Millisecond) {
		ok, out := cond()
		if ok {
			return
		}
		last = out
	}
	t.Fatalf("%s: timed out; last output: %s", what, last)
}

func natRouter(t *testing.T, c *computev1.Service) {
	t.Helper()
	waitDo(t, c)(c.Routers.Insert(proj, "us-central1", &computev1.Router{
		Name: "router", Network: "global/networks/vpc",
		Nats: []*computev1.RouterNat{{
			Name: "nat", NatIpAllocateOption: "AUTO_ONLY", SourceSubnetworkIpRangesToNat: "ALL_SUBNETWORKS_ALL_IP_RANGES",
			LogConfig: &computev1.RouterNatLogConfig{Enable: true, Filter: "ALL"},
		}},
	}).Do())
}

// TestEgressThroughNAT is FR-NAT-002 / FR-INT-009: a workload on a
// private subnetwork reaches the internet only while a Cloud NAT covers it.
func TestEgressThroughNAT(t *testing.T) {
	emutest.RequireRuntime(t)
	t.Parallel()
	inst, c, _ := newClients(t, func(cfg *config.Config) { cfg.NATReject = true })
	customNet(t, c, "vpc")
	subnet(t, c, "vpc", "nodes", "us-central1", "10.123.0.0/24", &computev1.SubnetworkSecondaryRange{RangeName: "pods", IpCidrRange: "10.124.0.0/16"})
	svc, _ := inst.Env.Lookup("compute")
	vpc := svc.(emu.VPC)
	ctx := context.Background()

	sp, err := vpc.ResolveSubnetwork(ctx, proj, "us-central1", "vpc", "")
	if err != nil || sp != "projects/"+proj+"/regions/us-central1/subnetworks/nodes" {
		t.Fatalf("resolve: %q %v", sp, err)
	}
	net, err := vpc.SubnetNetwork(ctx, sp)
	if err != nil {
		t.Fatal(err)
	}
	if net.SecondaryRanges["pods"] != "10.124.0.0/16" || net.NetworkName != "projects/"+proj+"/global/networks/vpc" {
		t.Fatalf("subnet net: %+v", net)
	}
	gw, err := vpc.EgressGateway(ctx, sp)
	if err != nil {
		t.Fatal(err)
	}
	ip, err := vpc.AllocateIP(ctx, net, "vm-1")
	if err != nil {
		t.Fatal(err)
	}
	if again, _ := vpc.AllocateIP(ctx, net, "vm-1"); again != ip {
		t.Fatalf("AllocateIP not idempotent: %s vs %s", ip, again)
	}
	if !net2(net.Subnet, ip, gw) {
		t.Fatalf("ip %s / gateway %s not in %s", ip, gw, net.Subnet)
	}
	rt, id := workload(t, inst, net, ip, gw)

	if out := fetch(t, rt, id); connected(out) {
		t.Fatalf("egress without NAT must fail: %s", out)
	}
	natRouter(t, c)
	eventually(t, "egress with NAT", func() (bool, string) { out := fetch(t, rt, id); return connected(out), out })

	// Port reporting (FR-NAT-003).
	m, err := c.Routers.GetNatMappingInfo(proj, "us-central1", "router").Do()
	if err != nil || len(m.Result) != 1 || m.Result[0].InstanceName != "vm-1" || m.Result[0].InterfaceNatMappings[0].SourceVirtualIp != ip {
		t.Fatalf("mapping info: %+v %v", m, err)
	}
	// The subnetwork is in use while a workload holds an address on it.
	if _, err := c.Subnetworks.Delete(proj, "us-central1", "nodes").Do(); reason(err) != "resourceInUseByAnotherResource" {
		t.Fatalf("subnet in use by workload: %v", err)
	}

	waitDo(t, c)(c.Routers.Delete(proj, "us-central1", "router").Do())
	eventually(t, "egress after NAT deletion", func() (bool, string) { out := fetch(t, rt, id); return !connected(out), out })

	if err := vpc.ReleaseIP(ctx, net, "vm-1"); err != nil {
		t.Fatal(err)
	}
}

// net2 reports whether every ip lies in cidr.
func net2(cidr string, ips ...string) bool {
	_, n, err := net.ParseCIDR(cidr)
	if err != nil {
		return false
	}
	for _, ip := range ips {
		if !n.Contains(net.ParseIP(ip)) {
			return false
		}
	}
	return true
}

// TestOfflineSink is FR-NAT-005: with --offline, NAT-allowed egress is
// answered by the sink and recorded.
func TestOfflineSink(t *testing.T) {
	emutest.RequireRuntime(t)
	t.Parallel()
	inst, c, _ := newClients(t, func(cfg *config.Config) {
		cfg.Offline = true
		cfg.NATSinkResponse = "418:offline sink"
	})
	customNet(t, c, "vpc")
	subnet(t, c, "vpc", "nodes", "us-central1", "10.125.0.0/24")
	svc, _ := inst.Env.Lookup("compute")
	vpc := svc.(emu.VPC)
	ctx := context.Background()
	sp := "projects/" + proj + "/regions/us-central1/subnetworks/nodes"
	net, err := vpc.SubnetNetwork(ctx, sp)
	if err != nil {
		t.Fatal(err)
	}
	gw, err := vpc.EgressGateway(ctx, sp)
	if err != nil {
		t.Fatal(err)
	}
	ip, _ := vpc.AllocateIP(ctx, net, "vm-offline")
	rt, id := workload(t, inst, net, ip, gw)
	natRouter(t, c)
	eventually(t, "sink answers", func() (bool, string) {
		out := fetch(t, rt, id)
		return strings.Contains(out, "418 I'm a teapot"), out
	})
	eventually(t, "sink attempt recorded", func() (bool, string) {
		var n int
		var last string
		_ = inst.Env.Store.View(func(tx store.Tx) error {
			tx.Scan("compute/natsink", "", func(_ string, v []byte) bool { n++; last = string(v); return true })
			return nil
		})
		return n > 0 && strings.Contains(last, "1.1.1.1:80") && strings.Contains(last, "vm-offline"), last
	})
}

// TestSubnetOverlapFallback: two instances realising the same range on one
// host; the second gets a runtime-chosen range (FR-CORE-033).
func TestSubnetOverlapFallback(t *testing.T) {
	emutest.RequireRuntime(t)
	t.Parallel()
	ctx := context.Background()
	var nets []emu.SubnetNet
	for i := 0; i < 2; i++ {
		inst, c, _ := newClients(t)
		customNet(t, c, "vpc")
		subnet(t, c, "vpc", "s", "us-central1", "10.126.0.0/24")
		svc, _ := inst.Env.Lookup("compute")
		vpc := svc.(emu.VPC)
		n, err := vpc.SubnetNetwork(ctx, "projects/"+proj+"/regions/us-central1/subnetworks/s")
		if err != nil {
			t.Fatal(err)
		}
		// Idempotent.
		if again, err := vpc.SubnetNetwork(ctx, "projects/"+proj+"/regions/us-central1/subnetworks/s"); err != nil || again.Name != n.Name || again.Subnet != n.Subnet {
			t.Fatalf("not idempotent: %+v %+v %v", n, again, err)
		}
		ip, err := vpc.AllocateIP(ctx, n, "x")
		if err != nil || strings.HasSuffix(ip, ".0") || strings.HasSuffix(ip, ".1") {
			t.Fatalf("allocate: %s %v", ip, err)
		}
		nets = append(nets, n)
	}
	if nets[0].Name == nets[1].Name {
		t.Fatalf("instances share a network name: %s", nets[0].Name)
	}
	if nets[0].Subnet == nets[1].Subnet {
		t.Fatalf("both instances use %s", nets[0].Subnet)
	}
	if nets[0].Subnet != "10.126.0.0/24" && nets[1].Subnet != "10.126.0.0/24" {
		t.Logf("neither instance got the requested range (host conflict): %s, %s", nets[0].Subnet, nets[1].Subnet)
	}
}

// TestIntraVPCRouting: workloads in different subnetworks and on the
// private services access range reach each other through the gateway
// (VPC implied routes), e.g. GKE pods → Cloud SQL private IP.
func TestIntraVPCRouting(t *testing.T) {
	emutest.RequireRuntime(t)
	t.Parallel()
	inst, c, sn := newClients(t)
	customNet(t, c, "vpc")
	subnet(t, c, "vpc", "a", "us-central1", "10.127.0.0/24")
	subnet(t, c, "vpc", "b", "europe-west1", "10.127.1.0/24")
	waitDo(t, c)(c.GlobalAddresses.Insert(proj, &computev1.Address{Name: "psa", AddressType: "INTERNAL", Purpose: "VPC_PEERING", PrefixLength: 24, Address: "10.127.8.0", Network: "global/networks/vpc"}).Do())
	if _, err := sn.Services.Connections.Create("services/servicenetworking.googleapis.com", &snv1.Connection{
		Network: "projects/" + proj + "/global/networks/vpc", ReservedPeeringRanges: []string{"psa"}}).Do(); err != nil {
		t.Fatal(err)
	}
	svc, _ := inst.Env.Lookup("compute")
	vpc := svc.(emu.VPC)
	ctx := context.Background()
	start := func(sp, owner string) (*runtime.Manager, string, string) {
		net, err := vpc.SubnetNetwork(ctx, sp)
		if err != nil {
			t.Fatal(err)
		}
		gw, err := vpc.EgressGateway(ctx, sp)
		if err != nil {
			t.Fatal(err)
		}
		ip, err := vpc.AllocateIP(ctx, net, owner)
		if err != nil {
			t.Fatal(err)
		}
		rt, id := workload(t, inst, net, ip, gw)
		return rt, id, ip
	}
	rt, a, _ := start("projects/"+proj+"/regions/us-central1/subnetworks/a", "vm-a")
	_, _, ipB := start("projects/"+proj+"/regions/europe-west1/subnetworks/b", "vm-b")

	// A Cloud SQL-like container on the PSA network keeps the runtime's
	// default route (it does not know about the gateway).
	psa, err := vpc.PrivateServicesNetwork(ctx, "projects/"+proj+"/global/networks/vpc")
	if err != nil {
		t.Fatal(err)
	}
	if psa.Subnet != "10.127.8.0/24" && !strings.HasPrefix(psa.Subnet, "10.24") {
		t.Logf("psa range fell back to %s", psa.Subnet)
	}
	ipDB, err := vpc.AllocateIP(ctx, psa, "sql-instance")
	if err != nil {
		t.Fatal(err)
	}
	id, err := rt.CreateContainer(ctx, runtime.ContainerSpec{
		Name: rt.Name("test", "db"), Image: probeImage, Entrypoint: []string{"sleep", "3600"},
		Labels: rt.Labels("compute-test", "", "probe"), Networks: []runtime.Attachment{{Network: psa.Name, IP: ipDB}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rt.RemoveContainer(context.Background(), id, true) })
	if err := rt.StartContainer(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := vpc.EgressGateway(ctx, "projects/"+proj+"/regions/us-central1/subnetworks/a"); err != nil {
		t.Fatal(err) // re-sync attaches the gateway to the PSA network
	}
	for _, target := range []string{ipB, ipDB} {
		eventually(t, "ping "+target, func() (bool, string) {
			res, err := rt.Exec(ctx, a, []string{"ping", "-c1", "-W2", target}, nil)
			if err != nil {
				t.Fatal(err)
			}
			return res.ExitCode == 0, string(res.Stdout) + string(res.Stderr)
		})
	}
}

// TestNetworksSurviveRestart: with --data-dir, container networks are
// adopted again after a restart (FR-CORE-031).
func TestNetworksSurviveRestart(t *testing.T) {
	emutest.RequireRuntime(t)
	t.Parallel()
	dir := t.TempDir()
	persistent := func(cfg *config.Config) { cfg.Ephemeral = false; cfg.DataDir = dir }
	ctx := context.Background()
	sp := "projects/" + proj + "/regions/us-central1/subnetworks/s"
	var first emu.SubnetNet
	var firstIP string
	{
		inst := emutest.Start(t, []string{"compute"}, persistent)
		c, _ := computev1.NewService(ctx, option.WithEndpoint(inst.GatewayURL()+"/compute/v1/"), option.WithoutAuthentication())
		customNet(t, c, "vpc")
		subnet(t, c, "vpc", "s", "us-central1", "10.122.0.0/24")
		svc, _ := inst.Env.Lookup("compute")
		var err error
		if first, err = svc.(emu.VPC).SubnetNetwork(ctx, sp); err != nil {
			t.Fatal(err)
		}
		firstIP, _ = svc.(emu.VPC).AllocateIP(ctx, first, "vm")
		rt, _ := inst.Env.Containers.Runtime(ctx)
		t.Cleanup(func() { _ = rt.RemoveNetwork(context.Background(), first.Name) })
		sctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		if err := inst.Shutdown(sctx); err != nil {
			t.Fatal(err)
		}
	}
	inst := emutest.Start(t, []string{"compute"}, persistent)
	svc, _ := inst.Env.Lookup("compute")
	again, err := svc.(emu.VPC).SubnetNetwork(ctx, sp)
	if err != nil {
		t.Fatal(err)
	}
	if again.Name != first.Name || again.Subnet != first.Subnet {
		t.Fatalf("network changed across restart: %+v vs %+v", first, again)
	}
	if ip, _ := svc.(emu.VPC).AllocateIP(ctx, again, "vm"); ip != firstIP {
		t.Fatalf("allocation lost: %s vs %s", ip, firstIP)
	}
}
