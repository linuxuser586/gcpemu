package compute

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	computev1 "google.golang.org/api/compute/v1"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/store"
)

const nsSubnets = "compute/subnetworks"

func subnetPath(project, region, name string) string {
	return "projects/" + project + "/regions/" + region + "/subnetworks/" + name
}

func (s *Service) insertSubnetwork(w http.ResponseWriter, r *http.Request) {
	p, region := r.PathValue("project"), r.PathValue("region")
	if err := s.env.EnsureProject(p); err != nil {
		apierr.Write(w, err)
		return
	}
	if err := checkRegion(p, region); err != nil {
		apierr.Write(w, err)
		return
	}
	var sn computev1.Subnetwork
	if _, err := decode(r, &sn); err != nil {
		apierr.Write(w, err)
		return
	}
	if err := validName("resource.name", sn.Name); err != nil {
		apierr.Write(w, err)
		return
	}
	path := subnetPath(p, region, sn.Name)
	if err := s.check(r.Context(), "compute.subnetworks.create", path); err != nil {
		apierr.Write(w, err)
		return
	}
	if sn.Network == "" {
		apierr.Write(w, errRequired("resource.network"))
		return
	}
	netPath, err := resolveNetworkRef(p, sn.Network)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	if sn.IpCidrRange == "" && sn.Purpose != "PRIVATE_SERVICE_CONNECT" {
		apierr.Write(w, errRequired("resource.ipCidrRange"))
		return
	}
	if sn.Purpose == "" {
		sn.Purpose = "PRIVATE"
	}
	if err := s.env.Store.View(func(tx store.Tx) error {
		if store.Exists(tx, nsSubnets, path) {
			return errExists(path)
		}
		if _, err := networkOf(tx, netPath); err != nil {
			return err
		}
		return validateSubnetRanges(tx, netPath, path, &sn)
	}); err != nil {
		apierr.Write(w, err)
		return
	}
	sn.Id = s.env.IDs.Uint64()
	op, err := s.startOp(r.Context(), opSpec{project: p, scope: "regions/" + region, opType: "insert", target: path, targetID: sn.Id},
		func(ctx context.Context) error {
			return s.env.Store.Update(func(tx store.Tx) error {
				if store.Exists(tx, nsSubnets, path) {
					return errExists(path)
				}
				if _, err := networkOf(tx, netPath); err != nil {
					return err
				}
				if err := validateSubnetRanges(tx, netPath, path, &sn); err != nil {
					return err
				}
				sn.Kind = "compute#subnetwork"
				sn.CreationTimestamp = stamp(s.env.Clock.Now())
				sn.Network = link(netPath)
				sn.Region = link("projects/" + p + "/regions/" + region)
				sn.SelfLink = link(path)
				c, _ := parseCIDR(sn.IpCidrRange, true)
				sn.GatewayAddress = ipString(c.gateway())
				if sn.StackType == "" {
					sn.StackType = "IPV4_ONLY"
				}
				if sn.PrivateIpv6GoogleAccess == "" {
					sn.PrivateIpv6GoogleAccess = "DISABLE_GOOGLE_ACCESS"
				}
				sn.State = "READY"
				sn.Fingerprint = fingerprint(&sn)
				return store.PutJSON(tx, nsSubnets, path, &sn)
			})
		})
	reply(w, op, err)
}

