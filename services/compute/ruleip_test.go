package compute

import (
	"context"
	"strings"
	"testing"

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
