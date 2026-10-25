package lb_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	computev1 "google.golang.org/api/compute/v1"

	"github.com/linuxuser586/gcpemu/emutest"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/services/gke"
	"github.com/linuxuser586/gcpemu/services/lb"
)

// TestEdgeRelayLeavesSubnetwork (#683): while external forwarding rules
// keep the Edge relay running, it leaves a subnetwork's container
// network once no pod route uses it, and deleting the subnetwork right
// after its last pod route is gone succeeds instead of finding the edge's
// address on it.
func TestEdgeRelayLeavesSubnetwork(t *testing.T) {
	emutest.RequireRuntime(t)
	e := start(t)
	c := e.c
	ctx := context.Background()
	env := e.inst.Internal().Env
	svc, _ := env.Lookup("lb")
	vpcSvc, _ := env.Lookup("compute")
	vpc := vpcSvc.(emu.VPC)
	regionOp := func(op *computev1.Operation, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		if op.Status != "DONE" {
			if op, err = c.RegionOperations.Wait(proj, "us-central1", op.Name).Do(); err != nil {
				t.Fatal(err)
			}
		}
		if op.Error != nil {
			t.Fatalf("operation %s failed: %+v", op.OperationType, op.Error.Errors[0])
		}
	}

	// Pod routes on the default subnetwork keep the edge on it; the
	// routes on "nodes" come and go like a cluster's.
	kept, err := vpc.ResolveSubnetwork(ctx, proj, "us-central1", "", "")
	if err != nil {
		t.Fatal(err)
	}
	e.do(c.Networks.Insert(proj, &computev1.Network{Name: "vpc", AutoCreateSubnetworks: false, ForceSendFields: []string{"AutoCreateSubnetworks"}}).Do())
	regionOp(c.Subnetworks.Insert(proj, "us-central1", &computev1.Subnetwork{Name: "nodes", Network: "global/networks/vpc", IpCidrRange: "10.150.0.0/20"}).Do())
	nodes := "projects/" + proj + "/regions/us-central1/subnetworks/nodes"
	keptNet, err := vpc.SubnetNetwork(ctx, kept)
	if err != nil {
		t.Fatal(err)
	}
	nodesNet, err := vpc.SubnetNetwork(ctx, nodes)
	if err != nil {
		t.Fatal(err)
	}

	// An external forwarding rule, so the Edge relay serves public
	// addresses to pods and stays up.
	e.do(c.BackendServices.Insert(proj, &computev1.BackendService{Name: "be", LoadBalancingScheme: "EXTERNAL_MANAGED"}).Do())
	e.do(c.UrlMaps.Insert(proj, &computev1.UrlMap{Name: "map", DefaultService: "global/backendServices/be"}).Do())
	e.do(c.TargetHttpProxies.Insert(proj, &computev1.TargetHttpProxy{Name: "p", UrlMap: "global/urlMaps/map"}).Do())
	e.do(c.GlobalForwardingRules.Insert(proj, &computev1.ForwardingRule{Name: "fr", LoadBalancingScheme: "EXTERNAL_MANAGED",
		PortRange: "8080", Target: "global/targetHttpProxies/p"}).Do())

	var onNodes atomic.Bool
	onNodes.Store(true)
	lb.SetPodRoutes(svc, func(ctx context.Context) []gke.PodRoute {
		prs := []gke.PodRoute{{Cluster: "projects/" + proj + "/locations/us-central1/clusters/kept", Node: "k",
			CIDR: "10.200.0.0/24", Via: "10.128.0.5", Subnetwork: kept}}
		if onNodes.Load() {
			prs = append(prs, gke.PodRoute{Cluster: "projects/" + proj + "/locations/us-central1/clusters/c", Node: "n",
				CIDR: "10.201.0.0/24", Via: "10.150.0.5", Subnetwork: nodes})
		}
		return prs
	})
	svc.(*lb.Service).SetOptions(lb.Options{})

	rt, err := env.Containers.Runtime(ctx)
	if err != nil {
		t.Fatal(err)
	}
	edgeIPs := func() map[string]string {
		ct, err := rt.InspectContainer(ctx, rt.Name("lb-edge"))
		if err != nil || !ct.Running {
			return nil
		}
		return ct.IPs
	}
	joined := func() bool { ips := edgeIPs(); return ips[keptNet.Name] != "" && ips[nodesNet.Name] != "" }
	eventually(t, 60*time.Second, "edge on both subnetworks", joined)

	// The route loop leaves "nodes" once its pod routes are gone.
	onNodes.Store(false)
	eventually(t, 30*time.Second, "edge off the nodes subnetwork", func() bool {
		ips := edgeIPs()
		return ips[keptNet.Name] != "" && ips[nodesNet.Name] == ""
	})

	// Deleting the subnetwork right after its pod routes are gone does not
	// wait for the route loop, which direct dial pauses here so that only
	// the delete can detach the Edge relay (ADR 0032).
	onNodes.Store(true)
	eventually(t, 30*time.Second, "edge back on the nodes subnetwork", joined)
	svc.(*lb.Service).SetOptions(lb.Options{DirectDial: true})
	time.Sleep(time.Second) // a pass already past the direct-dial check reads the routes before onNodes changes
	onNodes.Store(false)
	regionOp(c.Subnetworks.Delete(proj, "us-central1", "nodes").Do())
	if ips := edgeIPs(); ips[keptNet.Name] == "" || ips[nodesNet.Name] != "" {
		t.Fatalf("edge networks after the delete = %v; want it on %s only", ips, keptNet.Name)
	}
	if _, err := vpc.SubnetNetwork(ctx, nodes); err == nil {
		t.Fatalf("subnetwork %s still realised after its delete", nodes)
	}
}
