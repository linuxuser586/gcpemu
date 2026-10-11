package emu

import (
	"strings"
	"testing"
)

// TestAliasScript: a Translated network's Managed containers carry their
// Declared address on a VLAN of the VPC interface, and routes via the
// egress gateway go via its Declared address (ADR 0004).
func TestAliasScript(t *testing.T) {
	n := SubnetNet{Subnet: "10.0.0.0/20", ContainerSubnet: "10.241.0.0/20"}
	if s := n.AliasScript("10.0.0.2", "10.241.0.2"); s != "" {
		t.Fatalf("not Translated: %q", s)
	}
	n.Translated = true
	s := n.AliasScript("10.0.0.2", "10.241.0.2", "default", "172.16.0.0/12")
	for _, want := range []string{
		"awk -v a=10.241.0.2 ",
		`V="$IF.4000"`,
		`ip link add link "$IF" name "$V" type vlan id 4000`,
		`ip addr add 10.0.0.2/20 dev "$V"`,
		`ip route replace 10.0.0.0/20 dev "$V" src 10.0.0.2`,
		`ip route replace default via 10.0.15.254 dev "$V" src 10.0.0.2`,
		`ip route replace 172.16.0.0/12 via 10.0.15.254 dev "$V" src 10.0.0.2`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("script lacks %q:\n%s", want, s)
		}
	}
}
