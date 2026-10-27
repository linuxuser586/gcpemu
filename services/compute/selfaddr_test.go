package compute

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	computev1 "google.golang.org/api/compute/v1"

	"github.com/linuxuser586/gcpemu/internal/clock"
	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/store"
)

const tSelfSubnet = "projects/p/regions/us-central1/subnetworks/small"

// selfTestService returns a service whose subnetwork 10.0.0.0/29 (usable
// addresses .2 to .5) is realised as container network gcpemu-small, on
// which the Instance's container holds self.
func selfTestService(t *testing.T, self string) *Service {
	t.Helper()
	st := store.NewMemory()
	s := &Service{env: &emu.Env{Config: &config.Config{}, Store: st, Clock: clock.Real{}, Log: slog.New(slog.DiscardHandler)}}
	err := st.Update(func(tx store.Tx) error {
		if err := store.PutJSON(tx, nsSubnets, tSelfSubnet, &computev1.Subnetwork{
			Name: "small", IpCidrRange: "10.0.0.0/29", Network: link(tNet), SelfLink: link(tSelfSubnet),
		}); err != nil {
			return err
		}
		return store.PutJSON(tx, nsVPCNets, tSelfSubnet, &vpcNet{
			Name: "gcpemu-small", Subnet: "10.0.0.0/29", Range: "10.0.0.0/29", Network: tNet, Region: "us-central1", Subnetwork: tSelfSubnet, Self: self,
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// reserveInternal reserves an internal Address in the subnetwork, asking
// for ip ("" picks one), and returns the address it got.
func reserveInternal(t *testing.T, s *Service, name, ip string) (string, error) {
	t.Helper()
	a := &computev1.Address{Name: name, Address: ip, Subnetwork: "small"}
	err := s.env.Store.Update(func(tx store.Tx) error {
		if err := s.allocRegionalInternal(tx, "p", "us-central1", a); err != nil {
			return err
		}
		path := "projects/p/regions/us-central1/addresses/" + name
		a.SelfLink = link(path)
		return store.PutJSON(tx, nsAddresses, path, a)
	})
	return a.Address, err
}

// No internal Address takes the Self address (#811).
func TestAddressSkipsSelf(t *testing.T) {
	s := selfTestService(t, "10.0.0.5")
	if _, err := reserveInternal(t, s, "asked", "10.0.0.5"); err == nil || !strings.Contains(err.Error(), "already being used") {
		t.Fatalf("reserving the Self address: err = %v; want ipInUseByAnotherResource", err)
	}
	var got []string
	for _, name := range []string{"a", "b", "c"} {
		ip, err := reserveInternal(t, s, name, "")
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, ip)
	}
	if strings.Join(got, ",") != "10.0.0.2,10.0.0.3,10.0.0.4" {
		t.Errorf("picked %v; want 10.0.0.2-4", got)
	}
	if ip, err := reserveInternal(t, s, "d", ""); err == nil {
		t.Errorf("picked %s; want ipSpaceExhausted, as only the Self address is left", ip)
	}
}

// No ephemeral forwarding-rule IP is the Self address (#811).
func TestEphemeralIPSkipsSelf(t *testing.T) {
	s := selfTestService(t, "10.0.0.2")
	ip, err := s.EphemeralIP(context.Background(), "p", "us-central1", true, "small", nil)
	if err != nil {
		t.Fatal(err)
	}
	if ip != "10.0.0.3" {
		t.Errorf("EphemeralIP = %s; want 10.0.0.3, the lowest free address that is not the Self address", ip)
	}
}

// A recreated Instance container that rejoins at another address frees
// the old Self address and holds the new one (#811).
func TestSelfJoinedMoves(t *testing.T) {
	s := selfTestService(t, "10.0.0.5")
	s.SelfJoined(map[string]string{"gcpemu-small": "10.0.0.4", "gcpemu-other": "10.9.0.4"})
	if _, err := reserveInternal(t, s, "new", "10.0.0.4"); err == nil || !strings.Contains(err.Error(), "already being used") {
		t.Errorf("reserving the new Self address: err = %v; want ipInUseByAnotherResource", err)
	}
	if _, err := reserveInternal(t, s, "old", "10.0.0.5"); err != nil {
		t.Errorf("reserving the old Self address: %v", err)
	}
}

// An Instance that no longer holds an address on the network, such as one
// now on the host, frees the old Self address (#811).
func TestSelfJoinedClears(t *testing.T) {
	s := selfTestService(t, "10.0.0.5")
	s.SelfJoined(map[string]string{})
	if _, err := reserveInternal(t, s, "old", "10.0.0.5"); err != nil {
		t.Errorf("reserving the old Self address: %v", err)
	}
}
