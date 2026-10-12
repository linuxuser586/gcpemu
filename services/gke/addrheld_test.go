package gke_test

import (
	"context"
	"fmt"
	"math/rand/v2"
	"path"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"cloud.google.com/go/container/apiv1/containerpb"
	computev1 "google.golang.org/api/compute/v1"
	"google.golang.org/api/option"

	"github.com/linuxuser586/gcpemu/emutest"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/runtime"
	"github.com/linuxuser586/gcpemu/services/gke"
)

// TestNodeAddressHeld (#146): containers compute did not allocate for,
// such as ones the emulator does not manage, join a subnetwork's container network with an
// address the runtime picks. A resize still succeeds when one holds the
// address compute would pick next, and when one takes the new node's
// address after the node was created but before it started (the runtime
// does not hold it until then). Each node reports the address compute
// allocated to it.
func TestNodeAddressHeld(t *testing.T) {
	emutest.RequireRuntime(t)
	t.Parallel()
	ctx := context.Background()
	cl, err := runtime.Detect()
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	holders := map[string]bool{}
	// hold starts a container on sn's network with address ip, or one the
	// runtime picks.
	hold := func(sn emu.SubnetNet, ip string) error {
		name := fmt.Sprintf("gcpemu-test-holder-%d", rand.Uint32())
		id, err := cl.CreateContainer(ctx, runtime.ContainerSpec{
			Name: name, Image: "busybox:1.37", Entrypoint: []string{"sleep", "600"},
			Networks: []runtime.Attachment{{Network: sn.Name, IP: ip}},
		})
		if err != nil {
			return err
		}
		t.Cleanup(func() { _ = cl.RemoveContainer(context.Background(), id, true) })
		if err := cl.StartContainer(ctx, id); err != nil {
			return err
		}
		ct, err := cl.InspectContainer(ctx, id)
		if err != nil {
			return err
		}
		mu.Lock()
		holders[ct.IPs[sn.Name]] = true
		mu.Unlock()
		return nil
	}
	if err := cl.EnsureImage(ctx, "busybox:1.37", false); err != nil {
		t.Fatal(err)
	}

	inst := emutest.Start(t, []string{"gke", "compute"})
	gsvc, _ := inst.Env.Lookup("gke")
	var race atomic.Bool
	gke.SetBeforeNodeStart(gsvc, func(ctx context.Context, sn emu.SubnetNet, node, ip string) {
		if race.CompareAndSwap(true, false) {
			if err := hold(sn, ip); err != nil {
				t.Errorf("taking %s from node %s: %v", ip, node, err)
			}
		}
	})

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
	rng := fmt.Sprintf("10.%d.%d.0/24", 176+rand.IntN(16), rand.IntN(256))
	cop(c.Networks.Insert(project, &computev1.Network{Name: "vpc", AutoCreateSubnetworks: false, ForceSendFields: []string{"AutoCreateSubnetworks"}}).Do())
	cop(c.Subnetworks.Insert(project, "us-central1", &computev1.Subnetwork{Name: "nodes", Network: "global/networks/vpc", IpCidrRange: rng}).Do())

	cm := newClient(t, inst)
	op, err := cm.CreateCluster(ctx, &containerpb.CreateClusterRequest{Parent: parent, Cluster: &containerpb.Cluster{
		Name: "held", Network: "vpc", Subnetwork: "nodes",
		NodePools: []*containerpb.NodePool{{Name: "np", InitialNodeCount: 1}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	waitOp(t, cm, zone, op.Name, 4*time.Minute)

	cvc, _ := inst.Env.Lookup("compute")
	vpc := cvc.(emu.VPC)
	sn, err := vpc.SubnetNetwork(ctx, "projects/"+project+"/regions/us-central1/subnetworks/nodes")
	if err != nil {
		t.Fatal(err)
	}
	if sn.Translated || sn.ContainerSubnet != rng {
		t.Skipf("subnetwork %s overlaps a host network (container network on %s); its nodes get addresses the runtime picks", rng, sn.ContainerSubnet)
	}

	resize := func(n int32) {
		t.Helper()
		op, err := cm.SetNodePoolSize(ctx, &containerpb.SetNodePoolSizeRequest{Name: parent + "/clusters/held/nodePools/np", NodeCount: n})
		if err != nil {
			t.Fatal(err)
		}
		waitOp(t, cm, zone, op.Name, 4*time.Minute)
	}
	// Joined before the resize, with the lowest free address: the one
	// compute would pick for the next node.
	if err := hold(sn, ""); err != nil {
		t.Fatal(err)
	}
	resize(2)
	// Joins with the new node's address just before it starts.
	race.Store(true)
	resize(3)
	if race.Load() {
		t.Fatal("no node was started during the resize")
	}

	cluster, err := cm.GetCluster(ctx, &containerpb.GetClusterRequest{Name: parent + "/clusters/held"})
	if err != nil {
		t.Fatal(err)
	}
	adm, pool := adminClient(t, inst, parent+"/clusters/held")
	e := &env{t: t, inst: inst, cm: cm, c: cluster, ca: pool, adm: adm}
	var nl struct {
		Items []struct {
			Metadata struct{ Name string }
			Status   struct {
				Addresses []struct{ Type, Address string }
			}
		}
	}
	eventually(t, 2*time.Minute, "3 nodes", func() bool {
		e.mustKube("GET", "/api/v1/nodes", nil, &nl)
		return len(nl.Items) == 3
	})
	for _, n := range nl.Items {
		want, err := vpc.AllocateIP(ctx, sn, project+"/"+zone+"/held/"+n.Metadata.Name)
		if err != nil {
			t.Fatal(err)
		}
		for _, a := range n.Status.Addresses {
			if a.Type != "InternalIP" {
				continue
			}
			if a.Address != want {
				t.Errorf("node %s InternalIP %s; compute allocated %s", n.Metadata.Name, a.Address, want)
			}
			mu.Lock()
			held := holders[a.Address]
			mu.Unlock()
			if held {
				t.Errorf("node %s InternalIP %s is held by another container", n.Metadata.Name, a.Address)
			}
		}
	}
}
