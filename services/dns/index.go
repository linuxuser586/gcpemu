package dns

import (
	"encoding/json"
	"sort"
	"strings"

	mdns "github.com/miekg/dns"
	dnsv1 "google.golang.org/api/dns/v1"

	"github.com/linuxuser586/gcpemu/internal/store"
)

// index is the immutable in-memory view of every zone used by the query
// path. It is rebuilt from the store whenever the store generation changes.
type index struct {
	// zones maps a lower-case origin to the zones serving it, private
	// zones first, then by project/name.
	zones map[string][]*zoneIdx
}

type zoneIdx struct {
	project, name string
	origin        string
	private       bool
	networks      map[string]bool // bound networks, "projects/P/global/networks/N"
	soa           *mdns.SOA
	// names maps a lower-case owner name to its rrsets by type.
	names map[string]map[uint16]*rrsetIdx
	// ents holds empty non-terminals (names with descendants but no data).
	ents map[string]bool
}

type rrsetIdx struct {
	rrs    []mdns.RR
	policy *policyChoice
}

// records returns the records to answer with (a routing policy picks per query).
func (r *rrsetIdx) records() []mdns.RR {
	if r.policy != nil {
		return r.policy.pick()
	}
	return r.rrs
}

// currentIndex returns the query index, rebuilding it if the store changed.
func (s *Service) currentIndex() *index {
	var gen string
	_ = s.env.Store.View(func(tx store.Tx) error {
		b, _ := tx.Get(nsMeta, "gen")
		gen = string(b)
		return nil
	})
	s.ixMu.Lock()
	defer s.ixMu.Unlock()
	if s.ix != nil && !s.ixDirty && gen == s.ixGen {
		return s.ix
	}
	ix := s.buildIndex()
	s.ix, s.ixGen, s.ixDirty = ix, gen, false
	return ix
}

func (s *Service) buildIndex() *index {
	ix := &index{zones: map[string][]*zoneIdx{}}
	_ = s.env.Store.View(func(tx store.Tx) error {
		zones, _ := store.ListJSON[zoneRec](tx, nsZones, "")
		for _, zr := range zones {
			z := zr.Zone
			zi := &zoneIdx{
				project: zr.Project, name: z.Name,
				origin:  strings.ToLower(z.DnsName),
				private: z.Visibility == visibilityPriv,
				names:   map[string]map[uint16]*rrsetIdx{},
				ents:    map[string]bool{},
			}
			if z.PrivateVisibilityConfig != nil {
				zi.networks = map[string]bool{}
				for _, n := range z.PrivateVisibilityConfig.Networks {
					zi.networks[normalizeNetwork(n.NetworkUrl)] = true
				}
			}
			tx.Scan(nsRRSets, zoneKey(zr.Project, z.Name)+"/", func(_ string, b []byte) bool {
				var rs dnsv1.ResourceRecordSet
				if json.Unmarshal(b, &rs) == nil {
					zi.add(&rs)
				}
				return true
			})
			zi.computeENTs()
			ix.zones[zi.origin] = append(ix.zones[zi.origin], zi)
		}
		return nil
	})
	for _, zs := range ix.zones {
		sort.SliceStable(zs, func(i, j int) bool {
			if zs[i].private != zs[j].private {
				return zs[i].private
			}
			return zs[i].project+"/"+zs[i].name < zs[j].project+"/"+zs[j].name
		})
	}
	return ix
}

func (z *zoneIdx) add(rs *dnsv1.ResourceRecordSet) {
	t, ok := mdns.StringToType[rs.Type]
	if !ok {
		return
	}
	owner := strings.ToLower(rs.Name)
	ri := &rrsetIdx{}
	if rs.RoutingPolicy != nil {
		ri.policy = buildPolicy(rs)
	} else {
		for _, d := range rs.Rrdatas {
			rr, err := parseRR(owner, rs.Ttl, rs.Type, d)
			if err != nil {
				continue
			}
			ri.rrs = append(ri.rrs, rr)
			if soa, ok := rr.(*mdns.SOA); ok && owner == z.origin {
				z.soa = soa
			}
		}
	}
	if z.names[owner] == nil {
		z.names[owner] = map[uint16]*rrsetIdx{}
	}
	z.names[owner][t] = ri
}

// computeENTs records every ancestor of an owner name, below the apex, that
// has no data of its own.
func (z *zoneIdx) computeENTs() {
	for owner := range z.names {
		for n := parent(owner); n != "" && n != z.origin && inZone(n, z.origin); n = parent(n) {
			if _, ok := z.names[n]; !ok {
				z.ents[n] = true
			}
		}
	}
}

// parent returns the parent of an absolute name, or "" for the root.
func parent(name string) string {
	if name == "." || name == "" {
		return ""
	}
	i := strings.IndexByte(name, '.')
	if i == len(name)-1 {
		return "."
	}
	return name[i+1:]
}

// findZone returns the most specific zone containing name that is visible
// on network: public zones everywhere, private zones on bound networks.
func (ix *index) findZone(name, network string) *zoneIdx {
	for n := name; n != ""; n = parent(n) {
		for _, z := range ix.zones[n] {
			if !z.private || (network != "" && z.networks[network]) {
				return z
			}
		}
	}
	return nil
}

// normalizeNetwork reduces a network URL or path to
// "projects/P/global/networks/N" ("" if it isn't one).
func normalizeNetwork(n string) string {
	if i := strings.Index(n, "projects/"); i >= 0 {
		n = n[i:]
	}
	segs := strings.Split(n, "/")
	if len(segs) != 5 || segs[0] != "projects" || segs[2] != "global" || segs[3] != "networks" {
		return ""
	}
	return n
}