// validateSubnetRanges checks a subnetwork's primary and secondary ranges:
// valid, aligned, /8../29, and not overlapping other ranges of the network.
func validateSubnetRanges(tx store.Tx, netPath, self string, sn *computev1.Subnetwork) error {
	prim, err := parseCIDR(sn.IpCidrRange, true)
	if err != nil {
		return errInvalidField("resource.ipCidrRange", sn.IpCidrRange, "Must be a valid IPv4 CIDR network address range.")
	}
	if prim.bits < 8 || prim.bits > 29 {
		return errInvalidField("resource.ipCidrRange", sn.IpCidrRange, "The prefix length must be between 8 and 29.")
	}
	type rng struct {
		c     cidr
		owner string
	}
	var mine []rng
	mine = append(mine, rng{prim, "ipCidrRange"})
	seen := map[string]bool{}
	for i, sr := range sn.SecondaryIpRanges {
		if err := validName("resource.secondaryIpRanges.rangeName", sr.RangeName); err != nil {
			return err
		}
		if seen[sr.RangeName] {
			return errInvalidField("resource.secondaryIpRanges", sr.RangeName, "Secondary range names must be unique.")
		}
		seen[sr.RangeName] = true
		c, err := parseCIDR(sr.IpCidrRange, true)
		if err != nil || c.bits < 4 || c.bits > 29 {
			return errInvalidField("resource.secondaryIpRanges["+itoa(i)+"].ipCidrRange", sr.IpCidrRange, "Must be a valid IPv4 CIDR network address range.")
		}
		mine = append(mine, rng{c, sr.RangeName})
	}
	for i := range mine {
		for j := i + 1; j < len(mine); j++ {
			if mine[i].c.overlaps(mine[j].c) {
				return errInvalidField("resource.secondaryIpRanges", mine[j].c.String(), "Secondary range conflicts with "+mine[i].owner+" "+mine[i].c.String()+" of the same subnetwork.")
			}
		}
	}
	p, _, _ := pathParts(netPath)
	for _, o := range list[computev1.Subnetwork](tx, nsSubnets, "projects/"+p+"/regions/") {
		if relPath(o.Network) != netPath || relPath(o.SelfLink) == self {
			continue
		}
		ranges := []string{o.IpCidrRange}
		for _, sr := range o.SecondaryIpRanges {
			ranges = append(ranges, sr.IpCidrRange)
		}
		for _, rs := range ranges {
			oc, err := parseCIDR(rs, false)
			if err != nil {
				continue
			}
			for _, m := range mine {
				if m.c.overlaps(oc) {
					return apierr.InvalidArgument("Invalid IPCidrRange: %s conflicts with existing subnetwork '%s' in region '%s'.",
						m.c.String(), o.Name, lastSeg(o.Region)).WithLegacy("invalid")
				}
			}
		}
	}
	return nil
}

func itoa(i int) string { return strconv.Itoa(i) }

func (s *Service) loadSubnet(r *http.Request, perm string) (string, *computev1.Subnetwork, error) {
	p, region := r.PathValue("project"), r.PathValue("region")
	if err := s.env.EnsureProject(p); err != nil {
		return "", nil, err
	}
	if err := checkRegion(p, region); err != nil {
		return "", nil, err
	}
	path := subnetPath(p, region, r.PathValue("subnetwork"))
	if err := s.check(r.Context(), perm, path); err != nil {
		return "", nil, err
	}
	var sn *computev1.Subnetwork
	var ok bool
	_ = s.env.Store.View(func(tx store.Tx) error { sn, ok = get[computev1.Subnetwork](tx, nsSubnets, path); return nil })
	if !ok {
		return "", nil, errNotFound(path)
	}
	return path, sn, nil
}

func (s *Service) getSubnetwork(w http.ResponseWriter, r *http.Request) {
	_, sn, err := s.loadSubnet(r, "compute.subnetworks.get")
	reply(w, sn, err)
}

func (s *Service) listSubnetworks(w http.ResponseWriter, r *http.Request) {
	p, region := r.PathValue("project"), r.PathValue("region")
	if err := s.env.EnsureProject(p); err != nil {
		apierr.Write(w, err)
		return
	}
	if err := checkRegion(p, region); err != nil {
		apierr.Write(w, err)
		return
	}
	if err := s.check(r.Context(), "compute.subnetworks.list", "projects/"+p); err != nil {
		apierr.Write(w, err)
		return
	}
	coll := "projects/" + p + "/regions/" + region + "/subnetworks"
	var items []listItem
	_ = s.env.Store.View(func(tx store.Tx) error {
		for _, sn := range list[computev1.Subnetwork](tx, nsSubnets, coll+"/") {
			items = append(items, listItem{key: sn.Name, v: sn})
		}
		return nil
	})
	writeList(w, r, "compute#subnetworkList", coll, items)
}

func (s *Service) aggregatedSubnetworks(w http.ResponseWriter, r *http.Request) {
	s.aggregated(w, r, []string{nsSubnets}, "compute.subnetworks.list", "compute#subnetworkAggregatedList", "subnetworks",
		func(b []byte) (string, string, any) {
			var v computev1.Subnetwork
			_ = json.Unmarshal(b, &v)
			return v.SelfLink, v.Name, &v
		})
}

