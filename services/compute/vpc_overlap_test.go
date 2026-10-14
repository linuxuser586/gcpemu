package compute

import (
	"testing"

	"github.com/linuxuser586/gcpemu/internal/runtime"
)

func TestOverlapHolder(t *testing.T) {
	want, _ := parseCIDR("10.127.240.0/20", true)
	abandoned := func(id string) bool { return id == "abc123" }
	got := overlapHolder([]runtime.Network{
		{Name: "bridge", Subnet: "172.17.0.0/16"},
		{Name: "gcpemu-abc123-vpc-default", Subnet: "10.127.240.0/20", Labels: map[string]string{runtime.LabelInstance: "abc123"}},
	}, want, "gcpemu-def456-vpc-default", abandoned)
	if w := "10.127.240.0/20 is held by network gcpemu-abc123-vpc-default of Abandoned Instance ID abc123; run `gcpemu prune`"; got != w {
		t.Fatalf("got %q, want %q", got, w)
	}
}

func TestOverlapHolderLiveAndForeign(t *testing.T) {
	want, _ := parseCIDR("10.0.0.0/16", true)
	never := func(string) bool { return false }
	cases := []struct {
		name string
		nets []runtime.Network
		want string
	}{
		{"live instance", []runtime.Network{
			{Name: "gcpemu-def456-vpc-a", Subnet: "10.0.4.0/24", Labels: map[string]string{runtime.LabelInstance: "def456"}},
		}, "10.0.0.0/16 is held by network gcpemu-def456-vpc-a of Instance ID def456"},
		{"not gcpemu", []runtime.Network{
			{Name: "compose_default", Subnet: "10.0.0.0/8"},
		}, "10.0.0.0/16 is held by network compose_default"},
		{"self and no overlap", []runtime.Network{
			{Name: "gcpemu-x-vpc-a", Subnet: "10.0.0.0/16"},
			{Name: "other", Subnet: "10.1.0.0/16"},
			{Name: "v6", Subnet: "fd00::/64"},
		}, ""},
	}
	for _, c := range cases {
		if got := overlapHolder(c.nets, want, "gcpemu-x-vpc-a", never); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}
