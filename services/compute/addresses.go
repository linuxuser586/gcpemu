package compute

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"

	computev1 "google.golang.org/api/compute/v1"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// Regional and global addresses. External addresses get realistic public
// IPs from emulator pools; internal ones are allocated from their
// subnetwork; VPC_PEERING global addresses reserve a private range for
// private services access (servicenetworking).

const (
	nsAddresses       = "compute/addresses"
	nsGlobalAddresses = "compute/globalAddresses"
	nsMeta            = "compute/meta"
)

// External address pools: regional (and Cloud NAT auto-allocated) IPs come
// from 34.16.0.0/12, global ones from 34.120.0.0/14, as on GCP.
var (
	regionalPool, _ = parseCIDR("34.16.0.0/12", true)
	globalPool, _   = parseCIDR("34.120.0.0/14", true)
)

// nextExternalIP returns the next unused address of a pool.
func nextExternalIP(tx store.Tx, key string, pool cidr) (string, error) {
	var n uint32
	_ = store.GetJSON(tx, nsMeta, key, &n)
	for {
		n++
		ip := pool.base + n
		if !pool.contains(ip) {
			return "", apierr.New(8 /* ResourceExhausted */, "External IP address pool exhausted.").WithLegacy("quotaExceeded")
		}
		if o := ip & 0xff; o == 0 || o == 255 {
			continue
		}
		if err := store.PutJSON(tx, nsMeta, key, n); err != nil {
			return "", err
		}
		return ipString(ip), nil
	}
}

func addrNS(global bool) string {
	if global {
		return nsGlobalAddresses
	}
	return nsAddresses
}

func addrPerm(global bool, verb string) string {
	if global {
		return "compute.globalAddresses." + verb
	}
	return "compute.addresses." + verb
}

// addrScope returns (scope, path prefix) for the request.
func addrScope(r *http.Request) (global bool, scope, coll string, err error) {
	p := r.PathValue("project")
	if reg := r.PathValue("region"); reg != "" {
		if err := checkRegion(p, reg); err != nil {
			return false, "", "", err
		}
		return false, "regions/" + reg, "projects/" + p + "/regions/" + reg + "/addresses", nil
	}
	return true, "global", "projects/" + p + "/global/addresses", nil
}

func (s *Service) insertAddress(w http.ResponseWriter, r *http.Request) {
	p := r.PathValue("project")
	if err := s.env.EnsureProject(p); err != nil {
		apierr.Write(w, err)
		return
	}
	global, scope, coll, err := addrScope(r)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	var a computev1.Address
	if _, err := decode(r, &a); err != nil {
		apierr.Write(w, err)
		return
	}
	if err := validName("resource.name", a.Name); err != nil {
		apierr.Write(w, err)
		return
	}
	path := coll + "/" + a.Name
	verb := "create"
	if a.AddressType == "INTERNAL" && !global {
		verb = "createInternal"
	}
	if err := s.check(r.Context(), addrPerm(global, verb), path); err != nil {
		apierr.Write(w, err)
		return
	}
	// Validate synchronously (as GCP does) on a copy, then allocate in the
	// operation.
	dry := a
	if err := s.env.Store.View(func(tx store.Tx) error { return s.prepareAddress(tx, p, scope, path, global, &dry, true) }); err != nil {
		apierr.Write(w, err)
		return
	}
	a.Id = s.env.IDs.Uint64()
	op, err := s.startOp(r.Context(), opSpec{project: p, scope: scope, opType: "insert", target: path, targetID: a.Id},
		func(ctx context.Context) error { return s.createAddress(p, scope, path, global, &a) })
	reply(w, op, err)
}

// createAddress validates, allocates and stores an address.
func (s *Service) createAddress(project, scope, path string, global bool, a *computev1.Address) error {
	if a.AddressType == "" {
		a.AddressType = "EXTERNAL"
	}
	region := ""
	if !global {
		region = scope[len("regions/"):]
	}
	return s.env.Store.Update(func(tx store.Tx) error {
		if err := s.prepareAddress(tx, project, scope, path, global, a, false); err != nil {
			return err
		}
		a.Kind = "compute#address"
		a.CreationTimestamp = stamp(s.env.Clock.Now())
		a.SelfLink = link(path)
		if !global {
			a.Region = link("projects/" + project + "/regions/" + region)
		}
		a.Status = "RESERVED"
		a.LabelFingerprint = labelFingerprint(a.Labels)
		a.Users = nil
		return store.PutJSON(tx, addrNS(global), path, a)
	})
}

