package dns

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	dnsv1 "google.golang.org/api/dns/v1"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/project"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// Managed zones (FR-DNS-001).

// projectParam validates the {project} path parameter (FR-CORE-020).
func (s *Service) projectParam(r *http.Request) (string, error) {
	p := r.PathValue("project")
	return p, s.env.EnsureProject(p)
}

// loadZone resolves a zone by name or numeric ID.
func loadZone(tx store.Tx, proj, ref string) (*zoneRec, error) {
	var zr zoneRec
	if err := store.GetJSON(tx, nsZones, zoneKey(proj, ref), &zr); err == nil {
		return &zr, nil
	}
	if id, err := strconv.ParseUint(ref, 10, 64); err == nil {
		var found *zoneRec
		tx.Scan(nsZones, proj+"/", func(_ string, b []byte) bool {
			var c zoneRec
			if json.Unmarshal(b, &c) == nil && c.Zone.Id == id {
				found = &c
				return false
			}
			return true
		})
		if found != nil {
			return found, nil
		}
	}
	return nil, errNotFound("parameters.managedZone", ref)
}

// zoneFor loads the zone named in the request after checking perm on it.
func (s *Service) zoneFor(r *http.Request, perm string) (*zoneRec, error) {
	proj, err := s.projectParam(r)
	if err != nil {
		return nil, err
	}
	ref := r.PathValue("managedZone")
	if err := s.env.Auth.Check(r.Context(), perm, zoneResource(proj, ref)); err != nil {
		return nil, err
	}
	var zr *zoneRec
	err = s.env.Store.View(func(tx store.Tx) error {
		zr, err = loadZone(tx, proj, ref)
		return err
	})
	return zr, err
}

var networkRE = regexp.MustCompile(`(?:^|/)projects/([^/]+)/global/networks/([^/]+)$`)

// normalizeZone validates a zone's mutable fields and fills defaults. With
// checkIAM it also checks the caller in ctx may bind the zone's networks.
func (s *Service) normalizeZone(ctx context.Context, z *dnsv1.ManagedZone, checkIAM bool) error {
	if len(z.Description) > 1024 {
		return errInvalid("entity.managedZone.description", z.Description)
	}
	switch z.Visibility {
	case "":
		z.Visibility = visibilityPublic
	case visibilityPublic, visibilityPriv:
	default:
		return errInvalid("entity.managedZone.visibility", z.Visibility)
	}
	if z.PrivateVisibilityConfig != nil {
		if z.Visibility != visibilityPriv {
			return apierr.InvalidArgument("Invalid value for 'entity.managedZone.privateVisibilityConfig': a public zone cannot have private visibility config.")
		}
		z.PrivateVisibilityConfig.Kind = kindPVC
		for i, n := range z.PrivateVisibilityConfig.Networks {
			m := networkRE.FindStringSubmatch(n.NetworkUrl)
			if m == nil {
				return errInvalid("entity.managedZone.privateVisibilityConfig.networks["+strconv.Itoa(i)+"].networkUrl", n.NetworkUrl)
			}
			if checkIAM {
				if err := s.env.Auth.Check(ctx, "dns.networks.bindPrivateDNSZone",
					"//compute.googleapis.com/projects/"+m[1]+"/global/networks/"+m[2]); err != nil {
					return err
				}
			}
			n.Kind = kindPVCNet
			n.NetworkUrl = "https://www.googleapis.com/compute/v1/projects/" + m[1] + "/global/networks/" + m[2]
		}
	}
	if z.CloudLoggingConfig == nil {
		z.CloudLoggingConfig = &dnsv1.ManagedZoneCloudLoggingConfig{}
	}
	z.CloudLoggingConfig.Kind = kindLogging
	z.Kind = kindZone
	return nil
}

