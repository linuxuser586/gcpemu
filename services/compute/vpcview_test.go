package compute

import (
	"reflect"
	"testing"

	"github.com/linuxuser586/gcpemu/internal/runtime"
)

// managed is a Managed container of service and role holding ip on net.
func managed(name, service, resource, role, net, ip string, extra ...string) runtime.Container {
	l := map[string]string{runtime.LabelService: service, runtime.LabelResource: resource, runtime.LabelRole: role}
	for i := 0; i+1 < len(extra); i += 2 {
		l[extra[i]] = extra[i+1]
	}
	return runtime.Container{Name: name, Labels: l, IPs: map[string]string{net: ip, "gcpemu-x-svc": "172.30.0.9"}}
}

func TestContainerNetworkTranslated(t *testing.T) {
	const net = "gcpemu-x-vpc-default-a-1234"
	st := &vpcNet{
		Name: net, Subnet: "10.240.16.0/24", Gateway: "10.240.16.1", Range: "10.0.0.0/24",
		Fallback: true, Translated: true,
		Network: "projects/p/global/networks/default", Region: "us-central1",
		Subnetwork: "projects/p/regions/us-central1/subnetworks/a",
	}
	owners := []ownedIP{
		{owner: "sql:p--db", ip: "10.0.0.2"},
		{owner: "projects/p/locations/us-central1/clusters/c/node-1", ip: "10.0.0.3"},
		{owner: "projects/p/locations/us-central1/clusters/c/control-plane", ip: "10.0.0.4"},
		{owner: "lb-edge", ip: "10.0.0.5"},
	}
	cts := []runtime.Container{
		managed("gcpemu-x-sql-db", "sql", "p--db", "postgres", net, "10.240.16.2"),
		managed("gcpemu-x-gke-c-node-1", "gke", "projects/p/locations/us-central1/clusters/c", "node", net, "10.240.16.3", "gcpemu.gke.node", "node-1"),
		managed("gcpemu-x-gke-c-cp", "gke", "projects/p/locations/us-central1/clusters/c", "server", net, "10.240.16.4"),
		managed("gcpemu-x-lb-edge", "lb", "edge", "edge", net, "10.240.16.5"),
		managed("gcpemu-x-nat-default", "compute", "projects/p/global/networks/default", "nat", net, "10.240.16.254"),
		managed("gcpemu-x-other", "gke", "k", "node", "gcpemu-x-vpc-other", "10.9.9.9"),
	}
	got := containerNetwork(st, owners, cts, "", false)
	want := containerNetworkJSON{
		Network:       "projects/p/global/networks/default",
		Subnetwork:    "projects/p/regions/us-central1/subnetworks/a",
		Region:        "us-central1",
		Bridge:        net,
		DeclaredRange: "10.0.0.0/24",
		FallbackRange: "10.240.16.0/24",
		Translated:    true,
		HostAddress:   "",
		Containers: []containerJSON{
			{Name: "gcpemu-x-gke-c-cp", Service: "gke", Role: "server", DeclaredAddress: "10.0.0.4", Address: "10.240.16.4"},
			{Name: "gcpemu-x-gke-c-node-1", Service: "gke", Role: "node", DeclaredAddress: "10.0.0.3", Address: "10.240.16.3"},
			{Name: "gcpemu-x-lb-edge", Service: "lb", Role: "edge", DeclaredAddress: "10.0.0.5", Address: "10.240.16.5"},
			{Name: "gcpemu-x-nat-default", Service: "compute", Role: "nat", DeclaredAddress: "10.0.0.254", Address: "10.240.16.254"},
			{Name: "gcpemu-x-sql-db", Service: "sql", Role: "postgres", DeclaredAddress: "10.0.0.2", Address: "10.240.16.2"},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got  %+v\nwant %+v", got, want)
	}
}

func TestContainerNetworkNotTranslated(t *testing.T) {
	// Managed containers could not alias: they hold, and report,
	// Fallback-range addresses.
	const net = "gcpemu-x-vpc-default-a-1234"
	st := &vpcNet{
		Name: net, Subnet: "10.240.16.0/24", Gateway: "10.240.16.1", Range: "10.0.0.0/24", Fallback: true,
		Network: "projects/p/global/networks/default", Region: "us-central1",
		Subnetwork: "projects/p/regions/us-central1/subnetworks/a",
	}
	cts := []runtime.Container{managed("gcpemu-x-sql-db", "sql", "p--db", "postgres", net, "10.240.16.2")}
	got := containerNetwork(st, []ownedIP{{owner: "sql:p--db", ip: "10.240.16.2"}}, cts, "", true)
	if got.Translated || got.FallbackRange != "10.240.16.0/24" || got.DeclaredRange != "10.0.0.0/24" {
		t.Errorf("ranges: %+v", got)
	}
	want := []containerJSON{{Name: "gcpemu-x-sql-db", Service: "sql", Role: "postgres", DeclaredAddress: "10.240.16.2", Address: "10.240.16.2"}}
	if !reflect.DeepEqual(got.Containers, want) {
		t.Errorf("containers: got %+v, want %+v", got.Containers, want)
	}
	if got.HostAddress != "10.240.16.1" {
		t.Errorf("Host address: got %q, want the container network's gateway 10.240.16.1", got.HostAddress)
	}
}

func TestContainerNetworkHostAddress(t *testing.T) {
	const net = "gcpemu-x-vpc-default-a-1234"
	sub := &vpcNet{
		Name: net, Subnet: "10.0.0.0/24", Gateway: "10.0.0.1", Range: "10.0.0.0/24",
		Network: "projects/p/global/networks/default", Subnetwork: "projects/p/regions/us-central1/subnetworks/a",
	}
	relay := managed("gcpemu-x-vmrelay", "core", "vmrelay", "relay", net, "10.0.0.7")
	cases := []struct {
		name     string
		st       *vpcNet
		cts      []runtime.Container
		attached string
		direct   bool
		want     string
	}{
		{"routed", sub, nil, "", true, "10.0.0.1"},
		{"VM relay", sub, []runtime.Container{relay}, "", false, "10.0.0.7"},
		{"served before", sub, []runtime.Container{relay}, "10.0.0.9", false, "10.0.0.9"},
		{"unknown", sub, nil, "", false, ""},
		{"private services access", &vpcNet{Name: "psa", Subnet: "10.8.0.0/16", Gateway: "10.8.0.1", Range: "10.8.0.0/16", Network: sub.Network}, nil, "", true, ""},
	}
	for _, c := range cases {
		got := containerNetwork(c.st, nil, c.cts, c.attached, c.direct)
		if got.HostAddress != c.want {
			t.Errorf("%s: Host address %q, want %q", c.name, got.HostAddress, c.want)
		}
		if got.FallbackRange != "" || got.Translated {
			t.Errorf("%s: on the Declared range, got %+v", c.name, got)
		}
	}
}
