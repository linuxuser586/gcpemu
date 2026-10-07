package sql_test

import (
	"context"
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
// with a private services access connection gets a PRIVATE address on
// compute's private services network, reachable without authorized
// networks, also from a workload on a VPC subnetwork; without the
// connection insert fails like in GCP.
func TestPrivateIP(t *testing.T) {
	emutest.RequireRuntime(t)
	t.Parallel()
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

	cop(c.GlobalAddresses.Insert(testProject, &computev1.Address{Name: "psa", AddressType: "INTERNAL", Purpose: "VPC_PEERING", PrefixLength: 20, Network: "global/networks/vpc"}).Do())
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
	if privIP == "" || !cidrHas(ga.Address+"/20", privIP) {
		t.Fatalf("private IP %q not in the PSA range %s/20", privIP, ga.Address)
	}
	conn, err := connect(ctx, privIP, 5432, "postgres", "rootpw", "postgres")
	if err != nil {
		t.Fatalf("connect to private IP: %v", err)
	}
	conn.Close(ctx)

	// FR-INT-008: a workload on a VPC subnetwork (as a GKE node/pod is,
	// default route via compute's gateway) reaches the private IP.
	cvc, _ := inst.Env.Lookup("compute")
	vpc := cvc.(emu.VPC)
	sp := "projects/" + testProject + "/regions/us-central1/subnetworks/s"
	subnet, err := vpc.SubnetNetwork(ctx, sp)
	if err != nil {
		t.Fatal(err)
	}
	gw, err := vpc.EgressGateway(ctx, sp)
	if err != nil {
		t.Fatal(err)
	}
	podIP, err := vpc.AllocateIP(ctx, subnet, "sql-test-pod")
	if err != nil {
		t.Fatal(err)
	}
	rt, err := inst.Env.Containers.Runtime(ctx)
	if err != nil {
		t.Fatal(err)
	}
	pod, err := rt.CreateContainer(ctx, runtime.ContainerSpec{
		Name:       rt.Name("sql-test", "pod"),
		Image:      "postgres:17.10-alpine",
		Entrypoint: []string{"sh", "-c", "ip route replace default via " + gw + " && exec sleep 3600"},
		Labels:     rt.Labels("sql-test", "", "probe"),
		CapAdd:     []string{"NET_ADMIN"},
		Networks:   []runtime.Attachment{{Network: subnet.Name, IP: podIP}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rt.RemoveContainer(context.Background(), pod, true) })
	if err := rt.StartContainer(ctx, pod); err != nil {
		t.Fatal(err)
	}
	if err := rt.WaitRunning(ctx, pod); err != nil {
		t.Fatal(err)
	}
	var out string
	for i := 0; i < 50; i++ {
		res, err := rt.Exec(ctx, pod, []string{"sh", "-c", "PGPASSWORD=rootpw PGCONNECT_TIMEOUT=3 psql -h " + privIP + " -U postgres -d postgres -Atc 'SELECT inet_client_addr() IS NOT NULL' 2>&1"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		out = string(res.Stdout)
		if res.ExitCode == 0 && strings.TrimSpace(out) == "t" {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if strings.TrimSpace(out) != "t" {
		t.Fatalf("pod → private IP: %s", out)
	}
	// The instance's own default route stays on the external network.
	res, err := rt.Exec(ctx, "gcpemu-"+rt.InstanceID+"-sql-"+testProject+"-priv", []string{"ip", "route", "show", "default"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ext, _ := rt.InspectNetwork(ctx, rt.Name("ext"))
	if !strings.Contains(string(res.Stdout), "via "+ext.Gateway+" ") {
		t.Errorf("instance default route = %q, want via %s (external network)", res.Stdout, ext.Gateway)
	}
	waitOp(t, svc, must(svc.Instances.Delete(testProject, "priv").Do()))
	assertNoRuntimeObjects(t, inst, "projects/"+testProject+"/instances/priv")
}
