package dns

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	mdns "github.com/miekg/dns"
	dnsv1 "google.golang.org/api/dns/v1"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// Changes (FR-DNS-002): a change's deletions and additions are applied
// atomically in one store transaction with Cloud DNS's semantics.

// applyChange validates and applies a change to the zone inside tx and
// records it, returning the stored change. field prefixes error locations.
func (s *Service) applyChange(tx store.Tx, proj, zone string, add, del []*dnsv1.ResourceRecordSet, field string) (*dnsv1.Change, error) {
	zk := zoneKey(proj, zone)
	var zr zoneRec
	if err := store.GetJSON(tx, nsZones, zk, &zr); err != nil {
		return nil, errNotFound("parameters.managedZone", zone)
	}
	origin := zr.Zone.DnsName
	if len(add) == 0 && len(del) == 0 {
		return nil, apierr.InvalidArgument("The 'entity.change' resource must contain at least one addition or deletion.").WithLegacy("required")
	}
	for i, rs := range add {
		if err := validateRRSet(rs, fmt.Sprintf("%s.additions[%d]", field, i), origin); err != nil {
			return nil, err
		}
		rs.Kind = kindRRSet
	}
	for i, rs := range del {
		if rs == nil || rs.Name == "" || rs.Type == "" {
			return nil, errRequired(fmt.Sprintf("%s.deletions[%d]", field, i))
		}
		rs.Kind = kindRRSet
	}

	cur := map[string]*dnsv1.ResourceRecordSet{}
	tx.Scan(nsRRSets, zk+"/", func(k string, b []byte) bool {
		var rs dnsv1.ResourceRecordSet
		if json.Unmarshal(b, &rs) == nil {
			cur[k] = &rs
		}
		return true
	})
	touched := map[string]bool{}
	for i, rs := range del {
		k := rrsetKey(zk, rs.Name, rs.Type)
		f := fmt.Sprintf("%s.deletions[%d]", field, i)
		ex, ok := cur[k]
		if !ok {
			return nil, errNotFound(f, rrsetLabel(rs))
		}
		if !sameRRSet(ex, rs) {
			return nil, errConditionNotMet(f)
		}
		delete(cur, k)
		touched[k] = true
	}
	soaChanged := false
	for i, rs := range add {
		k := rrsetKey(zk, rs.Name, rs.Type)
		if _, ok := cur[k]; ok {
			return nil, errExists(fmt.Sprintf("%s.additions[%d]", field, i), rrsetLabel(rs))
		}
		cur[k] = rs
		touched[k] = true
		soaChanged = soaChanged || rs.Type == "SOA"
	}
	if err := checkZoneInvariants(cur, origin, add, field); err != nil {
		return nil, err
	}
	// Cloud DNS bumps the SOA serial on every change that leaves it alone.
	if !soaChanged {
		if k := rrsetKey(zk, origin, "SOA"); cur[k] != nil {
			cur[k] = bumpSerial(cur[k])
			touched[k] = true
		}
	}
	for k := range touched {
		var err error
		if rs, ok := cur[k]; ok {
			err = store.PutJSON(tx, nsRRSets, k, rs)
		} else {
			err = tx.Delete(nsRRSets, k)
		}
		if err != nil {
			return nil, err
		}
	}

	now := s.env.Clock.Now()
	ch := &dnsv1.Change{
		Kind: kindChange, Id: strconv.FormatInt(zr.NextChange, 10),
		Additions: add, Deletions: del, StartTime: rfc3339(now),
	}
	rec := changeRec{Change: ch, DoneAt: now.Add(s.env.Config.LRO("dns"))}
	if err := store.PutJSON(tx, nsChanges, seqKey(zk, zr.NextChange), rec); err != nil {
		return nil, err
	}
	zr.NextChange++
	if err := store.PutJSON(tx, nsZones, zk, zr); err != nil {
		return nil, err
	}
	return s.withStatus(rec), nil
}

