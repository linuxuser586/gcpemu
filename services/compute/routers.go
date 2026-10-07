package compute

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"

	computev1 "google.golang.org/api/compute/v1"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// Cloud Routers and their Cloud NAT configurations (FR-NAT-001). Every
// change re-programs the VPC's egress gateway (FR-NAT-002).

const (
	nsRouters = "compute/routers"
	// nsNatIPs holds auto-allocated NAT IPs keyed "routerPath/natName".
	nsNatIPs = "compute/natips"
)

func routerPath(project, region, name string) string {
	return "projects/" + project + "/regions/" + region + "/routers/" + name
}

func (s *Service) insertRouter(w http.ResponseWriter, r *http.Request) {
	p, region := r.PathValue("project"), r.PathValue("region")
	if err := s.env.EnsureProject(p); err != nil {
		apierr.Write(w, err)
		return
	}
	if err := checkRegion(p, region); err != nil {
		apierr.Write(w, err)
		return
	}
	var rt computev1.Router
	if _, err := decode(r, &rt); err != nil {
		apierr.Write(w, err)
		return
	}
	if err := validName("resource.name", rt.Name); err != nil {
		apierr.Write(w, err)
		return
	}
	path := routerPath(p, region, rt.Name)
	if err := s.check(r.Context(), "compute.routers.create", path); err != nil {
		apierr.Write(w, err)
		return
	}
	if rt.Network == "" {
		apierr.Write(w, errRequired("resource.network"))
		return
	}
	if err := s.env.Store.View(func(tx store.Tx) error {
		if store.Exists(tx, nsRouters, path) {
			return errExists(path)
		}
		return s.validateRouter(tx, path, &rt)
	}); err != nil {
		apierr.Write(w, err)
		return
	}
	rt.Id = s.env.IDs.Uint64()
	op, err := s.startOp(r.Context(), opSpec{project: p, scope: "regions/" + region, opType: "insert", target: path, targetID: rt.Id},
		func(ctx context.Context) error {
			rt.Kind = "compute#router"
			rt.CreationTimestamp = stamp(s.env.Clock.Now())
			rt.SelfLink = link(path)
			rt.Region = link("projects/" + p + "/regions/" + region)
			err := s.env.Store.Update(func(tx store.Tx) error {
				if store.Exists(tx, nsRouters, path) {
					return errExists(path)
				}
				if err := s.validateRouter(tx, path, &rt); err != nil {
					return err
				}
				if err := store.PutJSON(tx, nsRouters, path, &rt); err != nil {
					return err
				}
				return s.syncNatResources(tx, path, nil, &rt)
			})
			if err == nil {
				s.networkChanged(relPath(rt.Network))
			}
			return err
		})
	reply(w, op, err)
}

