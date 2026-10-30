package compute

import (
	"context"
	"encoding/json"
	"slices"

	computev1 "google.golang.org/api/compute/v1"

	"github.com/linuxuser586/gcpemu/internal/store"
)

// Internal forwarding rule addresses (ADR 0044): the load balancer stores
// forwarding rules, but compute records each internal rule's address
// against the rule's subnetwork, so that every internal allocation on the
// subnetwork skips it (#825).

// nsRuleIPs holds "RULE" → ruleIP, RULE being the forwarding rule's path.
const nsRuleIPs = "compute/ruleips"

type ruleIP struct {
	Subnetwork string `json:"subnetwork"` // subnetwork path
	IP         string `json:"ip"`
}

// RuleIP is an internal forwarding rule's address. Subnetwork is the
// reference the rule gives ("" is its region's default subnetwork; a
// global rule must give its region), Rule the rule's path.
type RuleIP struct {
	Rule, Subnetwork, IP string
}

// ruleSubnet resolves the subnetwork path of a rule's subnetwork
// reference: regional rules default to their region's "default".
func ruleSubnet(rule, subnetwork string) (string, bool) {
	project, loc, _ := pathParts(relPath(rule))
	region := loc
	if loc == "global" {
		region = ""
	}
	ref := subnetwork
	if ref == "" {
		ref = "default"
	}
	sp, err := regionalRef(project, region, "subnetworks", ref)
	return sp, err == nil
}

// ruleSubnetwork loads the subnetwork of a rule's subnetwork reference.
func ruleSubnetwork(tx store.Tx, rule, subnetwork string) (string, *computev1.Subnetwork, error) {
	sp, ok := ruleSubnet(rule, subnetwork)
	if !ok {
		return "", nil, errInvalidField("resource.subnetwork", subnetwork, "The URL is malformed.")
	}
	sn, ok := get[computev1.Subnetwork](tx, nsSubnets, sp)
	if !ok {
		return "", nil, errInvalidField("resource.subnetwork", subnetwork, "The referenced subnetwork resource cannot be found.")
	}
	return sp, sn, nil
}

// recordRuleIP records r unless its address is that of an internal
// Address on the subnetwork, which already holds it; then it drops the
// rule's record.
func recordRuleIP(tx store.Tx, r RuleIP) error {
	rule := relPath(r.Rule)
	sp, ok := ruleSubnet(rule, r.Subnetwork)
	if _, valid := parseIP(r.IP); !ok || !valid || r.IP == "0.0.0.0" || addressOn(tx, sp, r.IP) != nil {
		return tx.Delete(nsRuleIPs, rule)
	}
	return store.PutJSON(tx, nsRuleIPs, rule, &ruleIP{Subnetwork: sp, IP: r.IP})
}

// addressOn returns the internal Address holding ip on subnetwork sp.
func addressOn(tx store.Tx, sp, ip string) *computev1.Address {
	p, region, _ := pathParts(sp)
	for _, a := range list[computev1.Address](tx, nsAddresses, "projects/"+p+"/regions/"+region+"/addresses/") {
		if a.Address == ip && relPath(a.Subnetwork) == sp {
			return a
		}
	}
	return nil
}

// ruleIPs returns the addresses recorded on subnetwork sp, each with its
// rule's path.
func ruleIPs(tx store.Tx, sp string) map[uint32]string {
	out := map[uint32]string{}
	tx.Scan(nsRuleIPs, "", func(k string, b []byte) bool {
		var r ruleIP
		if json.Unmarshal(b, &r) == nil && r.Subnetwork == sp {
			if v, ok := parseIP(r.IP); ok {
				out[v] = k
			}
		}
		return true
	})
	return out
}

// ReleaseRuleIP drops the record of forwarding rule path, once it is
// deleted or no longer holds an address of its own.
func (s *Service) ReleaseRuleIP(ctx context.Context, path string) error {
	return s.env.Store.Update(func(tx store.Tx) error { return tx.Delete(nsRuleIPs, relPath(path)) })
}

// HoldRuleIP checks the address an internal forwarding rule names, and
// records it in the same transaction. It is refused (invalid on
// resource.IPAddress) outside the subnetwork's range or on one of GCP's
// reserved addresses, and (ipInUseByAnotherResource) when another
// resource holds it on the subnetwork (heldOn). An internal Address's IP
// is the exception, as in GCP: a rule may take it while the Address has
// no other users or is a SHARED_LOADBALANCER_VIP, and records nothing.
func (s *Service) HoldRuleIP(ctx context.Context, r RuleIP) error {
	rule := relPath(r.Rule)
	return s.env.Store.Update(func(tx store.Tx) error {
		sp, sn, err := ruleSubnetwork(tx, rule, r.Subnetwork)
		if err != nil {
			return err
		}
		c, _ := parseCIDR(sn.IpCidrRange, true)
		ip, ok := parseIP(r.IP)
		if !ok || !c.usable(ip) {
			return errInvalidField("resource.IPAddress", r.IP, "The specified IP address is not within the subnetwork range or is reserved.")
		}
		if a := addressOn(tx, sp, r.IP); a != nil {
			if a.Purpose != "SHARED_LOADBALANCER_VIP" && slices.ContainsFunc(a.Users, func(u string) bool { return relPath(u) != rule }) {
				return errIPInUse(r.IP)
			}
			return tx.Delete(nsRuleIPs, rule)
		}
		if h := heldOn(tx, sp, sn)[ip]; h != "" && h != rule {
			return errIPInUse(r.IP)
		}
		return recordRuleIP(tx, r)
	})
}

// RecordRuleIP records an internal forwarding rule's address as the
// load balancer stored it, replacing the rule's earlier record.
func (s *Service) RecordRuleIP(ctx context.Context, r RuleIP) error {
	return s.env.Store.Update(func(tx store.Tx) error { return recordRuleIP(tx, r) })
}

// ResetRuleIPs replaces every record with those of rules, the internal
// forwarding rules the load balancer has stored, so that a crash between
// the two services' writes leaves no stale or missing record.
func (s *Service) ResetRuleIPs(ctx context.Context, rules []RuleIP) error {
	return s.env.Store.Update(func(tx store.Tx) error {
		var keys []string
		tx.Scan(nsRuleIPs, "", func(k string, _ []byte) bool { keys = append(keys, k); return true })
		for _, k := range keys {
			if err := tx.Delete(nsRuleIPs, k); err != nil {
				return err
			}
		}
		for _, r := range rules {
			if err := recordRuleIP(tx, r); err != nil {
				return err
			}
		}
		return nil
	})
}

// dropSubnetRuleIPs removes the records of subnetwork sp as it is deleted.
func dropSubnetRuleIPs(tx store.Tx, sp string) error {
	for _, rule := range ruleIPs(tx, sp) {
		if err := tx.Delete(nsRuleIPs, rule); err != nil {
			return err
		}
	}
	return nil
}
