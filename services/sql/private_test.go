package sql_test

import (
	"context"
	"fmt"
	"math/rand/v2"
	"path"
	"strings"
	"testing"
	"time"

	computev1 "google.golang.org/api/compute/v1"
	"google.golang.org/api/option"
	snv1 "google.golang.org/api/servicenetworking/v1"
	sqladmin "google.golang.org/api/sqladmin/v1beta4"

	"github.com/linuxuser586/gcpemu/emutest"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/runtime"
)

// computeWaiter returns a function that waits for a compute operation.
func computeWaiter(t testing.TB, c *computev1.Service) func(*computev1.Operation, error) {
	return func(op *computev1.Operation, err error) { computeOp(t, c, op, err) }
}

func computeOp(t testing.TB, c *computev1.Service, op *computev1.Operation, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	for op.Status != "DONE" {
		switch {
		case op.Region != "":
			op, err = c.RegionOperations.Wait(testProject, path.Base(op.Region), op.Name).Do()
		default:
			op, err = c.GlobalOperations.Wait(testProject, op.Name).Do()
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if op.Error != nil {
		t.Fatalf("compute operation failed: %+v", op.Error.Errors[0])
	}
}

// TestPrivateIP covers FR-SQL-004 / FR-INT-008: an instance on a VPC
// with a private services access connection gets a PRIVATE address in the
// PSA range, reachable without authorized networks from a workload on the
// PSA network and from one on a VPC subnetwork; without the connection
// insert fails like in GCP.
func TestPrivateIP(t *testing.T) {
	emutest.RequireRuntime(t)
	t.Parallel()
	testPrivateIP(t, "")
}

// TestPrivateIPOverlap is TestPrivateIP on a PSA range that overlaps a
// network on the host, which a Docker network created first forces: the
// container network falls back to another range and the Declared
// addresses are aliased onto the containers (ADR 0004).
func TestPrivateIPOverlap(t *testing.T) {
	emutest.RequireRuntime(t)
	t.Parallel()
	ctx := context.Background()
	cl, err := runtime.Detect()
	if err != nil {
		t.Fatal(err)
	}
	rng := fmt.Sprintf("10.%d.%d.0/20", 160+rand.IntN(16), rand.IntN(16)*16)
	name := fmt.Sprintf("gcpemu-test-overlap-%d", rand.Uint32())
	if _, err := cl.CreateNetwork(ctx, runtime.NetworkSpec{Name: name, Subnet: rng}); err != nil {
		t.Logf("creating %s on %s: %v (the range is in use, which overlaps it too)", name, rng, err)
	} else {
		t.Cleanup(func() { _ = cl.RemoveNetwork(context.Background(), name) })
	}
	testPrivateIP(t, rng)
}

// testPrivateIP runs TestPrivateIP with the PSA range at psa ("" lets
// compute allocate it).
func testPrivateIP(t *testing.T, psa string) {
	inst := emutest.Start(t, []string{"sql", "compute"})
	ctx := context.Background()
	c, err := computev1.NewService(ctx, option.WithEndpoint(inst.GatewayURL()+"/compute/v1/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	sn, err := snv1.NewService(ctx, option.WithEndpoint(inst.GatewayURL()+"/servicenetworking/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	svc := adminClient(t, inst)
	cop := computeWaiter(t, c)

	cop(c.Networks.Insert(testProject, &computev1.Network{Name: "vpc", AutoCreateSubnetworks: false, ForceSendFields: []string{"AutoCreateSubnetworks"}}).Do())
	cop(c.Subnetworks.Insert(testProject, "us-central1", &computev1.Subnetwork{Name: "s", Network: "global/networks/vpc", IpCidrRange: "10.10.0.0/20"}).Do())

	private := &sqladmin.DatabaseInstance{
		Name: "priv", RootPassword: "rootpw",
		Settings: &sqladmin.Settings{IpConfiguration: &sqladmin.IpConfiguration{
			Ipv4Enabled: true, PrivateNetwork: "projects/" + testProject + "/global/networks/vpc",
		}},
	}
	if _, err := svc.Instances.Insert(testProject, private).Do(); err == nil || !strings.Contains(err.Error(), "private services access") {
		t.Fatalf("insert without a servicenetworking connection: %v", err)
	}

	alloc := &computev1.Address{Name: "psa", AddressType: "INTERNAL", Purpose: "VPC_PEERING", PrefixLength: 20, Network: "global/networks/vpc"}
	if psa != "" {
		alloc.Address = strings.TrimSuffix(psa, "/20")
	}
	cop(c.GlobalAddresses.Insert(testProject, alloc).Do())
	snOp, err := sn.Services.Connections.Create("services/servicenetworking.googleapis.com", &snv1.Connection{
		Network: "projects/" + testProject + "/global/networks/vpc", ReservedPeeringRanges: []string{"psa"},
	}).Do()
	if err != nil || !snOp.Done {
		t.Fatalf("servicenetworking: %+v %v", snOp, err)
	}

	in := createInstance(t, svc, private)
	var privIP, pubIP string
	for _, ip := range in.IpAddresses {
		switch ip.Type {
		case "PRIVATE":
			privIP = ip.IpAddress
		case "PRIMARY":
			pubIP = ip.IpAddress
		}
	}
	if pubIP == "" {
		t.Error("no PRIMARY address")
	}
	ga := must(c.GlobalAddresses.Get(testProject, "psa").Do())
	declared := ga.Address + "/20"
	cvc, _ := inst.Internal().Env.Lookup("compute")
	vpc := cvc.(emu.VPC)
	psaNet, err := vpc.PrivateServicesNetwork(ctx, "projects/"+testProject+"/global/networks/vpc")
	if err != nil {
		t.Fatal(err)
	}
	if psaNet.Subnet != declared {
		t.Skipf("PSA range %s overlaps a host network and compute could not translate it (container network on %s)", declared, psaNet.Subnet)
	}
	if psa != "" && !psaNet.Translated {
		t.Errorf("PSA range %s overlaps a host network but is not Translated (container network on %s)", declared, psaNet.ContainerSubnet)
	}
	if privIP == "" || !cidrHas(declared, privIP) {
		t.Fatalf("private IP %q not in the PSA range %s", privIP, declared)
	}
	rt, err := inst.Internal().Env.Containers.Runtime(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sqlContainer := "gcpemu-" + rt.InstanceID + "-sql-" + testProject + "-priv"
	// psql connects to the private IP from a workload and returns the
	// client address the instance sees (the Sidecar agent in front of
	// PostgreSQL holds the connection, so it is read from its socket).
	psql := func(what, id string) string {
		t.Helper()
		cmd := "PGPASSWORD=rootpw PGCONNECT_TIMEOUT=3 psql -h " + privIP + " -U postgres -d postgres -Atc "
		var out string
		for i := 0; ; i++ {
			res, err := rt.Exec(ctx, id, []string{"sh", "-c", cmd + "'SELECT 1' 2>&1"}, nil)
			if err != nil {
				t.Fatal(err)
			}
			out = strings.TrimSpace(string(res.Stdout))
			if res.ExitCode == 0 && out == "1" {
				break
			}
			if i == 50 {
				t.Fatalf("%s → private IP %s: %s", what, privIP, out)
			}
			time.Sleep(200 * time.Millisecond)
		}
		if _, err := rt.Exec(ctx, id, []string{"sh", "-c", cmd + "'SELECT pg_sleep(10)' >/dev/null 2>&1 &"}, nil); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 50; i++ {
			res, err := rt.Exec(ctx, sqlContainer, []string{"netstat", "-tn"}, nil)
			if err != nil {
				t.Fatal(err)
			}
			for _, l := range strings.Split(string(res.Stdout), "\n") {
				// The agent's socket is dual-stack: "::ffff:10.0.0.2:5432".
				f := strings.Fields(strings.ReplaceAll(l, "::ffff:", ""))
				if len(f) >= 6 && f[3] == privIP+":5432" && f[5] == "ESTABLISHED" {
					return f[4][:strings.LastIndex(f[4], ":")]
				}
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatalf("%s: no connection to %s:5432 seen in the instance", what, privIP)
		return ""
	}

	// A workload on the PSA network itself reaches the private IP
	// directly; the instance sees its address.
	peerIP, peer := vpcWorkload(t, inst, vpc, psaNet, "sql-test-peer", "")
	if got := psql("PSA workload", peer); got != peerIP {
		t.Errorf("the instance sees the PSA workload as %s, want %s", got, peerIP)
	}

	// FR-INT-008: a workload on a VPC subnetwork (as a GKE node/pod is,
	// default route via compute's NAT egress proxy) reaches the private IP.
	// The proxy relays it, and the instance sees an address of the VPC.
	sp := "projects/" + testProject + "/regions/us-central1/subnetworks/s"
	subnet, err := vpc.SubnetNetwork(ctx, sp)
	if err != nil {
		t.Fatal(err)
	}
	gw, err := vpc.EgressProxy(ctx, sp)
	if err != nil {
		t.Fatal(err)
	}
	_, pod := vpcWorkload(t, inst, vpc, subnet, "sql-test-pod", gw)
	if got := psql("subnetwork workload", pod); !cidrHas(declared, got) && !cidrHas(subnet.Subnet, got) {
		t.Errorf("the instance sees the subnetwork workload as %s, outside the VPC's ranges %s and %s", got, declared, subnet.Subnet)
	}
	// The instance's own default route stays on the external network.
	res, err := rt.Exec(ctx, sqlContainer, []string{"ip", "route", "show", "default"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ext, _ := rt.InspectNetwork(ctx, rt.Name("ext"))
	if !strings.Contains(string(res.Stdout), "via "+ext.Gateway+" ") {
		t.Errorf("instance default route = %q, want via %s (external network)", res.Stdout, ext.Gateway)
	}

	// After a restart of its container the instance keeps its address.
	waitOp(t, svc, must(svc.Instances.Restart(testProject, "priv").Do()))
	if got := psql("PSA workload after restart", peer); got != peerIP {
		t.Errorf("after restart the instance sees the PSA workload as %s, want %s", got, peerIP)
	}
	waitOp(t, svc, must(svc.Instances.Delete(testProject, "priv").Do()))
	assertNoRuntimeObjects(t, inst, "projects/"+testProject+"/instances/priv")
}

// vpcWorkload starts a container holding an address of net, as a Managed
// container would (aliased on a Translated network), with its default
// route via the NAT egress proxy at via when set. It returns the address and
// the container.
func vpcWorkload(t *testing.T, inst *emutest.Instance, vpc emu.VPC, net emu.SubnetNet, owner, via string) (string, string) {
	t.Helper()
	ctx := context.Background()
	ip, err := vpc.AllocateIP(ctx, net, owner)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = vpc.ReleaseIP(context.Background(), net, owner) })
	rt, err := inst.Internal().Env.Containers.Runtime(ctx)
	if err != nil {
		t.Fatal(err)
	}
	a := runtime.Attachment{Network: net.Name, IP: ip}
	if net.Translated {
		a.IP = ""
	}
	id, err := rt.CreateContainer(ctx, runtime.ContainerSpec{
		Name:       rt.Name("sql-test", owner),
		Image:      "postgres:17.10-alpine",
		Entrypoint: []string{"sleep", "3600"},
		Labels:     rt.Labels("sql-test", "", "probe"),
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
	case net.Translated && via != "":
		script = net.AliasScript(ip, ct.IPs[net.Name], "default")
	case net.Translated:
		script = net.AliasScript(ip, ct.IPs[net.Name])
	case via != "":
		script = "ip route replace default via " + via
	}
	if script != "" {
		res, err := rt.Exec(ctx, id, []string{"sh", "-c", script}, nil)
		if err != nil || res.ExitCode != 0 {
			t.Fatalf("network setup of %s: %v %s%s", owner, err, res.Stdout, res.Stderr)
		}
	}
	return ip, id
}
