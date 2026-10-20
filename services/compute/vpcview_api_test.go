package compute_test

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net/http"
	"testing"

	"github.com/linuxuser586/gcpemu/emutest"
	"github.com/linuxuser586/gcpemu/internal/emu"
)

// containerNetwork is an entry of /_emu/v1/vpc/containerNetworks.
type containerNetwork struct {
	Network       string `json:"network"`
	Subnetwork    string `json:"subnetwork"`
	Allocation    string `json:"allocation"`
	Region        string `json:"region"`
	Bridge        string `json:"bridge"`
	DeclaredRange string `json:"declaredRange"`
	FallbackRange string `json:"fallbackRange"`
	Translated    bool   `json:"translated"`
	HostAddress   string `json:"hostAddress"`
	Containers    []struct {
		Name            string `json:"name"`
		Service         string `json:"service"`
		Role            string `json:"role"`
		DeclaredAddress string `json:"declaredAddress"`
		Address         string `json:"address"`
	} `json:"containers"`
}

// containerNetworks lists the VPC container networks of query.
func containerNetworks(t *testing.T, inst *emutest.Instance, query string) []containerNetwork {
	t.Helper()
	var got struct {
		ContainerNetworks []containerNetwork `json:"containerNetworks"`
		Error             string             `json:"error"`
	}
	if code := admin(t, inst, http.MethodGet, "/_emu/v1/vpc/containerNetworks?"+query, nil, &got); code != http.StatusOK {
		t.Fatalf("containerNetworks?%s: %d %s", query, code, got.Error)
	}
	return got.ContainerNetworks
}

// TestContainerNetworksUnrealised: a subnetwork has no VPC container
// network until a Managed container uses it, and listing never makes one.
func TestContainerNetworksUnrealised(t *testing.T) {
	t.Parallel()
	inst, c, _ := newClients(t)
	customNet(t, c, "vpc")
	subnet(t, c, "vpc", "s", "us-central1", "10.127.0.0/24")
	if got := containerNetworks(t, inst, "project="+proj); len(got) != 0 {
		t.Fatalf("got %+v, want none", got)
	}
	var e struct{ Error string }
	if code := admin(t, inst, http.MethodGet, "/_emu/v1/vpc/containerNetworks", nil, &e); code != http.StatusBadRequest || e.Error == "" {
		t.Fatalf("without a project: %d %q", code, e.Error)
	}
}

// realise realises subnetwork s of network vpc, on rng, with the NAT
// egress proxy and a workload holding an address on it.
func realise(t *testing.T, rng string) (*emutest.Instance, emu.SubnetNet, string) {
	t.Helper()
	inst, c, _ := newClients(t)
	customNet(t, c, "vpc")
	customNet(t, c, "other")
	subnet(t, c, "vpc", "s", "us-central1", rng)
	svc, _ := inst.Internal().Env.Lookup("compute")
	vpc := svc.(emu.VPC)
	ctx := context.Background()
	sp := "projects/" + proj + "/regions/us-central1/subnetworks/s"
	n, err := vpc.SubnetNetwork(ctx, sp)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := vpc.EgressProxy(ctx, sp); err != nil {
		t.Fatal(err)
	}
	ip, err := vpc.AllocateIP(ctx, n, "vm-1")
	if err != nil {
		t.Fatal(err)
	}
	workload(t, inst, n, ip, "")
	return inst, n, ip
}

func TestContainerNetworksDeclaredRange(t *testing.T) {
	emutest.RequireRuntime(t)
	t.Parallel()
	inst, n, ip := realise(t, "10.122.0.0/24")
	if n.ContainerSubnet != "10.122.0.0/24" {
		t.Skipf("10.122.0.0/24 overlaps a host network (container network on %s)", n.ContainerSubnet)
	}
	if got := containerNetworks(t, inst, "project="+proj+"&network=other"); len(got) != 0 {
		t.Fatalf("network=other: got %+v, want none", got)
	}
	got := containerNetworks(t, inst, "project="+proj+"&network=vpc")
	if len(got) != 1 {
		t.Fatalf("got %+v, want one", got)
	}
	cn := got[0]
	if cn.Network != "projects/"+proj+"/global/networks/vpc" || cn.Subnetwork != "projects/"+proj+"/regions/us-central1/subnetworks/s" ||
		cn.Region != "us-central1" || cn.Bridge != n.Name || cn.DeclaredRange != "10.122.0.0/24" || cn.FallbackRange != "" || cn.Translated {
		t.Fatalf("got %+v", cn)
	}
	addrs := map[string][2]string{}
	for _, ct := range cn.Containers {
		addrs[ct.Role] = [2]string{ct.DeclaredAddress, ct.Address}
	}
	if addrs["probe"] != [2]string{ip, ip} {
		t.Errorf("workload: got %v, want %s held as declared", addrs["probe"], ip)
	}
	if addrs["nat"] != [2]string{"10.122.0.254", "10.122.0.254"} {
		t.Errorf("NAT egress proxy: got %v, want 10.122.0.254", addrs["nat"])
	}
}

// TestContainerNetworksFallback: on a Fallback range the entry says so,
// and a Translated network's NAT egress proxy holds a Fallback address
// while it reports its Declared one.
func TestContainerNetworksFallback(t *testing.T) {
	emutest.RequireRuntime(t)
	t.Parallel()
	rng := fmt.Sprintf("10.%d.%d.0/24", 176+rand.IntN(16), rand.IntN(256))
	overlap(t, rng)
	inst, n, _ := realise(t, rng)
	got := containerNetworks(t, inst, "project="+proj)
	if len(got) != 1 {
		t.Fatalf("got %+v, want one", got)
	}
	cn := got[0]
	if cn.DeclaredRange != rng || cn.FallbackRange != n.ContainerSubnet || cn.FallbackRange == rng || cn.Translated != n.Translated {
		t.Fatalf("got %+v for %+v", cn, n)
	}
	for _, ct := range cn.Containers {
		if ct.Role != "nat" {
			continue
		}
		if !net2(n.ContainerSubnet, ct.Address) {
			t.Errorf("NAT egress proxy holds %s, outside the Fallback range %s", ct.Address, n.ContainerSubnet)
		}
		if want := n.Subnet; !net2(want, ct.DeclaredAddress) {
			t.Errorf("NAT egress proxy reports %s, outside %s", ct.DeclaredAddress, want)
		}
		return
	}
	t.Errorf("no NAT egress proxy in %+v", cn.Containers)
}