func (s *Service) createZone(w http.ResponseWriter, r *http.Request) {
	proj, err := s.projectParam(r)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	if err := s.env.Auth.Check(r.Context(), "dns.managedZones.create", projectResource(proj)); err != nil {
		apierr.Write(w, err)
		return
	}
	var z dnsv1.ManagedZone
	if err := decode(r, &z); err != nil {
		apierr.Write(w, err)
		return
	}
	if err := s.normalizeZone(r.Context(), &z, true); err != nil {
		apierr.Write(w, err)
		return
	}
	out, err := s.CreateZone(proj, &z)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// CreateZone creates a managed zone with its automatic SOA and NS record
// sets (recorded as change "0", like Cloud DNS). It does not check IAM.
func (s *Service) CreateZone(proj string, z *dnsv1.ManagedZone) (*dnsv1.ManagedZone, error) {
	switch {
	case z.Name == "":
		return nil, errRequired("entity.managedZone.name")
	case !validZoneName(z.Name):
		return nil, errInvalid("entity.managedZone.name", z.Name)
	case z.DnsName == "":
		return nil, errRequired("entity.managedZone.dnsName")
	case !validDNSName(z.DnsName):
		return nil, errInvalid("entity.managedZone.dnsName", z.DnsName)
	}
	if err := s.normalizeZone(context.Background(), z, false); err != nil {
		return nil, err
	}
	now := s.env.Clock.Now()
	z.Id = s.env.IDs.Uint64()
	z.CreationTime = rfc3339(now)
	if z.Visibility == visibilityPriv {
		z.NameServers = []string{privateNS}
	} else {
		z.NameServers = nameServers(proj, z.Name)
	}
	zk := zoneKey(proj, z.Name)
	err := s.update(func(tx store.Tx) error {
		if store.Exists(tx, nsZones, zk) {
			return errExists("entity.managedZone", z.Name)
		}
		apex := apexRecords(z)
		for _, rs := range apex {
			if err := store.PutJSON(tx, nsRRSets, rrsetKey(zk, rs.Name, rs.Type), rs); err != nil {
				return err
			}
		}
		ch := &dnsv1.Change{Kind: kindChange, Id: "0", Additions: apex, StartTime: rfc3339(now), Status: "done"}
		if err := store.PutJSON(tx, nsChanges, seqKey(zk, 0), changeRec{Change: ch, DoneAt: now}); err != nil {
			return err
		}
		return store.PutJSON(tx, nsZones, zk, zoneRec{Project: proj, Zone: z, NextChange: 1})
	})
	if err != nil {
		return nil, err
	}
	return z, nil
}

func (s *Service) getZone(w http.ResponseWriter, r *http.Request) {
	zr, err := s.zoneFor(r, "dns.managedZones.get")
	if err != nil {
		apierr.Write(w, err)
		return
	}
	writeJSON(w, http.StatusOK, zr.Zone)
}

func (s *Service) listZones(w http.ResponseWriter, r *http.Request) {
	proj, err := s.projectParam(r)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	if err := s.env.Auth.Check(r.Context(), "dns.managedZones.list", projectResource(proj)); err != nil {
		apierr.Write(w, err)
		return
	}
	filter := r.URL.Query().Get("dnsName")
	if filter != "" && !strings.HasSuffix(filter, ".") {
		filter += "."
	}
	var items []keyed[*dnsv1.ManagedZone]
	_ = s.env.Store.View(func(tx store.Tx) error {
		recs, err := store.ListJSON[zoneRec](tx, nsZones, proj+"/")
		for _, zr := range recs {
			if filter == "" || strings.EqualFold(zr.Zone.DnsName, filter) {
				items = append(items, keyed[*dnsv1.ManagedZone]{zr.Zone.Name, zr.Zone})
			}
		}
		return err
	})
	page, next, err := paginate(r, items)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	if page == nil {
		page = []*dnsv1.ManagedZone{}
	}
	writeJSON(w, http.StatusOK, &dnsv1.ManagedZonesListResponse{Kind: kindZoneList, ManagedZones: page, NextPageToken: next})
}

func (s *Service) patchZone(w http.ResponseWriter, r *http.Request)  { s.modifyZone(w, r, true) }
func (s *Service) updateZone(w http.ResponseWriter, r *http.Request) { s.modifyZone(w, r, false) }

// modifyZone implements managedZones.patch (merge) and update (replace);
// both return a done Operation of type UPDATE.
func (s *Service) modifyZone(w http.ResponseWriter, r *http.Request, merge bool) {
	zr, err := s.zoneFor(r, "dns.managedZones.update")
	if err != nil {
		apierr.Write(w, err)
		return
	}
	var raw map[string]json.RawMessage
	if err := decode(r, &raw); err != nil {
		apierr.Write(w, err)
		return
	}
	var in dnsv1.ManagedZone
	b, _ := json.Marshal(raw)
	if err := json.Unmarshal(b, &in); err != nil {
		apierr.Write(w, apierr.InvalidArgument("Invalid JSON payload received. %v", err).WithLegacy("parseError"))
		return
	}
	old := zr.Zone
	// Immutable fields may be repeated but not changed.
	if in.Name != "" && in.Name != old.Name {
		apierr.Write(w, errInvalid("entity.managedZone.name", in.Name))
		return
	}
	if in.DnsName != "" && !strings.EqualFold(in.DnsName, old.DnsName) {
		apierr.Write(w, errInvalid("entity.managedZone.dnsName", in.DnsName))
		return
	}
	if in.Visibility != "" && in.Visibility != old.Visibility {
		apierr.Write(w, errInvalid("entity.managedZone.visibility", in.Visibility))
		return
	}
	cp := *old
	nz := &cp
	set := func(key string) bool { _, ok := raw[key]; return ok || !merge }
	if set("description") {
		nz.Description = in.Description
	}
	if set("labels") {
		nz.Labels = in.Labels
	}
	if set("privateVisibilityConfig") && old.Visibility == visibilityPriv {
		nz.PrivateVisibilityConfig = in.PrivateVisibilityConfig
	}
	if set("cloudLoggingConfig") {
		nz.CloudLoggingConfig = in.CloudLoggingConfig
	}
	if set("dnssecConfig") {
		nz.DnssecConfig = in.DnssecConfig
	}
	if set("forwardingConfig") {
		nz.ForwardingConfig = in.ForwardingConfig
	}
	if set("peeringConfig") {
		nz.PeeringConfig = in.PeeringConfig
	}
	if err := s.normalizeZone(r.Context(), nz, true); err != nil {
		apierr.Write(w, err)
		return
	}
	op := &dnsv1.Operation{
		Kind: kindOp, Type: "UPDATE", Status: "done",
		StartTime:   rfc3339(s.env.Clock.Now()),
		User:        emu.PrincipalFrom(r.Context()).Email(),
		ZoneContext: &dnsv1.OperationManagedZoneContext{OldValue: old, NewValue: nz},
	}
	zk := zoneKey(zr.Project, old.Name)
	err = s.update(func(tx store.Tx) error {
		var cur zoneRec
		if err := store.GetJSON(tx, nsZones, zk, &cur); err != nil {
			return errNotFound("parameters.managedZone", old.Name)
		}
		op.Id = strconv.FormatInt(cur.NextOp, 10)
		if err := store.PutJSON(tx, nsOps, seqKey(zk, cur.NextOp), op); err != nil {
			return err
		}
		cur.NextOp++
		cur.Zone = nz
		return store.PutJSON(tx, nsZones, zk, cur)
	})
	if err != nil {
		apierr.Write(w, err)
		return
	}
	writeJSON(w, http.StatusOK, op)
}

func (s *Service) deleteZone(w http.ResponseWriter, r *http.Request) {
	zr, err := s.zoneFor(r, "dns.managedZones.delete")
	if err != nil {
		apierr.Write(w, err)
		return
	}
	if err := s.DeleteZone(zr.Project, zr.Zone.Name); err != nil {
		apierr.Write(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// DeleteZone deletes an empty zone (only the apex SOA and NS remain);
// otherwise it fails with containerNotEmpty as Cloud DNS does.
func (s *Service) DeleteZone(proj, name string) error {
	zk := zoneKey(proj, name)
	return s.update(func(tx store.Tx) error {
		zr, err := loadZone(tx, proj, name)
		if err != nil {
			return err
		}
		var keys []string
		empty := true
		tx.Scan(nsRRSets, zk+"/", func(k string, b []byte) bool {
			var rs dnsv1.ResourceRecordSet
			_ = json.Unmarshal(b, &rs)
			if !strings.EqualFold(rs.Name, zr.Zone.DnsName) || (rs.Type != "SOA" && rs.Type != "NS") {
				empty = false
				return false
			}
			keys = append(keys, k)
			return true
		})
		if !empty {
			return errNotEmpty(name)
		}
		for _, ns := range []string{nsChanges, nsOps} {
			tx.Scan(ns, zk+"/", func(k string, _ []byte) bool { keys = append(keys, ns+"\x00"+k); return true })
		}
		for _, k := range keys {
			ns, key := nsRRSets, k
			if a, b, ok := strings.Cut(k, "\x00"); ok {
				ns, key = a, b
			}
			if err := tx.Delete(ns, key); err != nil {
				return err
			}
		}
		return tx.Delete(nsZones, zk)
	})
}

// Zone operations (minimal: patch/update record done UPDATE operations).

func (s *Service) getOperation(w http.ResponseWriter, r *http.Request) {
	zr, err := s.zoneFor(r, "dns.managedZoneOperations.get")
	if err != nil {
		apierr.Write(w, err)
		return
	}
	id := r.PathValue("operation")
	n, perr := strconv.ParseInt(id, 10, 64)
	var op dnsv1.Operation
	err = s.env.Store.View(func(tx store.Tx) error {
		if perr != nil {
			return store.ErrNotFound
		}
		return store.GetJSON(tx, nsOps, seqKey(zoneKey(zr.Project, zr.Zone.Name), n), &op)
	})
	if err != nil {
		apierr.Write(w, errNotFound("parameters.operation", id))
		return
	}
	writeJSON(w, http.StatusOK, &op)
}

func (s *Service) listOperations(w http.ResponseWriter, r *http.Request) {
	zr, err := s.zoneFor(r, "dns.managedZoneOperations.list")
	if err != nil {
		apierr.Write(w, err)
		return
	}
	zk := zoneKey(zr.Project, zr.Zone.Name)
	var items []keyed[*dnsv1.Operation]
	_ = s.env.Store.View(func(tx store.Tx) error {
		tx.Scan(nsOps, zk+"/", func(k string, b []byte) bool {
			var op dnsv1.Operation
			if json.Unmarshal(b, &op) == nil {
				items = append(items, keyed[*dnsv1.Operation]{k, &op})
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
		page = []*dnsv1.Operation{}
	}
	writeJSON(w, http.StatusOK, &dnsv1.ManagedZoneOperationsListResponse{Kind: kindOpList, Operations: page, NextPageToken: next})
}

// getProject implements projects.get with Cloud DNS's default quotas.
func (s *Service) getProject(w http.ResponseWriter, r *http.Request) {
	proj, err := s.projectParam(r)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	if err := s.env.Auth.Check(r.Context(), "dns.projects.get", projectResource(proj)); err != nil {
		apierr.Write(w, err)
		return
	}
	writeJSON(w, http.StatusOK, &dnsv1.Project{
		Kind: kindProject, Id: proj, Number: uint64(project.Number(proj)),
		Quota: &dnsv1.Quota{
			Kind: kindQuota, ManagedZones: 10000, RrsetsPerManagedZone: 10000,
			RrsetAdditionsPerChange: 1000, RrsetDeletionsPerChange: 1000,
			TotalRrdataSizePerChange: 100000, ResourceRecordsPerRrset: 100,
			NetworksPerManagedZone: 1000, ManagedZonesPerNetwork: 1000,
			Policies: 100, NetworksPerPolicy: 1000, DnsKeysPerManagedZone: 4,
			TargetNameServersPerManagedZone: 100, TargetNameServersPerPolicy: 100,
			ItemsPerRoutingPolicy: 100, GkeClustersPerManagedZone: 100,
			ManagedZonesPerGkeCluster: 1000, NameserversPerDelegation: 20,
			PeeringZonesPerTargetNetwork: 100, ResponsePolicies: 100,
			ResponsePolicyRulesPerResponsePolicy: 1000, NetworksPerResponsePolicy: 100,
			GkeClustersPerResponsePolicy: 100, GkeClustersPerPolicy: 100,
			InternetHealthChecksPerManagedZone: 100,
		},
	})
}
