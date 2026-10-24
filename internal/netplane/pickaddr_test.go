package netplane_test

import (
	"net/netip"
	"testing"

	"github.com/linuxuser586/gcpemu/internal/netplane"
)

func TestPickAddrKeepsPreferred(t *testing.T) {
	n := netplane.Net{Subnet: "172.31.0.0/16", Gateway: "172.31.0.1"}
	got, err := n.PickAddr("172.31.0.2")
	if err != nil || got != "172.31.0.2" {
		t.Fatalf("PickAddr = %q, %v; want 172.31.0.2", got, err)
	}
}

func TestPickAddrUpperHalf(t *testing.T) {
	for _, tc := range []struct {
		name            string
		n               netplane.Net
		preferred       string
		lowest, highest string
	}{
		// The network moved to another range (#644).
		{"outside", netplane.Net{Subnet: "172.25.0.0/16", Gateway: "172.25.0.1"}, "172.31.0.2", "172.25.128.0", "172.25.255.254"},
		{"none", netplane.Net{Subnet: "10.89.4.0/24", Gateway: "10.89.4.1"}, "", "10.89.4.128", "10.89.4.254"},
		{"gateway", netplane.Net{Subnet: "192.168.0.0/20", Gateway: "192.168.15.254"}, "192.168.15.254", "192.168.8.0", "192.168.15.253"},
		{"network", netplane.Net{Subnet: "10.0.0.0/30", Gateway: "10.0.0.1"}, "10.0.0.0", "10.0.0.2", "10.0.0.2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lo, hi := netip.MustParseAddr(tc.lowest), netip.MustParseAddr(tc.highest)
			for range 200 {
				got, err := tc.n.PickAddr(tc.preferred)
				if err != nil {
					t.Fatal(err)
				}
				a, err := netip.ParseAddr(got)
				if err != nil || a.Less(lo) || hi.Less(a) {
					t.Fatalf("PickAddr = %q; want within %s-%s", got, lo, hi)
				}
			}
		})
	}
}

func TestPickAddrBadSubnet(t *testing.T) {
	for _, s := range []string{"", "nonsense", "fd00::/64", "10.0.0.0/31"} {
		if got, err := (netplane.Net{Subnet: s}).PickAddr(""); err == nil {
			t.Errorf("PickAddr on %q = %q; want an error", s, got)
		}
	}
}
