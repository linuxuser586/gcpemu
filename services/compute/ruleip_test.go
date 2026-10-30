package compute

import (
	"context"
	"strings"
	"testing"

	computev1 "google.golang.org/api/compute/v1"

	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/store"
)

const tRule = "projects/p/regions/us-central1/forwardingRules/fr"

// tSmallNet is the container network of selfTestService's subnetwork.
var tSmallNet = emu.SubnetNet{Name: "gcpemu-small", Subnet: "10.0.0.0/29", SubnetworkName: tSelfSubnet, NetworkName: tNet, Region: "us-central1"}

// An ephemeral internal forwarding rule's address is recorded against its
// subnetwork, so AllocateIP gives the next container another one (#825).
func TestAllocateIPSkipsRuleIP(t *testing.T) {
	s := selfTestService(t, "")
	ctx := context.Background()
	ip, err := s.EphemeralIP(ctx, "p", "us-central1", true, "small", tRule, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.AllocateIP(ctx, tSmallNet, "c1")
	if err != nil {
		t.Fatal(err)
	}
	if ip != "10.0.0.2" || got != "10.0.0.3" {
		t.Errorf("EphemeralIP = %s, AllocateIP = %s; want 10.0.0.2 and 10.0.0.3", ip, got)
	}
}

// An ephemeral internal forwarding rule skips addresses compute allocated
// to containers on the subnetwork's container network (#825).
func TestEphemeralIPSkipsAllocations(t *testing.T) {
	s := selfTestService(t, "")
	ctx := context.Background()
	got, err := s.AllocateIP(ctx, tSmallNet, "c1")
	if err != nil {
		t.Fatal(err)
	}
	ip, err := s.EphemeralIP(ctx, "p", "us-central1", true, "small", tRule, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != "10.0.0.2" || ip != "10.0.0.3" {
		t.Errorf("AllocateIP = %s, EphemeralIP = %s; want 10.0.0.2 and 10.0.0.3", got, ip)
	}
}

// Reserving an internal Address on a forwarding rule's ephemeral address
// is refused, and a reservation that names none skips it (#825).
func TestAddressSkipsRuleIP(t *testing.T) {
	s := selfTestService(t, "")
	if _, err := s.EphemeralIP(context.Background(), "p", "us-central1", true, "small", tRule, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := reserveInternal(t, s, "asked", "10.0.0.2"); err == nil || !strings.Contains(err.Error(), "already being used") {
		t.Errorf("reserving the rule's address: err = %v; want ipInUseByAnotherResource", err)
	}
	if ip, err := reserveInternal(t, s, "picked", ""); err != nil || ip != "10.0.0.3" {
		t.Errorf("reserved %s, %v; want 10.0.0.3", ip, err)
	}
}

// A global internal forwarding rule picks from its subnetwork like a
// regional one: it skips the subnetwork's Addresses and regional rules'
// addresses (#825).
func TestGlobalRuleIPOnSubnetwork(t *testing.T) {
	s := selfTestService(t, "")
	ctx := context.Background()
	if _, err := reserveInternal(t, s, "a", "10.0.0.2"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EphemeralIP(ctx, "p", "us-central1", true, "small", tRule, nil); err != nil {
		t.Fatal(err)
	}
	ip, err := s.EphemeralIP(ctx, "p", "", true, tSelfSubnet, "projects/p/global/forwardingRules/g", nil)
	if err != nil || ip != "10.0.0.4" {
		t.Errorf("global EphemeralIP = %s, %v; want 10.0.0.4", ip, err)
	}
}

// Releasing a rule's record frees its address; so does recording another
// address for the rule (#825).
func TestReleaseRuleIP(t *testing.T) {
	s := selfTestService(t, "")
	ctx := context.Background()
	if _, err := s.EphemeralIP(ctx, "p", "us-central1", true, "small", tRule, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordRuleIP(ctx, RuleIP{Rule: tRule, Subnetwork: "small", IP: "10.0.0.4"}); err != nil {
		t.Fatal(err)
	}
	if got, err := s.AllocateIP(ctx, tSmallNet, "c1"); err != nil || got != "10.0.0.2" {
		t.Errorf("after moving the rule: AllocateIP = %s, %v; want 10.0.0.2", got, err)
	}
	if err := s.ReleaseRuleIP(ctx, tRule); err != nil {
		t.Fatal(err)
	}
	if got, err := s.AllocateIP(ctx, tSmallNet, "c2"); err != nil || got != "10.0.0.3" {
		t.Errorf("after releasing: AllocateIP = %s, %v; want 10.0.0.3", got, err)
	}
	if got, err := s.AllocateIP(ctx, tSmallNet, "c3"); err != nil || got != "10.0.0.4" {
		t.Errorf("after releasing: AllocateIP = %s, %v; want 10.0.0.4", got, err)
	}
}

// ResetRuleIPs drops the records of rules not listed and records those
// listed; a rule on an Address's IP records nothing (#825).
func TestResetRuleIPs(t *testing.T) {
	s := selfTestService(t, "")
	ctx := context.Background()
	if _, err := s.EphemeralIP(ctx, "p", "us-central1", true, "small", tRule, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := reserveInternal(t, s, "a", "10.0.0.4"); err != nil {
		t.Fatal(err)
	}
	err := s.ResetRuleIPs(ctx, []RuleIP{
		{Rule: "projects/p/regions/us-central1/forwardingRules/kept", Subnetwork: "small", IP: "10.0.0.3"},
		{Rule: "projects/p/regions/us-central1/forwardingRules/addr", Subnetwork: "small", IP: "10.0.0.4"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.AllocateIP(ctx, tSmallNet, "c1"); err != nil || got != "10.0.0.2" {
		t.Errorf("AllocateIP = %s, %v; want 10.0.0.2, the dropped record's", got, err)
	}
	if got, err := s.AllocateIP(ctx, tSmallNet, "c2"); err != nil || got != "10.0.0.5" {
		t.Errorf("AllocateIP = %s, %v; want 10.0.0.5, past the kept record and the Address", got, err)
	}
	if err := s.env.Store.Update(func(tx store.Tx) error { return tx.Delete(nsAddresses, "projects/p/regions/us-central1/addresses/a") }); err != nil {
		t.Fatal(err)
	}
	if _, err := reserveInternal(t, s, "b", "10.0.0.4"); err != nil {
		t.Errorf("reserving the Address's IP once it is gone: %v; the rule on it recorded nothing", err)
	}
}

// holdErr is HoldRuleIP's error for rule tRule naming ip on "small".
func holdErr(s *Service, ip string) error {
	return s.HoldRuleIP(context.Background(), RuleIP{Rule: tRule, Subnetwork: "small", IP: ip})
}

// A forwarding rule naming an address held on its subnetwork is refused:
// a container's, the Self address, another rule's; one outside the
// range or GCP-reserved is invalid (#823).
func TestHoldRuleIPRefusesHeld(t *testing.T) {
	s := selfTestService(t, "10.0.0.5")
	ctx := context.Background()
	if _, err := s.AllocateIP(ctx, tSmallNet, "c1"); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordRuleIP(ctx, RuleIP{Rule: "projects/p/regions/us-central1/forwardingRules/other", Subnetwork: "small", IP: "10.0.0.3"}); err != nil {
		t.Fatal(err)
	}
	for ip, want := range map[string]string{
		"10.0.0.2": "already being used", // c1's
		"10.0.0.3": "already being used", // the other rule's
		"10.0.0.5": "already being used", // the Self address
		"10.0.1.2": "Invalid value for field 'resource.IPAddress'",
		"10.0.0.6": "Invalid value for field 'resource.IPAddress'", // GCP-reserved
	} {
		if err := holdErr(s, ip); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v; want %q", ip, err, want)
		}
	}
	if err := holdErr(s, "10.0.0.4"); err != nil {
		t.Fatalf("a free address: %v", err)
	}
	if err := holdErr(s, "10.0.0.4"); err != nil {
		t.Errorf("the rule's own address again: %v", err)
	}
	if got, err := s.EphemeralIP(ctx, "p", "us-central1", true, "small", "projects/p/regions/us-central1/forwardingRules/next", nil); err == nil {
		t.Errorf("EphemeralIP = %s; want ipSpaceExhausted, the rule holding the last free address", got)
	}
}

// A forwarding rule may name a reserved internal Address's IP while the
// Address has no other users, or is a SHARED_LOADBALANCER_VIP (#823).
func TestHoldRuleIPOnAddress(t *testing.T) {
	s := selfTestService(t, "")
	for _, a := range []*computev1.Address{
		{Name: "free", Address: "10.0.0.2"},
		{Name: "used", Address: "10.0.0.3", Purpose: "GCE_ENDPOINT", Users: []string{link("projects/p/regions/us-central1/forwardingRules/other")}},
		{Name: "mine", Address: "10.0.0.4", Purpose: "GCE_ENDPOINT", Users: []string{link(tRule)}},
		{Name: "shared", Address: "10.0.0.5", Purpose: "SHARED_LOADBALANCER_VIP", Users: []string{link("projects/p/regions/us-central1/forwardingRules/other")}},
	} {
		a.AddressType, a.Subnetwork = "INTERNAL", link(tSelfSubnet)
		if err := s.env.Store.Update(func(tx store.Tx) error {
			return store.PutJSON(tx, nsAddresses, "projects/p/regions/us-central1/addresses/"+a.Name, a)
		}); err != nil {
			t.Fatal(err)
		}
	}
	for ip, ok := range map[string]bool{"10.0.0.2": true, "10.0.0.3": false, "10.0.0.4": true, "10.0.0.5": true} {
		if err := holdErr(s, ip); (err == nil) != ok {
			t.Errorf("%s: err = %v; want ok = %v", ip, err, ok)
		} else if !ok && !strings.Contains(err.Error(), "already being used") {
			t.Errorf("%s: err = %v; want ipInUseByAnotherResource", ip, err)
		}
	}
}