// prepareAddress validates a and allocates its IP (dry runs skip external
// pool allocation, which writes).
func (s *Service) prepareAddress(tx store.Tx, project, scope, path string, global bool, a *computev1.Address, dry bool) error {
	if a.AddressType == "" {
		a.AddressType = "EXTERNAL"
	}
	region := ""
	if !global {
		region = scope[len("regions/"):]
	}
	if store.Exists(tx, addrNS(global), path) {
		return errExists(path)
	}
	{
		switch a.AddressType {
		case "EXTERNAL":
			if a.Purpose != "" && a.Purpose != "NAT_AUTO" && a.Purpose != "IPSEC_INTERCONNECT" {
				// Purposes on external addresses are not supported.
				a.Purpose = ""
			}
			if a.NetworkTier == "" {
				a.NetworkTier = "PREMIUM"
			}
			if global && a.IpVersion == "" {
				a.IpVersion = "IPV4"
			}
			if a.Address == "" {
				if !dry {
					pool, key := regionalPool, "extip/regional"
					if global {
						pool, key = globalPool, "extip/global"
					}
					ip, err := nextExternalIP(tx, key, pool)
					if err != nil {
						return err
					}
					a.Address = ip
				}
			} else if _, ok := parseIP(a.Address); !ok {
				return errInvalidField("resource.address", a.Address, "Must be a valid IPv4 address.")
			}
		case "INTERNAL":
			if global {
				if err := s.allocGlobalInternal(tx, project, a); err != nil {
					return err
				}
			} else if err := s.allocRegionalInternal(tx, project, region, a); err != nil {
				return err
			}
		default:
			return errInvalidField("resource.addressType", a.AddressType, "")
		}
	}
	return nil
}

// allocRegionalInternal reserves an address in the subnetwork.
func (s *Service) allocRegionalInternal(tx store.Tx, project, region string, a *computev1.Address) error {
	if a.Purpose == "" {
		a.Purpose = "GCE_ENDPOINT"
	}
	ref := a.Subnetwork
	if ref == "" {
		ref = "default"
	}
	sp, err := regionalRef(project, region, "subnetworks", ref)
	if err != nil {
		return errInvalidField("resource.subnetwork", a.Subnetwork, "The URL is malformed.")
	}
	sn, ok := get[computev1.Subnetwork](tx, nsSubnets, sp)
	if !ok {
		return errInvalidField("resource.subnetwork", ref, "The referenced subnetwork resource cannot be found.")
	}
	a.Subnetwork = sn.SelfLink
	a.Network = ""
	c, _ := parseCIDR(sn.IpCidrRange, true)
	used := map[uint32]bool{}
	for _, o := range list[computev1.Address](tx, nsAddresses, "projects/"+project+"/regions/"+region+"/addresses/") {
		if relPath(o.Subnetwork) == sp {
			if ip, ok := parseIP(o.Address); ok {
				used[ip] = true
			}
		}
	}
	if st, ok := get[vpcNet](tx, nsVPCNets, sp); ok && st.Subnet == sn.IpCidrRange {
		for ip := range allocatedIPs(tx, st.Name) {
			used[ip] = true
		}
	}
	if a.Address != "" {
		ip, ok := parseIP(a.Address)
		if !ok || !c.usable(ip) {
			return errInvalidField("resource.address", a.Address, "The specified IP address is not within the subnetwork range or is reserved.")
		}
		if used[ip] {
			return apierr.InvalidArgument("IP address '%s' is already being used by another resource.", a.Address).WithLegacy("ipInUseByAnotherResource")
		}
		return nil
	}
	for ip := c.base + 2; ip < c.last(); ip++ {
		if c.usable(ip) && !used[ip] {
			a.Address = ipString(ip)
			return nil
		}
	}
	return apierr.New(8, "IP space of '%s' is exhausted.", sn.SelfLink).WithLegacy("ipSpaceExhausted")
}

