package dns

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	dnsv1 "google.golang.org/api/dns/v1"
	"gopkg.in/yaml.v3"

	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// Seed file section (FR-CORE-011):
//
//	dns:
//	  project: my-project            # default for zones without one
//	  zones:
//	    - name: example
//	      project: my-project
//	      dnsName: example.test.
//	      description: Example zone
//	      visibility: private        # default public
//	      networks: [default]        # private zones; names or network URLs
//	      labels: {env: dev}
//	      records:
//	        - name: www              # relative to dnsName; "@" is the apex
//	          type: A
//	          ttl: 300
//	          rrdatas: [192.0.2.10]
//
// Zones are created when absent; records are upserted (an existing rrset of
// the same name and type is replaced), so applying a seed is idempotent.

var _ emu.SeedExporter = (*Service)(nil)

type seedSection struct {
	Project string     `yaml:"project,omitempty"`
	Zones   []seedZone `yaml:"zones"`
}

type seedZone struct {
	Name        string            `yaml:"name"`
	Project     string            `yaml:"project,omitempty"`
	DNSName     string            `yaml:"dnsName"`
	Description string            `yaml:"description,omitempty"`
	Visibility  string            `yaml:"visibility,omitempty"`
	Networks    []string          `yaml:"networks,omitempty"`
	Labels      map[string]string `yaml:"labels,omitempty"`
	Records     []seedRecord      `yaml:"records,omitempty"`
}

type seedRecord struct {
	Name    string   `yaml:"name"`
	Type    string   `yaml:"type"`
	TTL     int64    `yaml:"ttl,omitempty"`
	Rrdatas []string `yaml:"rrdatas"`
}

// ApplySeed implements emu.Seeder.
func (s *Service) ApplySeed(ctx context.Context, section *yaml.Node, baseDir string) error {
	var sec seedSection
	if err := section.Decode(&sec); err != nil {
		return err
	}
	for i, sz := range sec.Zones {
		proj := sz.Project
		if proj == "" {
			proj = sec.Project
		}
		if proj == "" {
			return fmt.Errorf("zones[%d]: project is required", i)
		}
		if err := s.env.EnsureProject(proj); err != nil {
			return fmt.Errorf("zones[%d]: %w", i, err)
		}
		dnsName := mdnsFqdn(sz.DNSName)
		if err := s.seedZone(ctx, proj, sz, dnsName); err != nil {
			return fmt.Errorf("zones[%d] %s: %w", i, sz.Name, err)
		}
	}
	return nil
}

// ExportSeed implements emu.SeedExporter: every managed zone with its
// record sets. The SOA record (whose serial every change bumps) and an
// apex NS record the zone was created with are left out, as creating the
// zone makes them. Routing-policy record sets, TTLs of 0, forwarding,
// peering, DNSSEC and logging configs, GKE cluster bindings, DNS policies
// and response policies have no seed form and are not exported.
func (s *Service) ExportSeed(ctx context.Context) (any, error) {
	var sec seedSection
	err := s.env.Store.View(func(tx store.Tx) error {
		zones, err := store.ListJSON[zoneRec](tx, nsZones, "")
		if err != nil {
			return err
		}
		for _, zr := range zones {
			z := zr.Zone
			sz := seedZone{Name: z.Name, Project: zr.Project, DNSName: z.DnsName, Description: z.Description, Labels: z.Labels}
			if z.Visibility == visibilityPriv {
				sz.Visibility = visibilityPriv
			}
			if pvc := z.PrivateVisibilityConfig; pvc != nil {
				for _, n := range pvc.Networks {
					ref := n.NetworkUrl
					if m := networkRE.FindStringSubmatch(ref); m != nil {
						ref = "projects/" + m[1] + "/global/networks/" + m[2]
						if m[1] == zr.Project {
							ref = m[2]
						}
					}
					sz.Networks = append(sz.Networks, ref)
				}
			}
			apexNS := apexRecords(z)[0]
			tx.Scan(nsRRSets, zoneKey(zr.Project, z.Name)+"/", func(_ string, b []byte) bool {
				var rs dnsv1.ResourceRecordSet
				if json.Unmarshal(b, &rs) != nil || rs.RoutingPolicy != nil {
					return true
				}
				if rs.Type == "SOA" || (rs.Type == "NS" && sameRRSet(&rs, apexNS)) {
					return true
				}
				ttl := rs.Ttl
				if ttl == 300 {
					ttl = 0
				}
				sz.Records = append(sz.Records, seedRecord{Name: relName(rs.Name, z.DnsName), Type: rs.Type, TTL: ttl, Rrdatas: rs.Rrdatas})
				return true
			})
			slices.SortFunc(sz.Records, func(a, b seedRecord) int {
				return cmp.Or(cmp.Compare(a.Name, b.Name), cmp.Compare(a.Type, b.Type))
			})
			sec.Zones = append(sec.Zones, sz)
		}
		return nil
	})
	if err != nil || len(sec.Zones) == 0 {
		return nil, err
	}
	slices.SortFunc(sec.Zones, func(a, b seedZone) int {
		return cmp.Or(cmp.Compare(a.Project, b.Project), cmp.Compare(a.Name, b.Name))
	})
	return &sec, nil
}

