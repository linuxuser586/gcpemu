package compute_test

import (
	"context"
	"testing"

	computev1 "google.golang.org/api/compute/v1"

	"github.com/linuxuser586/gcpemu/services/compute"
)

// A forwarding rule's recorded address never keeps its subnetwork from
// being deleted, and goes with it: a subnetwork recreated with the name
// has the address free (#825).
func TestSubnetworkDeleteDropsRuleIPs(t *testing.T) {
	inst, c, _ := newClients(t)
	svc, _ := inst.Internal().Env.Lookup("compute")
	cmp := svc.(*compute.Service)
	customNet(t, c, "vpc")
	sub := &computev1.Subnetwork{Name: "s", Network: "global/networks/vpc", IpCidrRange: "10.60.0.0/24"}
	waitDo(t, c)(c.Subnetworks.Insert(proj, "us-central1", sub).Do())
	ip, err := cmp.EphemeralIP(context.Background(), proj, "us-central1", true, "s", "projects/"+proj+"/regions/us-central1/forwardingRules/fr", nil)
	if err != nil {
		t.Fatal(err)
	}
	waitDo(t, c)(c.Subnetworks.Delete(proj, "us-central1", "s").Do())
	waitDo(t, c)(c.Subnetworks.Insert(proj, "us-central1", sub).Do())
	waitDo(t, c)(c.Addresses.Insert(proj, "us-central1", &computev1.Address{Name: "a", AddressType: "INTERNAL", Subnetwork: "regions/us-central1/subnetworks/s", Address: ip}).Do())
}