// aggregated serves an aggregatedList over a namespace keyed by path.
func (s *Service) aggregated(w http.ResponseWriter, r *http.Request, nss []string, perm, kind, field string, dec func([]byte) (selfLink, name string, v any)) {
	p := r.PathValue("project")
	if err := s.env.EnsureProject(p); err != nil {
		apierr.Write(w, err)
		return
	}
	if err := s.check(r.Context(), perm, "projects/"+p); err != nil {
		apierr.Write(w, err)
		return
	}
	scoped := map[string][]listItem{}
	_ = s.env.Store.View(func(tx store.Tx) error {
		for _, ns := range nss {
			tx.Scan(ns, "projects/"+p+"/", func(_ string, b []byte) bool {
				sl, name, v := dec(b)
				segs := strings.Split(relPath(sl), "/")
				sc := "global"
				if len(segs) > 3 && segs[2] != "global" {
					sc = segs[2] + "/" + segs[3]
				}
				scoped[sc] = append(scoped[sc], listItem{key: name, v: v})
				return true
			})
		}
		return nil
	})
	coll := "projects/" + p + "/aggregated/" + field
	writeAggregated(w, r, kind, coll, field, scoped)
}

// patchSubnetwork updates mutable fields; the fingerprint must match.
func (s *Service) patchSubnetwork(w http.ResponseWriter, r *http.Request) {
	path, cur, err := s.loadSubnet(r, "compute.subnetworks.update")
	if err != nil {
		apierr.Write(w, err)
		return
	}
	raw, err := readBody(r, false)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	var in computev1.Subnetwork
	if err := json.Unmarshal(raw, &in); err != nil {
		apierr.Write(w, apierr.InvalidArgument("Invalid JSON payload received. %v", err).WithLegacy("parseError"))
		return
	}
	if err := checkFingerprint(in.Fingerprint, cur.Fingerprint); err != nil {
		apierr.Write(w, err)
		return
	}
	var next computev1.Subnetwork
	if err := mergePatch(cur, raw, &next, "name", "id", "kind", "selfLink", "creationTimestamp", "network", "region",
		"ipCidrRange", "gatewayAddress", "fingerprint", "state", "purpose"); err != nil {
		apierr.Write(w, err)
		return
	}
	s.updateSubnet(w, r, path, cur, &next, "patch")
}

// updateSubnet validates next and stores it with a new fingerprint.
func (s *Service) updateSubnet(w http.ResponseWriter, r *http.Request, path string, cur, next *computev1.Subnetwork, opType string) {
	netPath := relPath(cur.Network)
	if err := s.env.Store.View(func(tx store.Tx) error { return validateSubnetRanges(tx, netPath, path, next) }); err != nil {
		apierr.Write(w, err)
		return
	}
	p, region, _ := pathParts(path)
	op, err := s.startOp(r.Context(), opSpec{project: p, scope: "regions/" + region, opType: opType, target: path, targetID: cur.Id},
		func(ctx context.Context) error {
			err := s.env.Store.Update(func(tx store.Tx) error {
				if !store.Exists(tx, nsSubnets, path) {
					return errNotFound(path)
				}
				next.Fingerprint = ""
				next.Fingerprint = fingerprint(next)
				return store.PutJSON(tx, nsSubnets, path, next)
			})
			if err == nil {
				s.networkChanged(netPath)
			}
			return err
		})
	reply(w, op, err)
}

// checkFingerprint implements compute's optimistic concurrency: a missing
// fingerprint is a 400, a stale one a 412 (conditionNotMet).
func checkFingerprint(got, want string) error {
	if got == "" {
		return errInvalidField("resource.fingerprint", "", "Supplied fingerprint does not match current metadata fingerprint.")
	}
	if got != want {
		return apierr.FailedPrecondition("Supplied fingerprint does not match current metadata fingerprint.").
			WithLegacy("conditionNotMet").WithHTTP(http.StatusPreconditionFailed)
	}
	return nil
}