// validateRouter canonicalises references and validates the NAT configs.
func (s *Service) validateRouter(tx store.Tx, path string, rt *computev1.Router) error {
	p, region, _ := pathParts(path)
	np, err := resolveNetworkRef(p, rt.Network)
	if err != nil {
		return err
	}
	if _, ok := get[computev1.Network](tx, nsNetworks, np); !ok {
		return errInvalidField("resource.network", rt.Network, "The referenced network resource cannot be found.")
	}
	rt.Network = link(np)
	if rt.Bgp != nil && rt.Bgp.Asn != 0 {
		if a := rt.Bgp.Asn; !(a >= 64512 && a <= 65534) && !(a >= 4200000000 && a <= 4294967294) && a != 16550 {
			return errInvalidField("resource.bgp.asn", a, "Must be a private ASN (64512-65534, 4200000000-4294967294) or 16550.")
		}
		if rt.Bgp.AdvertiseMode == "" {
			rt.Bgp.AdvertiseMode = "DEFAULT"
		}
		if rt.Bgp.KeepaliveInterval == 0 {
			rt.Bgp.KeepaliveInterval = 20
		}
	}
	names := map[string]bool{}
	for i, nat := range rt.Nats {
		f := fmt.Sprintf("resource.nats[%d]", i)
		if err := validName(f+".name", nat.Name); err != nil {
			return err
		}
		if names[nat.Name] {
			return errInvalidField(f+".name", nat.Name, "NAT names must be unique within a router.")
		}
		names[nat.Name] = true
		if nat.Type == "" {
			nat.Type = "PUBLIC"
		}
		if len(nat.EndpointTypes) == 0 {
			nat.EndpointTypes = []string{"ENDPOINT_TYPE_VM"}
		}
		switch nat.NatIpAllocateOption {
		case "AUTO_ONLY":
			if len(nat.NatIps) > 0 {
				return errInvalidField(f+".natIps", strings.Join(nat.NatIps, ","), "NAT IPs must not be specified with AUTO_ONLY allocation.")
			}
		case "MANUAL_ONLY":
			if len(nat.NatIps) == 0 {
				return errInvalidField(f+".natIps", "", "At least one NAT IP is required with MANUAL_ONLY allocation.")
			}
			for j, ref := range append(append([]string{}, nat.NatIps...), nat.DrainNatIps...) {
				ap, err := regionalRef(p, region, "addresses", ref)
				if err != nil {
					return errInvalidField(fmt.Sprintf("%s.natIps[%d]", f, j), ref, "The URL is malformed.")
				}
				a, ok := get[computev1.Address](tx, nsAddresses, ap)
				if !ok {
					return errInvalidField(fmt.Sprintf("%s.natIps[%d]", f, j), ref, "The referenced address resource cannot be found.")
				}
				if a.AddressType != "EXTERNAL" {
					return errInvalidField(fmt.Sprintf("%s.natIps[%d]", f, j), ref, "NAT IPs must be external addresses.")
				}
				for _, u := range a.Users {
					if relPath(u) != path {
						return errInUse("address", ap, relPath(u))
					}
				}
				if j < len(nat.NatIps) {
					nat.NatIps[j] = link(ap)
				} else {
					nat.DrainNatIps[j-len(nat.NatIps)] = link(ap)
				}
			}
		case "":
			return errRequired(f + ".natIpAllocateOption")
		default:
			return errInvalidField(f+".natIpAllocateOption", nat.NatIpAllocateOption, "")
		}
		switch nat.SourceSubnetworkIpRangesToNat {
		case "ALL_SUBNETWORKS_ALL_IP_RANGES", "ALL_SUBNETWORKS_ALL_PRIMARY_IP_RANGES":
			if len(nat.Subnetworks) > 0 {
				return errInvalidField(f+".subnetworks", "", "Subnetworks must not be listed unless sourceSubnetworkIpRangesToNat is LIST_OF_SUBNETWORKS.")
			}
		case "LIST_OF_SUBNETWORKS":
			if len(nat.Subnetworks) == 0 {
				return errInvalidField(f+".subnetworks", "", "At least one subnetwork is required with LIST_OF_SUBNETWORKS.")
			}
			for j, sub := range nat.Subnetworks {
				sf := fmt.Sprintf("%s.subnetworks[%d]", f, j)
				sp, err := regionalRef(p, region, "subnetworks", sub.Name)
				if err != nil {
					return errInvalidField(sf+".name", sub.Name, "The URL is malformed.")
				}
				sn, ok := get[computev1.Subnetwork](tx, nsSubnets, sp)
				if !ok {
					return errInvalidField(sf+".name", sub.Name, "The referenced subnetwork resource cannot be found.")
				}
				if _, sreg, _ := pathParts(sp); sreg != region || relPath(sn.Network) != np {
					return errInvalidField(sf+".name", sub.Name, "The subnetwork must be in the router's region and network.")
				}
				sub.Name = sn.SelfLink
				if len(sub.SourceIpRangesToNat) == 0 {
					return errRequired(sf + ".sourceIpRangesToNat")
				}
				for _, k := range sub.SourceIpRangesToNat {
					switch k {
					case "ALL_IP_RANGES", "PRIMARY_IP_RANGE", "LIST_OF_SECONDARY_IP_RANGES":
					default:
						return errInvalidField(sf+".sourceIpRangesToNat", k, "")
					}
				}
				if contains(sub.SourceIpRangesToNat, "LIST_OF_SECONDARY_IP_RANGES") {
					if len(sub.SecondaryIpRangeNames) == 0 {
						return errRequired(sf + ".secondaryIpRangeNames")
					}
					for _, n := range sub.SecondaryIpRangeNames {
						found := false
						for _, sr := range sn.SecondaryIpRanges {
							found = found || sr.RangeName == n
						}
						if !found {
							return errInvalidField(sf+".secondaryIpRangeNames", n, "The secondary range does not exist in the subnetwork.")
						}
					}
				}
			}
		case "":
			return errRequired(f + ".sourceSubnetworkIpRangesToNat")
		default:
			return errInvalidField(f+".sourceSubnetworkIpRangesToNat", nat.SourceSubnetworkIpRangesToNat, "")
		}
		if nat.LogConfig != nil {
			switch nat.LogConfig.Filter {
			case "", "ALL", "ERRORS_ONLY", "TRANSLATIONS_ONLY":
				if nat.LogConfig.Filter == "" {
					nat.LogConfig.Filter = "ALL"
				}
			default:
				return errInvalidField(f+".logConfig.filter", nat.LogConfig.Filter, "")
			}
		}
		if m := nat.MinPortsPerVm; m != 0 {
			if m < 2 || m > 65536 {
				return errInvalidField(f+".minPortsPerVm", m, "Must be between 2 and 65536.")
			}
			if nat.EnableDynamicPortAllocation && m&(m-1) != 0 {
				return errInvalidField(f+".minPortsPerVm", m, "Must be a power of two when dynamic port allocation is enabled.")
			}
		}
		if nat.MaxPortsPerVm != 0 && !nat.EnableDynamicPortAllocation {
			return errInvalidField(f+".maxPortsPerVm", nat.MaxPortsPerVm, "maxPortsPerVm can only be set with dynamic port allocation.")
		}
	}
	return s.natConflicts(tx, path, np, region, rt)
}