// relName is absName's inverse: name relative to origin, "@" for the apex.
func relName(name, origin string) string {
	switch {
	case strings.EqualFold(name, origin):
		return "@"
	case strings.HasSuffix(strings.ToLower(name), "."+strings.ToLower(origin)):
		return name[:len(name)-len(origin)-1]
	default:
		return name
	}
}

func mdnsFqdn(s string) string {
	if s != "" && !strings.HasSuffix(s, ".") {
		return s + "."
	}
	return s
}

func (s *Service) seedZone(ctx context.Context, proj string, sz seedZone, dnsName string) error {
	var exists bool
	_ = s.env.Store.View(func(tx store.Tx) error {
		exists = store.Exists(tx, nsZones, zoneKey(proj, sz.Name))
		return nil
	})
	if !exists {
		z := &dnsv1.ManagedZone{
			Name: sz.Name, DnsName: dnsName, Description: sz.Description,
			Visibility: sz.Visibility, Labels: sz.Labels,
		}
		if len(sz.Networks) > 0 {
			z.Visibility = visibilityPriv
			z.PrivateVisibilityConfig = &dnsv1.ManagedZonePrivateVisibilityConfig{}
			for _, n := range sz.Networks {
				if !strings.Contains(n, "/") {
					n = "projects/" + proj + "/global/networks/" + n
				}
				z.PrivateVisibilityConfig.Networks = append(z.PrivateVisibilityConfig.Networks,
					&dnsv1.ManagedZonePrivateVisibilityConfigNetwork{NetworkUrl: n})
			}
		}
		if _, err := s.CreateZone(proj, z); err != nil {
			return err
		}
	}
	for _, rec := range sz.Records {
		rs := &dnsv1.ResourceRecordSet{
			Name: absName(rec.Name, dnsName), Type: strings.ToUpper(rec.Type),
			Ttl: rec.TTL, Rrdatas: rec.Rrdatas,
		}
		if rs.Ttl == 0 {
			rs.Ttl = 300
		}
		if err := s.UpsertRRSet(proj, sz.Name, rs); err != nil {
			return fmt.Errorf("record %s %s: %w", rs.Name, rs.Type, err)
		}
	}
	return nil
}

// absName makes a seed record name absolute within origin.
func absName(name, origin string) string {
	switch {
	case name == "" || name == "@":
		return origin
	case strings.HasSuffix(name, "."):
		return name
	default:
		return name + "." + origin
	}
}

// UpsertRRSet creates or replaces an rrset in a zone as one change. It does
// not check IAM; other services (e.g. the LB registering A records for
// forwarding rules) may use it.
func (s *Service) UpsertRRSet(proj, zone string, rs *dnsv1.ResourceRecordSet) error {
	return s.update(func(tx store.Tx) error {
		var del []*dnsv1.ResourceRecordSet
		var cur dnsv1.ResourceRecordSet
		if store.GetJSON(tx, nsRRSets, rrsetKey(zoneKey(proj, zone), rs.Name, rs.Type), &cur) == nil {
			if sameRRSet(&cur, rs) {
				return nil
			}
			del = append(del, &cur)
		}
		_, err := s.applyChange(tx, proj, zone, []*dnsv1.ResourceRecordSet{rs}, del, "entity.change")
		return err
	})
}