// allocGlobalInternal handles VPC_PEERING (private services access) and
// PRIVATE_SERVICE_CONNECT global internal addresses.
func (s *Service) allocGlobalInternal(tx store.Tx, project string, a *computev1.Address) error {
	if a.Purpose != "VPC_PEERING" && a.Purpose != "PRIVATE_SERVICE_CONNECT" {
		return errInvalidField("resource.purpose", a.Purpose, "Global internal addresses must have purpose VPC_PEERING or PRIVATE_SERVICE_CONNECT.")
	}
	if a.Network == "" {
		return errRequired("resource.network")
	}
	np, err := resolveNetworkRef(project, a.Network)
	if err != nil {
		return err
	}
	if _, ok := get[computev1.Network](tx, nsNetworks, np); !ok {
		return errInvalidField("resource.network", a.Network, "The referenced network resource cannot be found.")
	}
	a.Network = link(np)
	if a.Purpose == "PRIVATE_SERVICE_CONNECT" {
		if _, ok := parseIP(a.Address); !ok {
			return errInvalidField("resource.address", a.Address, "Must be a valid IPv4 address.")
		}
		return nil
	}
	if a.PrefixLength == 0 {
		return errRequired("resource.prefixLength")
	}
	if a.PrefixLength < 8 || a.PrefixLength > 29 {
		return errInvalidField("resource.prefixLength", a.PrefixLength, "Must be between 8 and 29.")
	}
	taken := networkRanges(tx, np)
	if a.Address != "" {
		c, err := parseCIDR(a.Address+"/"+strconv.Itoa(int(a.PrefixLength)), true)
		if err != nil {
			return errInvalidField("resource.address", a.Address, "The address must be the first address of a range of the given prefix length.")
		}
		for _, t := range taken {
			if t.overlaps(c) {
				return apierr.InvalidArgument("Requested range %s overlaps with an existing range %s of network '%s'.", c, t, np).WithLegacy("invalid")
			}
		}
		return nil
	}
	size := uint32(1) << (32 - a.PrefixLength)
	ten, _ := parseCIDR("10.0.0.0/8", true)
	// GCP picks from unused RFC 1918 space; the emulator scans 10/8 upwards
	// starting above the auto-mode ranges' typical neighbours.
	for base := ten.base; base+size-1 <= ten.last() && base >= ten.base; base += size {
		c := cidr{base: base, bits: int(a.PrefixLength)}
		free := true
		for _, t := range taken {
			if t.overlaps(c) {
				free = false
				break
			}
		}
		if free {
			a.Address = ipString(base)
			return nil
		}
	}
	return apierr.New(8, "No free range of size /%d in the network.", a.PrefixLength).WithLegacy("ipSpaceExhausted")
}

// networkRanges lists every range in use in a VPC: subnet primary and
// secondary ranges and VPC_PEERING allocations.
func networkRanges(tx store.Tx, np string) []cidr {
	p, _, _ := pathParts(np)
	var out []cidr
	add := func(s string) {
		if c, err := parseCIDR(s, false); err == nil {
			out = append(out, c)
		}
	}
	for _, sn := range list[computev1.Subnetwork](tx, nsSubnets, "projects/"+p+"/regions/") {
		if relPath(sn.Network) != np {
			continue
		}
		add(sn.IpCidrRange)
		for _, sr := range sn.SecondaryIpRanges {
			add(sr.IpCidrRange)
		}
	}
	for _, a := range list[computev1.Address](tx, nsGlobalAddresses, "projects/"+p+"/global/addresses/") {
		if a.Purpose == "VPC_PEERING" && relPath(a.Network) == np {
			add(a.Address + "/" + strconv.Itoa(int(a.PrefixLength)))
		}
	}
	return out
}

func (s *Service) loadAddress(r *http.Request, verb string) (bool, string, *computev1.Address, error) {
	p := r.PathValue("project")
	if err := s.env.EnsureProject(p); err != nil {
		return false, "", nil, err
	}
	global, _, coll, err := addrScope(r)
	if err != nil {
		return false, "", nil, err
	}
	path := coll + "/" + r.PathValue("address")
	if err := s.check(r.Context(), addrPerm(global, verb), path); err != nil {
		return false, "", nil, err
	}
	var a *computev1.Address
	var ok bool
	_ = s.env.Store.View(func(tx store.Tx) error { a, ok = get[computev1.Address](tx, addrNS(global), path); return nil })
	if !ok {
		return false, "", nil, errNotFound(path)
	}
	return global, path, a, nil
}

func (s *Service) getAddress(w http.ResponseWriter, r *http.Request) {
	_, _, a, err := s.loadAddress(r, "get")
	reply(w, a, err)
}

