package lb_test

import (
	"context"
	"testing"

	computev1 "google.golang.org/api/compute/v1"

	"github.com/linuxuser586/gcpemu/internal/store"
	"github.com/linuxuser586/gcpemu/services/lb"
)

// internalLB creates subnetwork "sub" (10.40.0.0/24) of network "vpc" and
// a regional internal Application Load Balancer up to its target proxy
// "ilb" in us-central1.
func internalLB(e *env) {
	c := e.c
	e.do(c.Networks.Insert(proj, &computev1.Network{Name: "vpc", AutoCreateSubnetworks: false, ForceSendFields: []string{"AutoCreateSubnetworks"}}).Do())
	e.do(c.Subnetworks.Insert(proj, "us-central1", &computev1.Subnetwork{Name: "sub", Network: "global/networks/vpc", IpCidrRange: "10.40.0.0/24"}).Do())
	e.do(c.RegionBackendServices.Insert(proj, "us-central1", &computev1.BackendService{Name: "ilb", LoadBalancingScheme: "INTERNAL_MANAGED", Protocol: "HTTP"}).Do())
	e.do(c.RegionUrlMaps.Insert(proj, "us-central1", &computev1.UrlMap{Name: "ilb", DefaultService: "regions/us-central1/backendServices/ilb"}).Do())
	e.do(c.RegionTargetHttpProxies.Insert(proj, "us-central1", &computev1.TargetHttpProxy{Name: "ilb", UrlMap: "regions/us-central1/urlMaps/ilb"}).Do())
}

// internalRule returns a regional internal forwarding rule on "sub".
func internalRule(name, ip, port string) *computev1.ForwardingRule {
	return &computev1.ForwardingRule{Name: name, LoadBalancingScheme: "INTERNAL_MANAGED", IPAddress: ip, PortRange: port,
		Network: "global/networks/vpc", Subnetwork: "regions/us-central1/subnetworks/sub", Target: "regions/us-central1/targetHttpProxies/ilb"}
}

// reserve reserves internal Address name on "sub", asking for ip.
func reserve(e *env, name, ip, purpose string) (*computev1.Operation, error) {
	return e.c.Addresses.Insert(proj, "us-central1", &computev1.Address{Name: name, AddressType: "INTERNAL", Purpose: purpose,
		Subnetwork: "regions/us-central1/subnetworks/sub", Address: ip}).Do()
}

// An ephemeral internal forwarding rule's address can't be reserved as an
// Address until the rule is deleted (#825).
func TestRuleIPHeldUntilDeleted(t *testing.T) {
	e := start(t)
	internalLB(e)
	e.do(e.c.ForwardingRules.Insert(proj, "us-central1", internalRule("fr", "", "8080")).Do())
	fr, err := e.c.ForwardingRules.Get(proj, "us-central1", "fr").Do()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reserve(e, "a", fr.IPAddress, ""); errReason(err) != "ipInUseByAnotherResource" {
		t.Fatalf("reserving the rule's address %s: %v; want ipInUseByAnotherResource", fr.IPAddress, err)
	}
	e.do(e.c.ForwardingRules.Delete(proj, "us-central1", "fr").Do())
	e.do(reserve(e, "a", fr.IPAddress, ""))
}

// A global and a regional internal forwarding rule on one subnetwork never
// get the same ephemeral address (#825).
func TestGlobalAndRegionalRuleIPs(t *testing.T) {
	e := start(t)
	c := e.c
	internalLB(e)
	e.do(c.BackendServices.Insert(proj, &computev1.BackendService{Name: "gilb", LoadBalancingScheme: "INTERNAL_MANAGED", Protocol: "HTTP"}).Do())
	e.do(c.UrlMaps.Insert(proj, &computev1.UrlMap{Name: "gilb", DefaultService: "global/backendServices/gilb"}).Do())
	e.do(c.TargetHttpProxies.Insert(proj, &computev1.TargetHttpProxy{Name: "gilb", UrlMap: "global/urlMaps/gilb"}).Do())
	e.do(c.ForwardingRules.Insert(proj, "us-central1", internalRule("fr", "", "8080")).Do())
	e.do(c.GlobalForwardingRules.Insert(proj, &computev1.ForwardingRule{Name: "g", LoadBalancingScheme: "INTERNAL_MANAGED", PortRange: "8080",
		Network: "global/networks/vpc", Subnetwork: "projects/" + proj + "/regions/us-central1/subnetworks/sub", Target: "global/targetHttpProxies/gilb"}).Do())
	r, _ := c.ForwardingRules.Get(proj, "us-central1", "fr").Do()
	g, err := c.GlobalForwardingRules.Get(proj, "g").Do()
	if err != nil || r.IPAddress == g.IPAddress {
		t.Fatalf("regional rule %s, global rule %s (%v); want different addresses", r.IPAddress, g.IPAddress, err)
	}
	if _, err := reserve(e, "a", g.IPAddress, ""); errReason(err) != "ipInUseByAnotherResource" {
		t.Errorf("reserving the global rule's address %s: %v; want ipInUseByAnotherResource", g.IPAddress, err)
	}
}

// At startup the load balancer replaces compute's records with its stored
// internal forwarding rules': a rule gone behind compute's back frees its
// address, and a rule compute has no record of is recorded (#825).
func TestRuleIPsResyncedAtStart(t *testing.T) {
	e := start(t)
	internalLB(e)
	e.do(e.c.ForwardingRules.Insert(proj, "us-central1", internalRule("gone", "", "8080")).Do())
	e.do(e.c.ForwardingRules.Insert(proj, "us-central1", internalRule("kept", "", "8080")).Do())
	gone, _ := e.c.ForwardingRules.Get(proj, "us-central1", "gone").Do()
	kept, _ := e.c.ForwardingRules.Get(proj, "us-central1", "kept").Do()
	env := e.inst.Internal().Env
	rules := "projects/" + proj + "/regions/us-central1/forwardingRules/"
	if err := env.Store.Update(func(tx store.Tx) error {
		if err := tx.Delete("lb/forwardingRules", rules+"gone"); err != nil {
			return err
		}
		return tx.Delete("compute/ruleips", rules+"kept")
	}); err != nil {
		t.Fatal(err)
	}
	svc, _ := env.Lookup("lb")
	if err := lb.ResyncRuleIPs(context.Background(), svc); err != nil {
		t.Fatal(err)
	}
	e.do(reserve(e, "a", gone.IPAddress, ""))
	if _, err := reserve(e, "b", kept.IPAddress, ""); errReason(err) != "ipInUseByAnotherResource" {
		t.Errorf("reserving the kept rule's address %s: %v; want ipInUseByAnotherResource", kept.IPAddress, err)
	}
}