// natConflicts enforces that a subnetwork range is served by at most one
// NAT gateway per network and region, as GCP requires.
func (s *Service) natConflicts(tx store.Tx, self, np, region string, rt *computev1.Router) error {
	p, _, _ := pathParts(self)
	type owner struct{ router, nat string }
	var all *owner             // the ALL_SUBNETWORKS_* NAT, if any
	subs := map[string]owner{} // LIST_OF_SUBNETWORKS claims by subnet path
	claim := func(rp string, nat *computev1.RouterNat) error {
		me := owner{rp, nat.Name}
		if nat.SourceSubnetworkIpRangesToNat != "LIST_OF_SUBNETWORKS" {
			if all != nil {
				return errNATConflict(me.nat, all.nat, all.router)
			}
			for _, o := range subs {
				return errNATConflict(me.nat, o.nat, o.router)
			}
			all = &me
			return nil
		}
		if all != nil {
			return errNATConflict(me.nat, all.nat, all.router)
		}
		for _, sub := range nat.Subnetworks {
			k := relPath(sub.Name)
			if o, ok := subs[k]; ok {
				return errNATConflict(me.nat, o.nat, o.router)
			}
			subs[k] = me
		}
		return nil
	}
	for _, o := range list[computev1.Router](tx, nsRouters, "projects/"+p+"/regions/"+region+"/routers/") {
		if relPath(o.SelfLink) == self || relPath(o.Network) != np {
			continue
		}
		for _, nat := range o.Nats {
			_ = claim(relPath(o.SelfLink), nat)
		}
	}
	for _, nat := range rt.Nats {
		if err := claim(self, nat); err != nil {
			return err
		}
	}
	return nil
}

func errNATConflict(nat, other, router string) error {
	return apierr.InvalidArgument("Invalid value for field 'resource.nats': NAT '%s' conflicts with NAT '%s' of router '%s': a subnetwork range can be served by only one NAT gateway in a network and region.",
		nat, other, router).WithLegacy("invalid")
}