// checkZoneInvariants enforces the zone-level rules after a change: one
// SOA and an NS set at the apex, and no CNAME next to other data.
func checkZoneInvariants(cur map[string]*dnsv1.ResourceRecordSet, origin string, add []*dnsv1.ResourceRecordSet, field string) error {
	var hasSOA, hasNS bool
	types := map[string][]string{}
	for _, rs := range cur {
		n := strings.ToLower(rs.Name)
		types[n] = append(types[n], rs.Type)
		if strings.EqualFold(rs.Name, origin) {
			hasSOA = hasSOA || rs.Type == "SOA"
			hasNS = hasNS || rs.Type == "NS"
		}
	}
	if !hasSOA || !hasNS {
		return apierr.InvalidArgument("The zone apex record sets (SOA and NS) of '%s' cannot be deleted.", origin).WithLegacy("headOfZoneRecordSetDeletion")
	}
	for i, rs := range add {
		ts := types[strings.ToLower(rs.Name)]
		if len(ts) > 1 && (rs.Type == "CNAME" || contains(ts, "CNAME")) {
			return apierr.AlreadyExists("The resource '%s.additions[%d]' named '%s' conflicts with an existing CNAME resource record set or with other data at the same name.", field, i, rrsetLabel(rs)).
				WithLegacy("cnameResourceRecordSetConflict")
		}
	}
	return nil
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// bumpSerial returns a copy of an SOA rrset with its serial incremented.
func bumpSerial(rs *dnsv1.ResourceRecordSet) *dnsv1.ResourceRecordSet {
	if len(rs.Rrdatas) != 1 {
		return rs
	}
	rr, err := parseRR(rs.Name, rs.Ttl, "SOA", rs.Rrdatas[0])
	if err != nil {
		return rs
	}
	soa := rr.(*mdns.SOA)
	cp := *rs
	cp.Rrdatas = []string{fmt.Sprintf("%s %s %d %d %d %d %d", soa.Ns, soa.Mbox, soa.Serial+1, soa.Refresh, soa.Retry, soa.Expire, soa.Minttl)}
	return &cp
}

// withStatus returns the change with status derived from its LRO latency.
func (s *Service) withStatus(rec changeRec) *dnsv1.Change {
	ch := *rec.Change
	if s.env.Clock.Now().Before(rec.DoneAt) {
		ch.Status = "pending"
	} else {
		ch.Status = "done"
	}
	return &ch
}

func (s *Service) createChange(w http.ResponseWriter, r *http.Request) {
	zr, err := s.zoneFor(r, "dns.changes.create")
	if err != nil {
		apierr.Write(w, err)
		return
	}
	var in dnsv1.Change
	if err := decode(r, &in); err != nil {
		apierr.Write(w, err)
		return
	}
	res := zoneResource(zr.Project, zr.Zone.Name)
	if len(in.Additions) > 0 {
		if err := s.env.Auth.Check(r.Context(), "dns.resourceRecordSets.create", res); err != nil {
			apierr.Write(w, err)
			return
		}
	}
	if len(in.Deletions) > 0 {
		if err := s.env.Auth.Check(r.Context(), "dns.resourceRecordSets.delete", res); err != nil {
			apierr.Write(w, err)
			return
		}
	}
	var ch *dnsv1.Change
	err = s.update(func(tx store.Tx) error {
		ch, err = s.applyChange(tx, zr.Project, zr.Zone.Name, in.Additions, in.Deletions, "entity.change")
		return err
	})
	if err != nil {
		apierr.Write(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ch)
}

func (s *Service) getChange(w http.ResponseWriter, r *http.Request) {
	zr, err := s.zoneFor(r, "dns.changes.get")
	if err != nil {
		apierr.Write(w, err)
		return
	}
	id := r.PathValue("changeId")
	n, perr := strconv.ParseInt(id, 10, 64)
	var rec changeRec
	err = s.env.Store.View(func(tx store.Tx) error {
		if perr != nil || n < 0 {
			return store.ErrNotFound
		}
		return store.GetJSON(tx, nsChanges, seqKey(zoneKey(zr.Project, zr.Zone.Name), n), &rec)
	})
	if err != nil {
		apierr.Write(w, errNotFound("parameters.changeId", id))
		return
	}
	writeJSON(w, http.StatusOK, s.withStatus(rec))
}

func (s *Service) listChanges(w http.ResponseWriter, r *http.Request) {
	zr, err := s.zoneFor(r, "dns.changes.list")
	if err != nil {
		apierr.Write(w, err)
		return
	}
	q := r.URL.Query()
	if sb := q.Get("sortBy"); sb != "" && sb != "changeSequence" {
		apierr.Write(w, errInvalid("parameters.sortBy", sb))
		return
	}
	desc := false
	switch so := q.Get("sortOrder"); so {
	case "", "ascending":
	case "descending":
		desc = true
	default:
		apierr.Write(w, errInvalid("parameters.sortOrder", so))
		return
	}
	zk := zoneKey(zr.Project, zr.Zone.Name)
	var items []keyed[*dnsv1.Change]
	_ = s.env.Store.View(func(tx store.Tx) error {
		tx.Scan(nsChanges, zk+"/", func(k string, b []byte) bool {
			var rec changeRec
			if json.Unmarshal(b, &rec) == nil {
				key := k
				if desc {
					n, _ := strconv.ParseInt(rec.Change.Id, 10, 64)
					key = fmt.Sprintf("%012d", 999999999999-n)
				}
				items = append(items, keyed[*dnsv1.Change]{key, s.withStatus(rec)})
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
		page = []*dnsv1.Change{}
	}
	writeJSON(w, http.StatusOK, &dnsv1.ChangesListResponse{Kind: kindChangeList, Changes: page, NextPageToken: next})
}
