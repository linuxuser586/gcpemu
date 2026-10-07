package compute

import (
	"testing"
	"time"

	computev1 "google.golang.org/api/compute/v1"
)

func TestFilter(t *testing.T) {
	item := toMap(&computev1.Route{
		Name: "default-route-1", DestRange: "0.0.0.0/0", Priority: 1000,
		Network: "https://www.googleapis.com/compute/v1/projects/p/global/networks/n",
		Tags:    []string{"a", "b"},
	})
	cases := []struct {
		f    string
		want bool
	}{
		{`name = "default-route-1"`, true},
		{`name = default-route-*`, true},
		{`name != default-route-1`, false},
		{`name eq default-route-\d`, true},
		{`name ne default-.*`, false},
		{`(network = "projects/p/global/networks/n") (destRange = "0.0.0.0/0")`, true},
		{`network = "https://www.googleapis.com/compute/v1/projects/p/global/networks/x" OR priority = 1000`, true},
		{`tags = b AND NOT name = x`, true},
		{`priority = 999`, false},
	}
	for _, c := range cases {
		f, err := parseFilter(c.f)
		if err != nil {
			t.Fatalf("%s: %v", c.f, err)
		}
		if got := f.match(item); got != c.want {
			t.Errorf("%s: got %v want %v", c.f, got, c.want)
		}
	}
	for _, bad := range []string{`name =`, `(name = a`, `name ~ a`, `"unterminated`} {
		if _, err := parseFilter(bad); err == nil {
			t.Errorf("%q: expected error", bad)
		}
	}
}

func TestCIDR(t *testing.T) {
	c, err := parseCIDR("10.128.0.0/20", true)
	if err != nil {
		t.Fatal(err)
	}
	if ipString(c.gateway()) != "10.128.0.1" || ipString(c.reservedTail()) != "10.128.15.254" || ipString(c.last()) != "10.128.15.255" {
		t.Fatalf("addresses: %s %s %s", ipString(c.gateway()), ipString(c.reservedTail()), ipString(c.last()))
	}
	for ip, want := range map[string]bool{"10.128.0.0": false, "10.128.0.1": false, "10.128.0.2": true, "10.128.15.253": true, "10.128.15.254": false, "10.128.15.255": false, "10.128.16.0": false} {
		v, _ := parseIP(ip)
		if c.usable(v) != want {
			t.Errorf("usable(%s) = %v", ip, !want)
		}
	}
	if _, err := parseCIDR("10.0.0.1/24", true); err == nil {
		t.Error("unaligned CIDR accepted")
	}
	o, _ := parseCIDR("10.128.8.0/24", true)
	if !c.overlaps(o) || !c.covers(o) || o.covers(c) {
		t.Error("overlap/cover")
	}
}

func TestNatRanges(t *testing.T) {
	sn := &computev1.Subnetwork{
		SelfLink:          link("projects/p/regions/r/subnetworks/s"),
		IpCidrRange:       "10.0.0.0/24",
		SecondaryIpRanges: []*computev1.SubnetworkSecondaryRange{{RangeName: "pods", IpCidrRange: "10.4.0.0/14"}, {RangeName: "svc", IpCidrRange: "10.8.0.0/20"}},
	}
	cases := []struct {
		nat  *computev1.RouterNat
		want []string
	}{
		{&computev1.RouterNat{SourceSubnetworkIpRangesToNat: "ALL_SUBNETWORKS_ALL_IP_RANGES"}, []string{"10.0.0.0/24", "10.4.0.0/14", "10.8.0.0/20"}},
		{&computev1.RouterNat{SourceSubnetworkIpRangesToNat: "ALL_SUBNETWORKS_ALL_PRIMARY_IP_RANGES"}, []string{"10.0.0.0/24"}},
		{&computev1.RouterNat{SourceSubnetworkIpRangesToNat: "LIST_OF_SUBNETWORKS", Subnetworks: []*computev1.RouterNatSubnetworkToNat{
			{Name: "projects/p/regions/r/subnetworks/s", SourceIpRangesToNat: []string{"LIST_OF_SECONDARY_IP_RANGES"}, SecondaryIpRangeNames: []string{"pods"}}}}, []string{"10.4.0.0/14"}},
		{&computev1.RouterNat{SourceSubnetworkIpRangesToNat: "LIST_OF_SUBNETWORKS", Subnetworks: []*computev1.RouterNatSubnetworkToNat{
			{Name: "projects/p/regions/r/subnetworks/other", SourceIpRangesToNat: []string{"ALL_IP_RANGES"}}}}, nil},
	}
	for i, c := range cases {
		got := natRanges(c.nat, sn, sn.IpCidrRange)
		if len(got) != len(c.want) {
			t.Fatalf("%d: got %v want %v", i, got, c.want)
		}
		for j := range got {
			if got[j] != c.want[j] {
				t.Fatalf("%d: got %v want %v", i, got, c.want)
			}
		}
	}
}

func TestParseConntrack(t *testing.T) {
	ev, ok := parseConntrack("    [NEW] tcp      6 120 SYN_SENT src=10.0.0.2 dst=1.1.1.1 sport=40000 dport=443 [UNREPLIED] src=1.1.1.1 dst=172.22.0.2 sport=443 dport=40001")
	if !ok || ev.Src != "10.0.0.2" || ev.Dst != "1.1.1.1" || ev.DstPort != 443 || ev.NatIP != "172.22.0.2" || ev.NatPort != 40001 || ev.Proto != "tcp" {
		t.Fatalf("%+v %v", ev, ok)
	}
	if _, ok := parseConntrack("[NEW] udp 17 30 src=10.0.0.2 dst=10.0.1.2 sport=1 dport=2 [UNREPLIED] src=10.0.1.2 dst=10.0.0.2 sport=2 dport=1"); ok {
		t.Fatal("untranslated flow reported")
	}
}

func TestMisc(t *testing.T) {
	if upperSnake("resourceInUseByAnotherResource") != "RESOURCE_IN_USE_BY_ANOTHER_RESOURCE" {
		t.Fatal(upperSnake("resourceInUseByAnotherResource"))
	}
	if st, body := parseSinkResponse("204"); st != 204 || body != "No Content" {
		t.Fatal(st, body)
	}
	if st, body := parseSinkResponse("200:ok:x"); st != 200 || body != "ok:x" {
		t.Fatal(st, body)
	}
	for in, want := range map[string]string{
		"vpc":                 "projects/p/global/networks/vpc",
		"global/networks/vpc": "projects/p/global/networks/vpc",
		"https://www.googleapis.com/compute/v1/projects/q/global/networks/vpc":       "projects/q/global/networks/vpc",
		"https://compute.googleapis.com/compute/beta/projects/q/global/networks/vpc": "projects/q/global/networks/vpc",
	} {
		if got, err := globalRef("p", "networks", in); err != nil || got != want {
			t.Errorf("%s: %s %v", in, got, err)
		}
	}
	if got, _ := regionalRef("p", "r1", "subnetworks", "regions/r2/subnetworks/s"); got != "projects/p/regions/r2/subnetworks/s" {
		t.Error(got)
	}
	if stamp(mustTime("2026-01-02T15:04:05Z"))[len("2026-01-02T08:04:05.000"):] != "-07:00" {
		t.Error(stamp(mustTime("2026-01-02T15:04:05Z")))
	}
}

func mustTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}
