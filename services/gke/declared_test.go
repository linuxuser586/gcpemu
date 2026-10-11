package gke_test

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net"
	"path"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/container/apiv1/containerpb"
	computev1 "google.golang.org/api/compute/v1"
	"google.golang.org/api/option"
	snv1 "google.golang.org/api/servicenetworking/v1"
	sqladmin "google.golang.org/api/sqladmin/v1beta4"

	"github.com/linuxuser586/gcpemu/emutest"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/runtime"
)

// TestDeclaredAddressesOverlap is ADR 0004 with GKE: the cluster's
// subnetwork and the VPC's private services access range overlap networks
// on the host (Docker networks created first), so their container networks
// fall back to other ranges. The control plane, the node (as its
// Kubernetes InternalIP) and the Cloud SQL instance still hold addresses
// in the Declared ranges, and a pod reaches the instance's private IP
// (FR-SQL-004, FR-INT-008).
func TestDeclaredAddressesOverlap(t *testing.T) {
	emutest.RequireRuntime(t)
	t.Parallel()
	ctx := context.Background()
	cl, err := runtime.Detect()
	if err != nil {
		t.Fatal(err)
	}
	block := func(rng string) {
		name := fmt.Sprintf("gcpemu-test-overlap-%d", rand.Uint32())
		if _, err := cl.CreateNetwork(ctx, runtime.NetworkSpec{Name: name, Subnet: rng}); err != nil {
			t.Logf("creating %s on %s: %v (the range is in use, which overlaps it too)", name, rng, err)
			return
		}
		t.Cleanup(func() { _ = cl.RemoveNetwork(context.Background(), name) })
	}
	b := 160 + rand.IntN(16)
	nodes := fmt.Sprintf("10.%d.%d.0/20", b, rand.IntN(8)*16)
	psa := fmt.Sprintf("10.%d.%d.0/20", b, 128+rand.IntN(8)*16)
	block(nodes)
	block(psa)

	inst := emutest.Start(t, []string{"gke", "compute", "sql"})
	c, err := computev1.NewService(ctx, option.WithEndpoint(inst.GatewayURL()+"/compute/v1/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	cop := func(op *computev1.Operation, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		for op.Status != "DONE" {
			if op.Region != "" {
				op, err = c.RegionOperations.Wait(project, path.Base(op.Region), op.Name).Do()
			} else {
				op, err = c.GlobalOperations.Wait(project, op.Name).Do()
			}
			if err != nil {
				t.Fatal(err)
			}
		}
		if op.Error != nil {
			t.Fatalf("compute operation failed: %+v", op.Error.Errors[0])
		}
	}
	cop(c.Networks.Insert(project, &computev1.Network{Name: "vpc", AutoCreateSubnetworks: false, ForceSendFields: []string{"AutoCreateSubnetworks"}}).Do())
	cop(c.Subnetworks.Insert(project, "us-central1", &computev1.Subnetwork{Name: "nodes", Network: "global/networks/vpc", IpCidrRange: nodes}).Do())
	cop(c.GlobalAddresses.Insert(project, &computev1.Address{Name: "psa", AddressType: "INTERNAL", Purpose: "VPC_PEERING",
		Address: strings.TrimSuffix(psa, "/20"), PrefixLength: 20, Network: "global/networks/vpc"}).Do())
	sn, err := snv1.NewService(ctx, option.WithEndpoint(inst.GatewayURL()+"/servicenetworking/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	if op, err := sn.Services.Connections.Create("services/servicenetworking.googleapis.com", &snv1.Connection{
		Network: "projects/" + project + "/global/networks/vpc", ReservedPeeringRanges: []string{"psa"},
	}).Do(); err != nil || !op.Done {
		t.Fatalf("servicenetworking: %+v %v", op, err)
	}

	cvc, _ := inst.Env.Lookup("compute")
	vpc := cvc.(emu.VPC)
	for _, n := range []struct {
		what, rng string
		get       func() (emu.SubnetNet, error)
	}{
		{"subnetwork", nodes, func() (emu.SubnetNet, error) {
			return vpc.SubnetNetwork(ctx, "projects/"+project+"/regions/us-central1/subnetworks/nodes")
		}},
		{"PSA range", psa, func() (emu.SubnetNet, error) {
			return vpc.PrivateServicesNetwork(ctx, "projects/"+project+"/global/networks/vpc")
		}},
	} {
		got, err := n.get()
		if err != nil {
			t.Fatal(err)
		}
		if got.Subnet != n.rng {
			t.Skipf("%s %s overlaps a host network and compute could not translate it (container network on %s)", n.what, n.rng, got.Subnet)
		}
		if !got.Translated {
			t.Fatalf("%s %s overlaps a host network but is not Translated (container network on %s)", n.what, n.rng, got.ContainerSubnet)
		}
	}

	sa, err := sqladmin.NewService(ctx, option.WithEndpoint(inst.GatewayURL()+"/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	sop, err := sa.Instances.Insert(project, &sqladmin.DatabaseInstance{
		Name: "db", RootPassword: "rootpw",
		Settings: &sqladmin.Settings{IpConfiguration: &sqladmin.IpConfiguration{
			PrivateNetwork: "projects/" + project + "/global/networks/vpc",
		}},
	}).Do()
	if err != nil {
		t.Fatal(err)
	}
	for sop.Status != "DONE" {
		time.Sleep(200 * time.Millisecond)
		if sop, err = sa.Operations.Get(project, sop.Name).Do(); err != nil {
			t.Fatal(err)
		}
	}
	if sop.Error != nil {
		t.Fatalf("create instance: %+v", sop.Error.Errors[0])
	}
	db, err := sa.Instances.Get(project, "db").Do()
	if err != nil {
		t.Fatal(err)
	}
	var privIP string
	for _, ip := range db.IpAddresses {
		if ip.Type == "PRIVATE" {
			privIP = ip.IpAddress
		}
	}
	if !inCIDR(psa, privIP) {
		t.Fatalf("Cloud SQL private IP %q not in the PSA range %s", privIP, psa)
	}

	cm := newClient(t, inst)
	op, err := cm.CreateCluster(ctx, &containerpb.CreateClusterRequest{Parent: parent, Cluster: &containerpb.Cluster{
		Name: "declared", Network: "vpc", Subnetwork: "nodes",
		PrivateClusterConfig: &containerpb.PrivateClusterConfig{},
		NodePools:            []*containerpb.NodePool{{Name: "np", InitialNodeCount: 1}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	waitOp(t, cm, zone, op.Name, 4*time.Minute)
	cluster, err := cm.GetCluster(ctx, &containerpb.GetClusterRequest{Name: parent + "/clusters/declared"})
	if err != nil {
		t.Fatal(err)
	}
	if pe := cluster.GetPrivateClusterConfig().GetPrivateEndpoint(); !inCIDR(nodes, pe) {
		t.Errorf("control plane private endpoint %q not in the subnetwork range %s", pe, nodes)
	}
	adm, pool := adminClient(t, inst, parent+"/clusters/declared")
	e := &env{t: t, inst: inst, cm: cm, c: cluster, ca: pool, adm: adm}

	var nl struct {
		Items []struct {
			Status struct {
				Addresses []struct{ Type, Address string }
			}
		}
	}
	e.mustKube("GET", "/api/v1/nodes", nil, &nl)
	if len(nl.Items) != 1 {
		t.Fatalf("nodes: %+v", nl)
	}
	for _, a := range nl.Items[0].Status.Addresses {
		if a.Type == "InternalIP" && !inCIDR(nodes, a.Address) {
			t.Errorf("node InternalIP %s not in the subnetwork range %s", a.Address, nodes)
		}
	}

	e.runPod("default", "psql", "default", "while true; do if nc -w 3 "+privIP+" 5432 </dev/null; then echo R OK; else echo R FAIL; fi; sleep 1; done")
	eventually(t, 90*time.Second, "a pod connecting to the Cloud SQL private IP", func() bool {
		return strings.Contains(e.podLogs("default", "psql"), "R OK")
	})
}

func inCIDR(cidr, ip string) bool {
	_, n, err := net.ParseCIDR(cidr)
	return err == nil && n.Contains(net.ParseIP(ip))
}
