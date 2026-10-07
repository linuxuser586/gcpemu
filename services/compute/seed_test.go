package compute_test

import (
	"context"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/linuxuser586/gcpemu/internal/emu"
)

const seedYAML = `
project: test-project
networks:
  - name: vpc
subnetworks:
  - name: nodes
    region: us-central1
    network: vpc
    ipCidrRange: 10.0.0.0/24
    secondaryIpRanges: [{rangeName: pods, ipCidrRange: 10.4.0.0/14}]
firewalls:
  - name: allow-internal
    network: vpc
    sourceRanges: [10.0.0.0/8]
    allowed: [{IPProtocol: tcp}]
addresses:
  - name: nat-ip
    region: us-central1
  - name: psa-range
    addressType: INTERNAL
    purpose: VPC_PEERING
    prefixLength: 16
    network: vpc
routers:
  - name: router
    region: us-central1
    network: vpc
    nats:
      - name: nat
        natIpAllocateOption: MANUAL_ONLY
        natIps: [nat-ip]
        sourceSubnetworkIpRangesToNat: ALL_SUBNETWORKS_ALL_IP_RANGES
serviceNetworkingConnections:
  - network: vpc
    reservedPeeringRanges: [psa-range]
`

func TestSeed(t *testing.T) {
	inst, c, _ := newClients(t)
	var node yaml.Node
	if err := yaml.Unmarshal([]byte(seedYAML), &node); err != nil {
		t.Fatal(err)
	}
	svc, _ := inst.Env.Lookup("compute")
	seeder := svc.(emu.Seeder)
	for i := 0; i < 2; i++ { // idempotent
		if err := seeder.ApplySeed(context.Background(), node.Content[0], "."); err != nil {
			t.Fatalf("apply %d: %v", i, err)
		}
	}
	n, err := c.Networks.Get(proj, "vpc").Do()
	if err != nil || n.AutoCreateSubnetworks || len(n.Subnetworks) != 1 || len(n.Peerings) != 1 {
		t.Fatalf("network: %+v %v", n, err)
	}
	if sn, err := c.Subnetworks.Get(proj, "us-central1", "nodes").Do(); err != nil || len(sn.SecondaryIpRanges) != 1 {
		t.Fatalf("subnet: %v", err)
	}
	if _, err := c.Firewalls.Get(proj, "allow-internal").Do(); err != nil {
		t.Fatal(err)
	}
	rt, err := c.Routers.Get(proj, "us-central1", "router").Do()
	if err != nil || len(rt.Nats) != 1 {
		t.Fatalf("router: %v", err)
	}
	if a, _ := c.Addresses.Get(proj, "us-central1", "nat-ip").Do(); a.Status != "IN_USE" {
		t.Fatalf("nat ip: %+v", a)
	}
}
