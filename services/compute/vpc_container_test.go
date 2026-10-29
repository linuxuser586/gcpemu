package compute_test

import (
	"context"
	"fmt"
	"math/rand/v2"
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
	"github.com/linuxuser586/gcpemu/internal/emutestx"
	"github.com/linuxuser586/gcpemu/internal/runtime"
	"github.com/linuxuser586/gcpemu/internal/store"
	"github.com/linuxuser586/gcpemu/services/compute"
)

const probeImage = "rancher/k3s:v1.36.5-k3s1"

// workload starts a container holding ip on net, as a Managed container
// would (aliased on a Translated network), with its default route via the
// NAT egress proxy at gw when set, as a private GKE node would be.
func workload(t *testing.T, inst *emutest.Instance, net emu.SubnetNet, ip, gw string) (*runtime.Manager, string) {
	t.Helper()
	ctx := context.Background()
	rt, err := inst.Internal().Env.Containers.Runtime(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Creating a container doesn't pull its image, and a fresh runner may
	// not have run anything that does.
	if err := rt.EnsureImage(ctx, probeImage, false); err != nil {
		t.Fatal(err)
	}
	a := runtime.Attachment{Network: net.Name, IP: ip}
	if net.Translated {
		a.IP = ""
	}
	id, err := rt.CreateContainer(ctx, runtime.ContainerSpec{
		Name:       rt.Name("test", "vm", ip),
		Image:      probeImage,
		Entrypoint: []string{"sleep", "3600"},
		Labels:     rt.Labels("compute-test", "", "probe"),
		CapAdd:     []string{"NET_ADMIN"},
		Networks:   []runtime.Attachment{a},
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
	ct, err := rt.InspectContainer(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	var script string
	switch {
	case net.Translated && gw != "":
		script = net.AliasScript(ip, ct.IPs[net.Name], "default")
	case net.Translated:
		script = net.AliasScript(ip, ct.IPs[net.Name])
	case gw != "":
		script = "ip route replace default via " + gw
	}
	if script != "" {
		res, err := rt.Exec(ctx, id, []string{"sh", "-c", script}, nil)
		if err != nil || res.ExitCode != 0 {
			t.Fatalf("network setup of %s: %v %s%s", ip, err, res.Stdout, res.Stderr)
		}
	}
	return rt, id
}

// overlap creates a Docker network on rng and holds it for the rest of
// the test, so that a VPC range realised on it afterwards falls back to
// another range. While rng is in use by another network, which may be
// freed at any time, it retries for up to 30s, then skips the test.
func overlap(t *testing.T, rng string) {
	t.Helper()
	ctx := context.Background()
	cl, err := runtime.Detect()
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("gcpemu-test-overlap-%d", rand.Uint32())
	deadline := time.Now().Add(30 * time.Second)
	for {
		_, err := cl.CreateNetwork(ctx, runtime.NetworkSpec{Name: name, Subnet: rng})
		if err == nil {
			break
		}
		if !strings.Contains(strings.ToLower(err.Error()), "overlap") {
			t.Fatalf("creating %s on %s: %v", name, rng, err)
		}
		if time.Now().After(deadline) {
			t.Skipf("range %s stayed in use by another network for 30s: %v", rng, err)
		}
		time.Sleep(100*time.Millisecond + rand.N(400*time.Millisecond))
	}
	t.Cleanup(func() { _ = cl.RemoveNetwork(context.Background(), name) })
}

// translated fails unless net, whose Declared range rng overlaps a host
// network, is Translated, and skips when compute could not translate it.
func translated(t *testing.T, net emu.SubnetNet, rng string) {
	t.Helper()
	if net.Subnet != rng {
		t.Skipf("range %s overlaps a host network and compute could not translate it (container network on %s)", rng, net.Subnet)
	}
	if !net.Translated || net.ContainerSubnet == rng {
		t.Fatalf("range %s overlaps a host network but is not Translated: %+v", rng, net)
	}
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
	testEgressThroughNAT(t, "10.123.0.0/24", false)
}

// TestEgressThroughNATTranslated is TestEgressThroughNAT on a subnetwork
// whose range overlaps a host network (ADR 0004): the workload sends from
// its Declared address, which the NAT covers and attributes.
func TestEgressThroughNATTranslated(t *testing.T) {
	emutest.RequireRuntime(t)
	t.Parallel()
	rng := fmt.Sprintf("10.%d.%d.0/24", 176+rand.IntN(16), rand.IntN(256))
	overlap(t, rng)
	testEgressThroughNAT(t, rng, true)
}

func testEgressThroughNAT(t *testing.T, rng string, overlapped bool) {
	inst, c, _ := newClients(t, emutest.WithConfig(func(cfg *config.Config) { cfg.NATReject = true }))
	customNet(t, c, "vpc")
	subnet(t, c, "vpc", "nodes", "us-central1", rng, &computev1.SubnetworkSecondaryRange{RangeName: "pods", IpCidrRange: "10.124.0.0/16"})
	svc, _ := inst.Internal().Env.Lookup("compute")
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
	if overlapped {
		translated(t, net, rng)
	}
	gw, err := vpc.EgressProxy(ctx, sp)
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
	if !net2(net.Subnet, ip) || !net2(net.ContainerSubnet, gw) {
		t.Fatalf("ip %s not in %s or NAT egress proxy %s not in %s", ip, net.Subnet, gw, net.ContainerSubnet)
	}
	rt, id := workload(t, inst, net, ip, gw)

	if out := fetch(t, rt, id); connected(out) {
		t.Fatalf("egress without NAT must fail: %s", out)
	}
	// The proxy reports the refused connection (FR-NAT-004 drop events).
	egressLogged(t, inst, "DROPPED", "projects/"+proj+"/global/networks/vpc")
	natRouter(t, c)
	eventually(t, "egress with NAT", func() (bool, string) { out := fetch(t, rt, id); return connected(out), out })
	natLogged(t, inst, "OK")
	emutestx.RequireRelayStream(t, inst, "NAT events", "gateway")

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
	// Offline, the images must already be present: pull them first, as
	// a fresh runner may not have run anything that does.
	cl, err := runtime.Detect()
	if err != nil {
		t.Fatal(err)
	}
	for _, img := range compute.Images {
		if err := cl.EnsureImage(context.Background(), img, false); err != nil {
			t.Fatal(err)
		}
	}
	inst, c, _ := newClients(t, emutest.WithConfig(func(cfg *config.Config) {
		cfg.Offline = true
		cfg.NATSinkResponse = "418:offline sink"
	}))
	customNet(t, c, "vpc")
	subnet(t, c, "vpc", "nodes", "us-central1", "10.125.0.0/24")
	svc, _ := inst.Internal().Env.Lookup("compute")
	vpc := svc.(emu.VPC)
	ctx := context.Background()
	sp := "projects/" + proj + "/regions/us-central1/subnetworks/nodes"
	net, err := vpc.SubnetNetwork(ctx, sp)
	if err != nil {
		t.Fatal(err)
	}
	gw, err := vpc.EgressProxy(ctx, sp)
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
		_ = inst.Internal().Env.Store.View(func(tx store.Tx) error {
			tx.Scan("compute/natsink", "", func(_ string, v []byte) bool { n++; last = string(v); return true })
			return nil
		})
		return n > 0 && strings.Contains(last, "1.1.1.1:80") && strings.Contains(last, "vm-offline"), last
	})
	natLogged(t, inst, "OFFLINE_SINK")
}

// natLogged waits for an egress entry with code in the request log
// (FR-CORE-061), attributed to the test's Cloud NAT gateway.
func natLogged(t *testing.T, inst *emutest.Instance, code string) {
	t.Helper()
	egressLogged(t, inst, code, "projects/"+proj+"/regions/us-central1/routers/router/nats/nat")
}

// egressLogged waits for an egress entry with code attributed to resource.
func egressLogged(t *testing.T, inst *emutest.Instance, code, resource string) {
	t.Helper()
	eventually(t, "nat request log entry "+code, func() (bool, string) {
		es := inst.Internal().Env.RequestLog.Entries("nat")
		for _, e := range es {
			if e.Code == code && e.Resource == resource &&
				strings.HasPrefix(e.Method, "EGRESS ") && strings.Contains(e.Method, " -> ") {
				return true, ""
			}
		}
		return false, fmt.Sprint(es)
	})
}

// TestSubnetOverlapFallback: two instances realising the same range on one
// host; the second's container network gets another range (FR-CORE-033),
// while both allocate addresses from the Declared range.
func TestSubnetOverlapFallback(t *testing.T) {
	emutest.RequireRuntime(t)
	t.Parallel()
	ctx := context.Background()
	var nets []emu.SubnetNet
	for i := 0; i < 2; i++ {
		inst, c, _ := newClients(t)
		customNet(t, c, "vpc")
		subnet(t, c, "vpc", "s", "us-central1", "10.126.0.0/24")
		svc, _ := inst.Internal().Env.Lookup("compute")
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
	if nets[0].ContainerSubnet == nets[1].ContainerSubnet {
		t.Fatalf("both instances' container networks use %s", nets[0].ContainerSubnet)
	}
	if nets[0].ContainerSubnet != "10.126.0.0/24" && nets[1].ContainerSubnet != "10.126.0.0/24" {
		t.Logf("neither instance got the requested range (host conflict): %s, %s", nets[0].ContainerSubnet, nets[1].ContainerSubnet)
	}
	// Both report the Declared range (ADR 0004).
	for _, n := range nets {
		if n.ContainerSubnet != "10.126.0.0/24" {
			translated(t, n, "10.126.0.0/24")
		}
	}
}

// TestIntraVPCRouting: workloads in different subnetworks and on the
// private services access range reach each other through the NAT egress proxy
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
	svc, _ := inst.Internal().Env.Lookup("compute")
	vpc := svc.(emu.VPC)
	ctx := context.Background()
	start := func(sp, owner string) (*runtime.Manager, string, string) {
		net, err := vpc.SubnetNetwork(ctx, sp)
		if err != nil {
			t.Fatal(err)
		}
		gw, err := vpc.EgressProxy(ctx, sp)
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
	// default route (it does not know about the proxy).
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
	workload(t, inst, psa, ipDB, "")
	if _, err := vpc.EgressProxy(ctx, "projects/"+proj+"/regions/us-central1/subnetworks/a"); err != nil {
		t.Fatal(err) // re-sync attaches the proxy to the PSA network
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
	persistent := emutest.WithTestDataDir(t, dir)
	ctx := context.Background()
	sp := "projects/" + proj + "/regions/us-central1/subnetworks/s"
	var first emu.SubnetNet
	var firstIP string
	{
		inst := emutest.Start(t, []string{"compute"}, persistent)
		c, _ := computev1.NewService(ctx, option.WithEndpoint(inst.GatewayURL()+"/compute/v1/"), option.WithoutAuthentication())
		customNet(t, c, "vpc")
		subnet(t, c, "vpc", "s", "us-central1", "10.122.0.0/24")
		svc, _ := inst.Internal().Env.Lookup("compute")
		var err error
		if first, err = svc.(emu.VPC).SubnetNetwork(ctx, sp); err != nil {
			t.Fatal(err)
		}
		firstIP, _ = svc.(emu.VPC).AllocateIP(ctx, first, "vm")
		rt, _ := inst.Internal().Env.Containers.Runtime(ctx)
		t.Cleanup(func() { _ = rt.RemoveNetwork(context.Background(), first.Name) })
		sctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		if err := inst.Internal().Shutdown(sctx); err != nil {
			t.Fatal(err)
		}
	}
	inst := emutest.Start(t, []string{"compute"}, persistent)
	svc, _ := inst.Internal().Env.Lookup("compute")
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

// TestTranslatedNetworkSurvivesRestart is ADR 0004 across a restart: a
// subnetwork whose range overlaps a host network allocates its Declared
// addresses around internal addresses reserved through the compute API,
// and keeps them when its container network is recreated on another
// Fallback range.
func TestTranslatedNetworkSurvivesRestart(t *testing.T) {
	emutest.RequireRuntime(t)
	t.Parallel()
	o2, o3 := 192+rand.IntN(16), rand.IntN(256)
	rng := fmt.Sprintf("10.%d.%d.0/24", o2, o3)
	reserved := fmt.Sprintf("10.%d.%d.2", o2, o3)
	overlap(t, rng)
	dir := t.TempDir()
	persistent := emutest.WithTestDataDir(t, dir)
	ctx := context.Background()
	sp := "projects/" + proj + "/regions/us-central1/subnetworks/s"
	var first emu.SubnetNet
	var firstIP string
	{
		inst := emutest.Start(t, []string{"compute"}, persistent)
		c, _ := computev1.NewService(ctx, option.WithEndpoint(inst.GatewayURL()+"/compute/v1/"), option.WithoutAuthentication())
		customNet(t, c, "vpc")
		subnet(t, c, "vpc", "s", "us-central1", rng)
		waitDo(t, c)(c.Addresses.Insert(proj, "us-central1", &computev1.Address{Name: "res", AddressType: "INTERNAL",
			Subnetwork: "regions/us-central1/subnetworks/s", Address: reserved}).Do())
		svc, _ := inst.Internal().Env.Lookup("compute")
		var err error
		if first, err = svc.(emu.VPC).SubnetNetwork(ctx, sp); err != nil {
			t.Fatal(err)
		}
		translated(t, first, rng)
		if firstIP, err = svc.(emu.VPC).AllocateIP(ctx, first, "vm"); err != nil {
			t.Fatal(err)
		}
		if firstIP == reserved || !net2(rng, firstIP) {
			t.Fatalf("allocated %s: want an address in %s other than the reserved %s", firstIP, rng, reserved)
		}
		sctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		if err := inst.Internal().Shutdown(sctx); err != nil {
			t.Fatal(err)
		}
	}
	// The container network is recreated elsewhere: its old range is taken.
	cl, err := runtime.Detect()
	if err != nil {
		t.Fatal(err)
	}
	if err := cl.RemoveNetwork(ctx, first.Name); err != nil {
		t.Fatal(err)
	}
	overlap(t, first.ContainerSubnet)
	inst := emutest.Start(t, []string{"compute"}, persistent)
	rt, _ := inst.Internal().Env.Containers.Runtime(ctx)
	svc, _ := inst.Internal().Env.Lookup("compute")
	again, err := svc.(emu.VPC).SubnetNetwork(ctx, sp)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rt.RemoveNetwork(context.Background(), again.Name) })
	if !again.Translated || again.Subnet != rng || again.ContainerSubnet == first.ContainerSubnet {
		t.Fatalf("after recreation: %+v (first on %s)", again, first.ContainerSubnet)
	}
	if ip, _ := svc.(emu.VPC).AllocateIP(ctx, again, "vm"); ip != firstIP {
		t.Fatalf("allocation changed: %s vs %s", ip, firstIP)
	}
}
