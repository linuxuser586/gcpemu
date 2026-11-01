package compute_test

import (
	"bytes"
	"context"
	"testing"

	"go.yaml.in/yaml/v3"
	computev1 "google.golang.org/api/compute/v1"

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
	svc, _ := inst.Internal().Env.Lookup("compute")
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

// TestSeedExport exports a seeded Instance, applies the export to the
// reset Instance and expects the same export back (FR-CORE-013).
func TestSeedExport(t *testing.T) {
	inst, c, _ := newClients(t)
	ctx := context.Background()
	seed := `
compute:
  project: test-project
  networks:
    - {name: vpc, mtu: 1500}
    - {name: auto, autoCreateSubnetworks: true, description: auto mode}
  subnetworks:
    - name: nodes
      region: us-central1
      network: vpc
      ipCidrRange: 10.0.0.0/24
      secondaryIpRanges: [{rangeName: pods, ipCidrRange: 10.4.0.0/14}]
  firewalls:
    - {name: allow-internal, network: vpc, sourceRanges: [10.0.0.0/8], allowed: [{IPProtocol: tcp, ports: ["80", "443"]}], targetTags: [web]}
    - {name: deny-egress, network: vpc, direction: EGRESS, priority: 0, destinationRanges: [0.0.0.0/0], denied: [{IPProtocol: all}]}
  routes:
    - {name: via-gw, network: vpc, destRange: 192.168.0.0/16, priority: 900, nextHopGateway: global/gateways/default-internet-gateway, tags: [egress]}
  addresses:
    - {name: lb-ip, ipVersion: IPV4, labels: {env: test}}
    - {name: nat-ip, region: us-central1}
    - {name: node-ip, region: us-central1, addressType: INTERNAL, subnetwork: nodes, address: 10.0.0.10}
    - {name: psa-range, addressType: INTERNAL, purpose: VPC_PEERING, prefixLength: 16, network: vpc}
  routers:
    - name: router
      region: us-central1
      network: vpc
      nats:
        - name: nat
          natIpAllocateOption: MANUAL_ONLY
          natIps: [nat-ip]
          sourceSubnetworkIpRangesToNat: LIST_OF_SUBNETWORKS
          subnetworks: [{name: nodes, sourceIpRangesToNat: [ALL_IP_RANGES]}]
  serviceNetworkingConnections:
    - network: vpc
      reservedPeeringRanges: [psa-range]
`
	if err := inst.Internal().ApplySeedData(ctx, []byte(seed), "."); err != nil {
		t.Fatalf("%v\n%s", err, seed)
	}
	first, err := inst.Internal().ExportSeed(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, unwanted := range []string{"selfLink", "creationTimestamp", "fingerprint", "default-route-", "https://", "status:"} {
		if bytes.Contains(first, []byte(unwanted)) {
			t.Errorf("export contains %q:\n%s", unwanted, first)
		}
	}
	if err := inst.Internal().Reset(ctx); err != nil {
		t.Fatal(err)
	}
	if err := inst.Internal().ApplySeedData(ctx, first, "."); err != nil {
		t.Fatalf("apply export: %v\n%s", err, first)
	}
	second, err := inst.Internal().ExportSeed(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatalf("export changed after a round trip:\n%s\n---\n%s", first, second)
	}
	n, err := c.Networks.Get(proj, "vpc").Do()
	if err != nil || len(n.Subnetworks) != 1 || len(n.Peerings) != 1 || n.Mtu != 1500 {
		t.Fatalf("network after round trip: %+v %v", n, err)
	}
	if a, _ := c.Addresses.Get(proj, "us-central1", "nat-ip").Do(); a.Status != "IN_USE" {
		t.Fatalf("nat ip after round trip: %+v", a)
	}
	if fw, err := c.Firewalls.Get(proj, "deny-egress").Do(); err != nil || fw.Priority != 0 {
		t.Fatalf("priority 0 firewall after round trip: %+v %v", fw, err)
	}
	// New external addresses skip the IPs the export reserved.
	waitDo(t, c)(c.GlobalAddresses.Insert(proj, &computev1.Address{Name: "next"}).Do())
	if a, err := c.GlobalAddresses.Get(proj, "next").Do(); err != nil || a.Address == "34.120.0.1" {
		t.Fatalf("new address after round trip: %+v %v", a, err)
	}
}