func (s *Service) expandIpCidrRange(w http.ResponseWriter, r *http.Request) {
	path, cur, err := s.loadSubnet(r, "compute.subnetworks.expandIpCidrRange")
	if err != nil {
		apierr.Write(w, err)
		return
	}
	var req computev1.SubnetworksExpandIpCidrRangeRequest
	if _, err := decode(r, &req); err != nil {
		apierr.Write(w, err)
		return
	}
	nc, err := parseCIDR(req.IpCidrRange, true)
	if err != nil {
		apierr.Write(w, errInvalidField("ipCidrRange", req.IpCidrRange, "Must be a valid IPv4 CIDR network address range."))
		return
	}
	oc, _ := parseCIDR(cur.IpCidrRange, true)
	if !nc.covers(oc) || nc.bits >= oc.bits {
		apierr.Write(w, errInvalidField("ipCidrRange", req.IpCidrRange, "The new range must contain the existing range "+cur.IpCidrRange+" and be larger."))
		return
	}
	next := *cur
	next.IpCidrRange = nc.String()
	s.updateSubnet(w, r, path, cur, &next, "expandIpCidrRange")
}

func (s *Service) setPrivateIpGoogleAccess(w http.ResponseWriter, r *http.Request) {
	path, cur, err := s.loadSubnet(r, "compute.subnetworks.setPrivateIpGoogleAccess")
	if err != nil {
		apierr.Write(w, err)
		return
	}
	var req computev1.SubnetworksSetPrivateIpGoogleAccessRequest
	if _, err := decode(r, &req); err != nil {
		apierr.Write(w, err)
		return
	}
	next := *cur
	next.PrivateIpGoogleAccess = req.PrivateIpGoogleAccess
	s.updateSubnet(w, r, path, cur, &next, "setPrivateIpGoogleAccess")
}

func (s *Service) deleteSubnetwork(w http.ResponseWriter, r *http.Request) {
	path, cur, err := s.loadSubnet(r, "compute.subnetworks.delete")
	if err != nil {
		apierr.Write(w, err)
		return
	}
	if err := s.subnetInUse(r.Context(), path, cur); err != nil {
		apierr.Write(w, err)
		return
	}
	p, region, _ := pathParts(path)
	op, err := s.startOp(r.Context(), opSpec{project: p, scope: "regions/" + region, opType: "delete", target: path, targetID: cur.Id},
		func(ctx context.Context) error {
			if err := s.subnetInUse(ctx, path, cur); err != nil {
				return err
			}
			if err := s.env.Store.Update(func(tx store.Tx) error { return tx.Delete(nsSubnets, path) }); err != nil {
				return err
			}
			s.releaseNetworkRuntime(ctx, "", []string{path})
			s.networkChanged(relPath(cur.Network))
			return nil
		})
	reply(w, op, err)
}

func (s *Service) subnetInUse(ctx context.Context, path string, sn *computev1.Subnetwork) error {
	var user string
	_ = s.env.Store.View(func(tx store.Tx) error {
		if nw, ok := get[computev1.Network](tx, nsNetworks, relPath(sn.Network)); ok && nw.AutoCreateSubnetworks && nw.Name == sn.Name {
			user = relPath(nw.SelfLink)
			return nil
		}
		user = s.subnetUserTx(tx, path)
		return nil
	})
	if user == "" {
		user = s.externalUser(ctx, path)
	}
	if user != "" {
		return errInUse("subnetwork", path, user)
	}
	return nil
}

// subnetUserTx finds a resource using a subnetwork: an internal address,
// a NEG, a router NAT listing it, or a workload holding an IP on its
// container network.
func (s *Service) subnetUserTx(tx store.Tx, path string) string {
	p, _, _ := pathParts(path)
	for _, a := range list[computev1.Address](tx, nsAddresses, "projects/"+p+"/") {
		if a.Subnetwork != "" && relPath(a.Subnetwork) == path {
			return relPath(a.SelfLink)
		}
	}
	for _, g := range list[computev1.NetworkEndpointGroup](tx, nsNEGs, "projects/"+p+"/") {
		if g.Subnetwork != "" && relPath(g.Subnetwork) == path {
			return relPath(g.SelfLink)
		}
	}
	for _, rt := range list[computev1.Router](tx, nsRouters, "projects/"+p+"/regions/") {
		for _, nat := range rt.Nats {
			for _, sub := range nat.Subnetworks {
				if relPath(sub.Name) == path {
					return relPath(rt.SelfLink)
				}
			}
		}
	}
	if st, ok := get[vpcNet](tx, nsVPCNets, path); ok {
		if owner := firstOwner(tx, st.Name); owner != "" {
			return owner
		}
	}
	return ""
}
