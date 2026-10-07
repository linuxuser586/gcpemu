package dns

import (
	"encoding/json"
	"net/http"
	"strings"

	dnsv1 "google.golang.org/api/dns/v1"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// ResourceRecordSets CRUD (FR-DNS-002). Each mutation is applied through
// applyChange, so it obeys the same rules and is recorded as a change.

func (s *Service) createRRSet(w http.ResponseWriter, r *http.Request) {
	zr, err := s.zoneFor(r, "dns.resourceRecordSets.create")
	if err != nil {
		apierr.Write(w, err)
		return
	}
	var rs dnsv1.ResourceRecordSet
	if err := decode(r, &rs); err != nil {
		apierr.Write(w, err)
		return
	}
	err = s.update(func(tx store.Tx) error {
		_, err := s.applyChange(tx, zr.Project, zr.Zone.Name, []*dnsv1.ResourceRecordSet{&rs}, nil, "entity.resourceRecordSet")
		return fixField(err, "entity.resourceRecordSet.additions[0]", "entity.resourceRecordSet")
	})
	if err != nil {
		apierr.Write(w, err)
		return
	}
	writeJSON(w, http.StatusOK, &rs)
}

// fixField rewrites change-relative field paths in error messages to the
// rrset-relative ones the rrsets methods report.
func fixField(err error, from, to string) error {
	if err == nil {
		return nil
	}
	if e, ok := err.(*apierr.Error); ok {
		e.Message = strings.ReplaceAll(e.Message, from, to)
	}
	return err
}

// rrsetParams loads the zone and the rrset named by {name}/{type}.
func (s *Service) rrsetParams(r *http.Request, perm string) (*zoneRec, *dnsv1.ResourceRecordSet, error) {
	zr, err := s.zoneFor(r, perm)
	if err != nil {
		return nil, nil, err
	}
	name, typ := r.PathValue("name"), r.PathValue("type")
	var rs dnsv1.ResourceRecordSet
	err = s.env.Store.View(func(tx store.Tx) error {
		return store.GetJSON(tx, nsRRSets, rrsetKey(zoneKey(zr.Project, zr.Zone.Name), name, typ), &rs)
	})
	if err != nil {
		return nil, nil, errNotFound("parameters.name", name+" ("+typ+")")
	}
	return zr, &rs, nil
}

func (s *Service) getRRSet(w http.ResponseWriter, r *http.Request) {
	_, rs, err := s.rrsetParams(r, "dns.resourceRecordSets.get")
	if err != nil {
		apierr.Write(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rs)
}

func (s *Service) patchRRSet(w http.ResponseWriter, r *http.Request) {
	zr, old, err := s.rrsetParams(r, "dns.resourceRecordSets.update")
	if err != nil {
		apierr.Write(w, err)
		return
	}
	var raw map[string]json.RawMessage
	if err := decode(r, &raw); err != nil {
		apierr.Write(w, err)
		return
	}
	var in dnsv1.ResourceRecordSet
	b, _ := json.Marshal(raw)
	if err := json.Unmarshal(b, &in); err != nil {
		apierr.Write(w, apierr.InvalidArgument("Invalid JSON payload received. %v", err).WithLegacy("parseError"))
		return
	}
	nrs := *old
	if _, ok := raw["name"]; ok && in.Name != "" {
		nrs.Name = in.Name
	}
	if _, ok := raw["type"]; ok && in.Type != "" {
		nrs.Type = in.Type
	}
	if _, ok := raw["ttl"]; ok {
		nrs.Ttl = in.Ttl
	}
	if _, ok := raw["rrdatas"]; ok {
		nrs.Rrdatas = in.Rrdatas
		if _, rp := raw["routingPolicy"]; !rp {
			nrs.RoutingPolicy = nil
		}
	}
	if _, ok := raw["routingPolicy"]; ok {
		nrs.RoutingPolicy = in.RoutingPolicy
		if _, rd := raw["rrdatas"]; !rd {
			nrs.Rrdatas = nil
		}
	}
	if _, ok := raw["signatureRrdatas"]; ok {
		nrs.SignatureRrdatas = in.SignatureRrdatas
	}
	err = s.update(func(tx store.Tx) error {
		_, err := s.applyChange(tx, zr.Project, zr.Zone.Name, []*dnsv1.ResourceRecordSet{&nrs}, []*dnsv1.ResourceRecordSet{old}, "entity.resourceRecordSet")
		return fixField(err, "entity.resourceRecordSet.additions[0]", "entity.resourceRecordSet")
	})
	if err != nil {
		apierr.Write(w, err)
		return
	}
	writeJSON(w, http.StatusOK, &nrs)
}

func (s *Service) deleteRRSet(w http.ResponseWriter, r *http.Request) {
	zr, old, err := s.rrsetParams(r, "dns.resourceRecordSets.delete")
	if err != nil {
		apierr.Write(w, err)
		return
	}
	err = s.update(func(tx store.Tx) error {
		_, err := s.applyChange(tx, zr.Project, zr.Zone.Name, nil, []*dnsv1.ResourceRecordSet{old}, "entity.resourceRecordSet")
		return err
	})
	if err != nil {
		apierr.Write(w, err)
		return
	}
	writeJSON(w, http.StatusOK, struct{}{})
}

func (s *Service) listRRSets(w http.ResponseWriter, r *http.Request) {
	zr, err := s.zoneFor(r, "dns.resourceRecordSets.list")
	if err != nil {
		apierr.Write(w, err)
		return
	}
	q := r.URL.Query()
	name, typ := q.Get("name"), q.Get("type")
	if typ != "" && name == "" {
		apierr.Write(w, apierr.InvalidArgument("Invalid value for 'parameters.type': the 'name' parameter must be specified with 'type'.").WithLegacy("invalid"))
		return
	}
	zk := zoneKey(zr.Project, zr.Zone.Name)
	var items []keyed[*dnsv1.ResourceRecordSet]
	_ = s.env.Store.View(func(tx store.Tx) error {
		tx.Scan(nsRRSets, zk+"/", func(k string, b []byte) bool {
			var rs dnsv1.ResourceRecordSet
			if json.Unmarshal(b, &rs) != nil {
				return true
			}
			if (name == "" || strings.EqualFold(rs.Name, name)) && (typ == "" || rs.Type == typ) {
				items = append(items, keyed[*dnsv1.ResourceRecordSet]{k, &rs})
			}
			return true
		})
		return nil
	})
	page, next, err := paginate(r, items)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	if page == nil {
		page = []*dnsv1.ResourceRecordSet{}
	}
	writeJSON(w, http.StatusOK, &dnsv1.ResourceRecordSetsListResponse{Kind: kindRRSetList, Rrsets: page, NextPageToken: next})
}
