package dns

import (
	"fmt"
	"math/rand/v2"

	mdns "github.com/miekg/dns"
	dnsv1 "google.golang.org/api/dns/v1"

	"github.com/linuxuser586/gcpemu/internal/apierr"
)

// Routing policies (FR-DNS-005): weighted round robin picks an item at
// random in proportion to its weight; geolocation (and the backup targets
// of a primary/backup policy) answers with the first item, since the
// emulator has no notion of client location or health.

func validateRoutingPolicy(rs *dnsv1.ResourceRecordSet, field string) error {
	p := rs.RoutingPolicy
	f := field + ".routingPolicy"
	n := 0
	if p.Wrr != nil {
		n++
		if len(p.Wrr.Items) == 0 {
			return errRequired(f + ".wrr.items")
		}
		for i, it := range p.Wrr.Items {
			itf := fmt.Sprintf("%s.wrr.items[%d]", f, i)
			if it.Weight < 0 {
				return errInvalid(itf+".weight", it.Weight)
			}
			if err := validateItemRrdatas(rs, it.Rrdatas, it.HealthCheckedTargets != nil, itf); err != nil {
				return err
			}
		}
	}
	if p.Geo != nil {
		n++
		if err := validateGeo(rs, p.Geo, f+".geo"); err != nil {
			return err
		}
	}
	if p.PrimaryBackup != nil {
		n++
		if p.PrimaryBackup.BackupGeoTargets != nil {
			if err := validateGeo(rs, p.PrimaryBackup.BackupGeoTargets, f+".primaryBackup.backupGeoTargets"); err != nil {
				return err
			}
		}
	}
	if n != 1 {
		return apierr.InvalidArgument("The routing policy '%s' must specify exactly one of wrr, geo or primaryBackup.", f)
	}
	return nil
}

func validateGeo(rs *dnsv1.ResourceRecordSet, g *dnsv1.RRSetRoutingPolicyGeoPolicy, f string) error {
	if len(g.Items) == 0 {
		return errRequired(f + ".items")
	}
	for i, it := range g.Items {
		itf := fmt.Sprintf("%s.items[%d]", f, i)
		if it.Location == "" {
			return errRequired(itf + ".location")
		}
		if err := validateItemRrdatas(rs, it.Rrdatas, it.HealthCheckedTargets != nil, itf); err != nil {
			return err
		}
	}
	return nil
}

func validateItemRrdatas(rs *dnsv1.ResourceRecordSet, rrdatas []string, healthChecked bool, f string) error {
	if len(rrdatas) == 0 {
		if healthChecked {
			return nil // internal LB targets; nothing to answer with locally
		}
		return errRequired(f + ".rrdatas")
	}
	return validateRrdatas(rs.Name, rs.Ttl, rs.Type, rrdatas, f+".rrdatas")
}

// policyChoice holds pre-parsed routing policy items for the query path.
type policyChoice struct {
	weights []float64
	items   [][]mdns.RR
	wrr     bool
}

func buildPolicy(rs *dnsv1.ResourceRecordSet) *policyChoice {
	p := rs.RoutingPolicy
	pc := &policyChoice{}
	parse := func(rrdatas []string) []mdns.RR {
		var out []mdns.RR
		for _, d := range rrdatas {
			if rr, err := parseRR(rs.Name, rs.Ttl, rs.Type, d); err == nil {
				out = append(out, rr)
			}
		}
		return out
	}
	switch {
	case p.Wrr != nil:
		pc.wrr = true
		for _, it := range p.Wrr.Items {
			pc.weights = append(pc.weights, it.Weight)
			pc.items = append(pc.items, parse(it.Rrdatas))
		}
	case p.Geo != nil:
		for _, it := range p.Geo.Items {
			pc.items = append(pc.items, parse(it.Rrdatas))
		}
	case p.PrimaryBackup != nil && p.PrimaryBackup.BackupGeoTargets != nil:
		for _, it := range p.PrimaryBackup.BackupGeoTargets.Items {
			pc.items = append(pc.items, parse(it.Rrdatas))
		}
	}
	return pc
}

// pick returns the records to answer with for one query.
func (pc *policyChoice) pick() []mdns.RR {
	if len(pc.items) == 0 {
		return nil
	}
	if !pc.wrr {
		return pc.items[0]
	}
	var total float64
	for _, w := range pc.weights {
		total += w
	}
	if total <= 0 {
		return pc.items[rand.IntN(len(pc.items))]
	}
	x := rand.Float64() * total
	for i, w := range pc.weights {
		if x < w {
			return pc.items[i]
		}
		x -= w
	}
	return pc.items[len(pc.items)-1]
}