func (s *Service) listAddresses(w http.ResponseWriter, r *http.Request) {
	p := r.PathValue("project")
	if err := s.env.EnsureProject(p); err != nil {
		apierr.Write(w, err)
		return
	}
	global, _, coll, err := addrScope(r)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	if err := s.check(r.Context(), addrPerm(global, "list"), "projects/"+p); err != nil {
		apierr.Write(w, err)
		return
	}
	var items []listItem
	_ = s.env.Store.View(func(tx store.Tx) error {
		for _, a := range list[computev1.Address](tx, addrNS(global), coll+"/") {
			items = append(items, listItem{key: a.Name, v: a})
		}
		return nil
	})
	writeList(w, r, "compute#addressList", coll, items)
}

func (s *Service) aggregatedAddresses(w http.ResponseWriter, r *http.Request) {
	s.aggregated(w, r, []string{nsAddresses, nsGlobalAddresses}, "compute.addresses.list", "compute#addressAggregatedList", "addresses",
		func(b []byte) (string, string, any) {
			var v computev1.Address
			_ = json.Unmarshal(b, &v)
			return v.SelfLink, v.Name, &v
		})
}

func (s *Service) deleteAddress(w http.ResponseWriter, r *http.Request) {
	global, path, a, err := s.loadAddress(r, "delete")
	if err != nil {
		apierr.Write(w, err)
		return
	}
	if err := s.addressInUse(r.Context(), path, a); err != nil {
		apierr.Write(w, err)
		return
	}
	scope := "global"
	if !global {
		_, region, _ := pathParts(path)
		scope = "regions/" + region
	}
	p, _, _ := pathParts(path)
	op, err := s.startOp(r.Context(), opSpec{project: p, scope: scope, opType: "delete", target: path, targetID: a.Id},
		func(ctx context.Context) error {
			if err := s.addressInUse(ctx, path, a); err != nil {
				return err
			}
			return s.env.Store.Update(func(tx store.Tx) error { return tx.Delete(addrNS(global), path) })
		})
	reply(w, op, err)
}

// addressInUse refuses deleting an address used by a NAT, a load balancer
// or a private services access connection (FR-CORE-026).
func (s *Service) addressInUse(ctx context.Context, path string, a *computev1.Address) error {
	user := ""
	if len(a.Users) > 0 {
		user = relPath(a.Users[0])
	}
	if user == "" {
		_ = s.env.Store.View(func(tx store.Tx) error {
			if a.Purpose == "VPC_PEERING" {
				if c, ok := get[psaConnection](tx, nsPSA, relPath(a.Network)); ok && contains(c.ReservedPeeringRanges, a.Name) {
					user = relPath(a.Network) + "/peerings/" + psaPeeringName
				}
			}
			return nil
		})
	}
	if user == "" {
		user = s.externalUser(ctx, path)
	}
	if user != "" {
		return errInUse("address", path, user)
	}
	return nil
}

func (s *Service) setAddressLabels(w http.ResponseWriter, r *http.Request) {
	global, path, a, err := s.loadAddress(r, "setLabels")
	if err != nil {
		apierr.Write(w, err)
		return
	}
	var req computev1.RegionSetLabelsRequest
	if _, err := decode(r, &req); err != nil {
		apierr.Write(w, err)
		return
	}
	if req.LabelFingerprint != a.LabelFingerprint {
		apierr.Write(w, apierr.FailedPrecondition("Labels fingerprint either invalid or resource labels have changed").
			WithLegacy("conditionNotMet").WithHTTP(http.StatusPreconditionFailed))
		return
	}
	scope := "global"
	if !global {
		_, region, _ := pathParts(path)
		scope = "regions/" + region
	}
	p, _, _ := pathParts(path)
	op, err := s.startOp(r.Context(), opSpec{project: p, scope: scope, opType: "setLabels", target: path, targetID: a.Id},
		func(ctx context.Context) error {
			return s.env.Store.Update(func(tx store.Tx) error {
				cur, ok := get[computev1.Address](tx, addrNS(global), path)
				if !ok {
					return errNotFound(path)
				}
				cur.Labels = req.Labels
				cur.LabelFingerprint = labelFingerprint(req.Labels)
				return store.PutJSON(tx, addrNS(global), path, cur)
			})
		})
	reply(w, op, err)
}

// setAddressUsers records the resources using an address (status IN_USE).
func setAddressUsers(tx store.Tx, path string, users []string) error {
	ns := nsAddresses
	if _, loc, _ := pathParts(path); loc == "global" {
		ns = nsGlobalAddresses
	}
	a, ok := get[computev1.Address](tx, ns, path)
	if !ok {
		return nil
	}
	a.Users = users
	a.Status = "RESERVED"
	if len(users) > 0 {
		a.Status = "IN_USE"
	}
	return store.PutJSON(tx, ns, path, a)
}
