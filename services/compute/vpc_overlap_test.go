package compute

import (
	"testing"

	"github.com/linuxuser586/gcpemu/internal/runtime"
)

func TestOverlapHolder(t *testing.T) {
	inst := func(id string) map[string]string { return map[string]string{runtime.LabelInstance: id} }
	// abc123 is Abandoned; old999 is Abandoned but no object records its data dir.
	status := func(id string) (abandoned, unverified bool) {
		return id == "abc123" || id == "old999", id == "old999"
	}
	cases := []struct {
		name string
		rng  string
		nets []runtime.Network
		want string
	}{
		{"abandoned", "10.127.240.0/20", []runtime.Network{
			{Name: "bridge", Subnet: "172.17.0.0/16"},
			{Name: "gcpemu-abc123-vpc-default", Subnet: "10.127.240.0/20", Labels: inst("abc123")},
		}, "10.127.240.0/20 is held by network gcpemu-abc123-vpc-default of Abandoned Instance ID abc123; run `gcpemu prune`"},
		{"unverified", "10.0.0.0/16", []runtime.Network{
			{Name: "gcpemu-old999-vpc-a", Subnet: "10.0.0.0/24", Labels: inst("old999")},
		}, "10.0.0.0/16 is held by network gcpemu-old999-vpc-a of Instance ID old999, which may be Abandoned (unverified: an instance started with --data-dir elsewhere may still own it); run `gcpemu prune` to check"},
		{"live instance", "10.0.0.0/16", []runtime.Network{
			{Name: "gcpemu-def456-vpc-a", Subnet: "10.0.4.0/24", Labels: inst("def456")},
		}, "10.0.0.0/16 is held by network gcpemu-def456-vpc-a of Instance ID def456"},
		{"not gcpemu", "10.0.0.0/16", []runtime.Network{
			{Name: "compose_default", Subnet: "10.0.0.0/8"},
		}, "10.0.0.0/16 is held by network compose_default"},
		{"no runtime network", "10.0.0.0/16", []runtime.Network{
			{Name: "gcpemu-x-vpc-a", Subnet: "10.0.0.0/16"}, // self
			{Name: "other", Subnet: "10.1.0.0/16"},
			{Name: "v6", Subnet: "fd00::/64"},
		}, "no runtime network holds 10.0.0.0/16; a host route or VPN may"},
	}
	for _, c := range cases {
		rng, err := parseCIDR(c.rng, true)
		if err != nil {
			t.Fatal(err)
		}
		if got := overlapHolder(c.nets, rng, "gcpemu-x-vpc-a", status); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}