// syncNatResources updates address users and auto-allocated NAT IPs after
// a router changes from prev to next (either may be nil).
func (s *Service) syncNatResources(tx store.Tx, path string, prev, next *computev1.Router) error {
	used := map[string]bool{}
	if next != nil {
		for _, nat := range next.Nats {
			for _, ip := range append(append([]string{}, nat.NatIps...), nat.DrainNatIps...) {
				used[relPath(ip)] = true
			}
		}
	}
	if prev != nil {
		for _, nat := range prev.Nats {
			for _, ip := range append(append([]string{}, nat.NatIps...), nat.DrainNatIps...) {
				if !used[relPath(ip)] {
					if err := setAddressUsers(tx, relPath(ip), nil); err != nil {
						return err
					}
				}
			}
		}
	}
	for ap := range used {
		if err := setAddressUsers(tx, ap, []string{link(path)}); err != nil {
			return err
		}
	}
	// Auto-allocated IPs: keep per NAT while it stays AUTO_ONLY.
	keep := map[string]bool{}
	if next != nil {
		for _, nat := range next.Nats {
			if nat.NatIpAllocateOption != "AUTO_ONLY" {
				continue
			}
			k := path + "/" + nat.Name
			keep[k] = true
			if store.Exists(tx, nsNatIPs, k) {
				continue
			}
			ip, err := nextExternalIP(tx, "extip/regional", regionalPool)
			if err != nil {
				return err
			}
			if err := store.PutJSON(tx, nsNatIPs, k, []string{ip}); err != nil {
				return err
			}
		}
	}
	var stale []string
	tx.Scan(nsNatIPs, path+"/", func(k string, _ []byte) bool {
		if !keep[k] {
			stale = append(stale, k)
		}
		return true
	})
	for _, k := range stale {
		if err := tx.Delete(nsNatIPs, k); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) loadRouter(r *http.Request, perm string) (string, *computev1.Router, error) {
	p, region := r.PathValue("project"), r.PathValue("region")
	if err := s.env.EnsureProject(p); err != nil {
		return "", nil, err
	}
	if err := checkRegion(p, region); err != nil {
		return "", nil, err
	}
	path := routerPath(p, region, r.PathValue("router"))
	if err := s.check(r.Context(), perm, path); err != nil {
		return "", nil, err
	}
	var rt *computev1.Router
	var ok bool
	_ = s.env.Store.View(func(tx store.Tx) error { rt, ok = get[computev1.Router](tx, nsRouters, path); return nil })
	if !ok {
		return "", nil, errNotFound(path)
	}
	return path, rt, nil
}

func (s *Service) getRouter(w http.ResponseWriter, r *http.Request) {
	_, rt, err := s.loadRouter(r, "compute.routers.get")
	reply(w, rt, err)
}

func (s *Service) listRouters(w http.ResponseWriter, r *http.Request) {
	p, region := r.PathValue("project"), r.PathValue("region")
	if err := s.env.EnsureProject(p); err != nil {
		apierr.Write(w, err)
		return
	}
	if err := checkRegion(p, region); err != nil {
		apierr.Write(w, err)
		return
	}
	if err := s.check(r.Context(), "compute.routers.list", "projects/"+p); err != nil {
		apierr.Write(w, err)
		return
	}
	coll := "projects/" + p + "/regions/" + region + "/routers"
	var items []listItem
	_ = s.env.Store.View(func(tx store.Tx) error {
		for _, rt := range list[computev1.Router](tx, nsRouters, coll+"/") {
			items = append(items, listItem{key: rt.Name, v: rt})
		}
		return nil
	})
	writeList(w, r, "compute#routerList", coll, items)
}

func (s *Service) aggregatedRouters(w http.ResponseWriter, r *http.Request) {
	s.aggregated(w, r, []string{nsRouters}, "compute.routers.list", "compute#routerAggregatedList", "routers",
		func(b []byte) (string, string, any) {
			var v computev1.Router
			_ = json.Unmarshal(b, &v)
			return v.SelfLink, v.Name, &v
		})
}

// updateRouter implements patch (merge; nats replace as a whole) and
// update (replace).
func (s *Service) updateRouter(w http.ResponseWriter, r *http.Request) {
	path, cur, err := s.loadRouter(r, "compute.routers.update")
	if err != nil {
		apierr.Write(w, err)
		return
	}
	raw, err := readBody(r, false)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	var next computev1.Router
	keep := []string{"name", "id", "kind", "selfLink", "creationTimestamp", "region"}
	base := any(cur)
	if r.Method == http.MethodPut {
		base = map[string]any{}
	}
	if err := mergePatch(base, raw, &next, keep...); err != nil {
		apierr.Write(w, err)
		return
	}
	if r.Method == http.MethodPut {
		next.Name, next.Id, next.Kind, next.SelfLink, next.CreationTimestamp, next.Region = cur.Name, cur.Id, cur.Kind, cur.SelfLink, cur.CreationTimestamp, cur.Region
	}
	if next.Network == "" {
		next.Network = cur.Network
	}
	p, region, _ := pathParts(path)
	if np, _ := resolveNetworkRef(p, next.Network); np != relPath(cur.Network) {
		apierr.Write(w, errInvalidField("resource.network", next.Network, "The network of a router cannot be changed."))
		return
	}
	if err := s.env.Store.View(func(tx store.Tx) error { return s.validateRouter(tx, path, &next) }); err != nil {
		apierr.Write(w, err)
		return
	}
	opType := "patch"
	if r.Method == http.MethodPut {
		opType = "update"
	}
	op, err := s.startOp(r.Context(), opSpec{project: p, scope: "regions/" + region, opType: opType, target: path, targetID: cur.Id},
		func(ctx context.Context) error {
			err := s.env.Store.Update(func(tx store.Tx) error {
				prev, ok := get[computev1.Router](tx, nsRouters, path)
				if !ok {
					return errNotFound(path)
				}
				if err := s.validateRouter(tx, path, &next); err != nil {
					return err
				}
				if err := store.PutJSON(tx, nsRouters, path, &next); err != nil {
					return err
				}
				return s.syncNatResources(tx, path, prev, &next)
			})
			if err == nil {
				s.networkChanged(relPath(next.Network))
			}
			return err
		})
	reply(w, op, err)
}

func (s *Service) deleteRouter(w http.ResponseWriter, r *http.Request) {
	path, cur, err := s.loadRouter(r, "compute.routers.delete")
	if err != nil {
		apierr.Write(w, err)
		return
	}
	if u := s.externalUser(r.Context(), path); u != "" {
		apierr.Write(w, errInUse("router", path, u))
		return
	}
	p, region, _ := pathParts(path)
	op, err := s.startOp(r.Context(), opSpec{project: p, scope: "regions/" + region, opType: "delete", target: path, targetID: cur.Id},
		func(ctx context.Context) error {
			err := s.env.Store.Update(func(tx store.Tx) error {
				prev, ok := get[computev1.Router](tx, nsRouters, path)
				if !ok {
					return errNotFound(path)
				}
				if err := s.syncNatResources(tx, path, prev, nil); err != nil {
					return err
				}
				return tx.Delete(nsRouters, path)
			})
			if err == nil {
				s.networkChanged(relPath(cur.Network))
			}
			return err
		})
	reply(w, op, err)
}

// natVMs lists the workloads (IP owners) a NAT serves, by source IP.
func (s *Service) natVMs(tx store.Tx, rt *computev1.Router, nat *computev1.RouterNat) []natVM {
	p, region, _ := pathParts(relPath(rt.SelfLink))
	var out []natVM
	for _, sn := range list[computev1.Subnetwork](tx, nsSubnets, "projects/"+p+"/regions/"+region+"/subnetworks/") {
		if relPath(sn.Network) != relPath(rt.Network) {
			continue
		}
		if len(natRanges(nat, sn, sn.IpCidrRange)) == 0 {
			continue
		}
		st, ok := get[vpcNet](tx, nsVPCNets, relPath(sn.SelfLink))
		if !ok {
			continue
		}
		for _, a := range ownersOf(tx, st.Name) {
			out = append(out, natVM{owner: a.owner, ip: a.ip})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ip < out[j].ip })
	return out
}

type natVM struct{ owner, ip string }

// natIPs returns a NAT's active IP addresses (auto-allocated or reserved).
func natIPs(tx store.Tx, routerP string, nat *computev1.RouterNat) []string {
	if nat.NatIpAllocateOption == "AUTO_ONLY" {
		var ips []string
		_ = store.GetJSON(tx, nsNatIPs, routerP+"/"+nat.Name, &ips)
		return ips
	}
	var ips []string
	for _, ref := range nat.NatIps {
		if a, ok := get[computev1.Address](tx, nsAddresses, relPath(ref)); ok {
			ips = append(ips, a.Address)
		}
	}
	return ips
}

// getRouterStatus reports NAT IPs and the number of VM endpoints with NAT
// mappings (FR-NAT-003).
func (s *Service) getRouterStatus(w http.ResponseWriter, r *http.Request) {
	path, rt, err := s.loadRouter(r, "compute.routers.get")
	if err != nil {
		apierr.Write(w, err)
		return
	}
	res := &computev1.RouterStatus{Network: rt.Network, BestRoutes: []*computev1.Route{}, BestRoutesForRouter: []*computev1.Route{}}
	_ = s.env.Store.View(func(tx store.Tx) error {
		for _, nat := range rt.Nats {
			st := &computev1.RouterStatusNatStatus{Name: nat.Name}
			ips := natIPs(tx, path, nat)
			if nat.NatIpAllocateOption == "AUTO_ONLY" {
				st.AutoAllocatedNatIps = ips
			} else {
				st.UserAllocatedNatIps = ips
				for _, ref := range nat.NatIps {
					st.UserAllocatedNatIpResources = append(st.UserAllocatedNatIpResources, ref)
				}
			}
			st.NumVmEndpointsWithNatMappings = int64(len(s.natVMs(tx, rt, nat)))
			st.ForceSendFields = []string{"NumVmEndpointsWithNatMappings", "MinExtraNatIpsNeeded"}
			res.NatStatus = append(res.NatStatus, st)
		}
		return nil
	})
	writeJSON(w, http.StatusOK, &computev1.RouterStatusResponse{Kind: "compute#routerStatusResponse", Result: res})
}

// getNatMappingInfo lists per-VM NAT port ranges (FR-NAT-003). Each
// workload holding an IP in a covered subnetwork gets minPortsPerVm ports
// (default 64) on the NAT's first IP, allocated in IP order.
func (s *Service) getNatMappingInfo(w http.ResponseWriter, r *http.Request) {
	path, rt, err := s.loadRouter(r, "compute.routers.get")
	if err != nil {
		apierr.Write(w, err)
		return
	}
	natName := r.URL.Query().Get("natName")
	var items []listItem
	_ = s.env.Store.View(func(tx store.Tx) error {
		for _, nat := range rt.Nats {
			if natName != "" && nat.Name != natName {
				continue
			}
			ips := natIPs(tx, path, nat)
			per := nat.MinPortsPerVm
			if per == 0 {
				per = 64
				if nat.EnableDynamicPortAllocation {
					per = 32
				}
			}
			for i, vm := range s.natVMs(tx, rt, nat) {
				m := &computev1.VmEndpointNatMappingsInterfaceNatMappings{SourceVirtualIp: vm.ip, NumTotalNatPorts: per}
				if len(ips) > 0 {
					lo := 1024 + int64(i)*per
					m.NatIpPortRanges = []string{fmt.Sprintf("%s:%d-%d", ips[0], lo, lo+per-1)}
				}
				m.ForceSendFields = []string{"NumTotalDrainNatPorts"}
				items = append(items, listItem{key: nat.Name + "/" + vm.ip, v: &computev1.VmEndpointNatMappings{
					InstanceName:         vm.owner,
					InterfaceNatMappings: []*computev1.VmEndpointNatMappingsInterfaceNatMappings{m},
				}})
			}
		}
		return nil
	})
	q := r.URL.Query()
	q.Del("natName")
	r2 := r.Clone(r.Context())
	r2.URL.RawQuery = q.Encode()
	pg, next, err := page(r2, items)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	out := map[string]any{"kind": "compute#vmEndpointNatMappingsList", "id": path + "/getNatMappingInfo", "selfLink": link(path) + "/getNatMappingInfo"}
	if len(pg) > 0 {
		out["result"] = pg
	}
	if next != "" {
		out["nextPageToken"] = next
	}
	writeJSON(w, http.StatusOK, out)
}

// natRanges returns the CIDRs of subnetwork sn a NAT covers (FR-NAT-002).
// primary is the effective primary range (the container network's subnet,
// which differs from ipCidrRange after an overlap fallback).
func natRanges(nat *computev1.RouterNat, sn *computev1.Subnetwork, primary string) []string {
	var secondary []string
	for _, sr := range sn.SecondaryIpRanges {
		secondary = append(secondary, sr.IpCidrRange)
	}
	switch nat.SourceSubnetworkIpRangesToNat {
	case "ALL_SUBNETWORKS_ALL_IP_RANGES":
		return append([]string{primary}, secondary...)
	case "ALL_SUBNETWORKS_ALL_PRIMARY_IP_RANGES":
		return []string{primary}
	case "LIST_OF_SUBNETWORKS":
		for _, sub := range nat.Subnetworks {
			if relPath(sub.Name) != relPath(sn.SelfLink) {
				continue
			}
			var out []string
			if contains(sub.SourceIpRangesToNat, "ALL_IP_RANGES") {
				return append([]string{primary}, secondary...)
			}
			if contains(sub.SourceIpRangesToNat, "PRIMARY_IP_RANGE") {
				out = append(out, primary)
			}
			if contains(sub.SourceIpRangesToNat, "LIST_OF_SECONDARY_IP_RANGES") {
				for _, sr := range sn.SecondaryIpRanges {
					if contains(sub.SecondaryIpRangeNames, sr.RangeName) {
						out = append(out, sr.IpCidrRange)
					}
				}
			}
			return out
		}
	}
	return nil
}
