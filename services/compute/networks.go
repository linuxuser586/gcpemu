package compute

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strconv"

	computev1 "google.golang.org/api/compute/v1"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/locations"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// VPC networks (auto and custom mode). Auto-mode networks get one
// subnetwork per region with GCP's real auto-mode ranges; every network
// gets a default route to the internet gateway, as in GCP.

const (
	nsNetworks = "compute/networks"
	nsRoutes   = "compute/routes"

	defaultRouteDesc = "Default route to the Internet."
)

func networkPath(project, name string) string {
	return "projects/" + project + "/global/networks/" + name
}

func (s *Service) insertNetwork(w http.ResponseWriter, r *http.Request) {
	p := r.PathValue("project")
	if err := s.env.EnsureProject(p); err != nil {
		apierr.Write(w, err)
		return
	}
	var n computev1.Network
	if _, err := decode(r, &n); err != nil {
		apierr.Write(w, err)
		return
	}
	if err := validName("resource.name", n.Name); err != nil {
		apierr.Write(w, err)
		return
	}
	path := networkPath(p, n.Name)
	if err := s.check(r.Context(), "compute.networks.create", path); err != nil {
		apierr.Write(w, err)
		return
	}
	if n.IPv4Range != "" {
		apierr.Write(w, errInvalidField("resource.IPv4Range", n.IPv4Range, "Legacy networks are not supported; set autoCreateSubnetworks instead."))
		return
	}
	if n.Mtu != 0 && (n.Mtu < 1300 || n.Mtu > 8896) {
		apierr.Write(w, errInvalidField("resource.mtu", n.Mtu, "Must be between 1300 and 8896."))
		return
	}
	exists := false
	_ = s.env.Store.View(func(tx store.Tx) error { exists = store.Exists(tx, nsNetworks, path); return nil })
	if exists {
		apierr.Write(w, errExists(path))
		return
	}
	n.Id = s.env.IDs.Uint64()
	op, err := s.startOp(r.Context(), opSpec{project: p, scope: "global", opType: "insert", target: path, targetID: n.Id},
		func(ctx context.Context) error { return s.createNetwork(p, &n) })
	reply(w, op, err)
}

// createNetwork stores a network (and its auto-mode subnetworks and
// default route).
func (s *Service) createNetwork(project string, n *computev1.Network) error {
	path := networkPath(project, n.Name)
	if n.Id == 0 {
		n.Id = s.env.IDs.Uint64()
	}
	n.Kind = "compute#network"
	n.CreationTimestamp = stamp(s.env.Clock.Now())
	n.SelfLink = link(path)
	n.SelfLinkWithId = link(networkPath(project, strconv.FormatUint(n.Id, 10)))
	if n.Mtu == 0 {
		n.Mtu = 1460
	}
	if n.RoutingConfig == nil {
		n.RoutingConfig = &computev1.NetworkRoutingConfig{}
	}
	if n.RoutingConfig.RoutingMode == "" {
		n.RoutingConfig.RoutingMode = "REGIONAL"
	}
	if n.RoutingConfig.BgpBestPathSelectionMode == "" {
		n.RoutingConfig.BgpBestPathSelectionMode = "LEGACY"
	}
	if n.NetworkFirewallPolicyEnforcementOrder == "" {
		n.NetworkFirewallPolicyEnforcementOrder = "AFTER_CLASSIC_FIREWALL"
	}
	n.Subnetworks = nil
	n.Peerings = nil
	n.GatewayIPv4 = ""
	now := stamp(s.env.Clock.Now())
	return s.env.Store.Update(func(tx store.Tx) error {
		if store.Exists(tx, nsNetworks, path) {
			return errExists(path)
		}
		if err := store.PutJSON(tx, nsNetworks, path, n); err != nil {
			return err
		}
		if n.AutoCreateSubnetworks {
			for _, reg := range locations.Regions() {
				sp := subnetPath(project, reg.Name, n.Name)
				sn := &computev1.Subnetwork{
					Kind:                    "compute#subnetwork",
					Id:                      s.env.IDs.Uint64(),
					CreationTimestamp:       now,
					Name:                    n.Name,
					Network:                 n.SelfLink,
					IpCidrRange:             reg.AutoCIDR,
					Region:                  link("projects/" + project + "/regions/" + reg.Name),
					SelfLink:                link(sp),
					PrivateIpGoogleAccess:   false,
					PrivateIpv6GoogleAccess: "DISABLE_GOOGLE_ACCESS",
					Purpose:                 "PRIVATE",
					StackType:               "IPV4_ONLY",
					State:                   "READY",
				}
				c, _ := parseCIDR(reg.AutoCIDR, true)
				sn.GatewayAddress = ipString(c.gateway())
				sn.Fingerprint = fingerprint(sn)
				if err := store.PutJSON(tx, nsSubnets, sp, sn); err != nil {
					return err
				}
			}
		}
		rname := "default-route-" + s.env.IDs.Hex(8)
		rp := "projects/" + project + "/global/routes/" + rname
		return store.PutJSON(tx, nsRoutes, rp, &computev1.Route{
			Kind:              "compute#route",
			Id:                s.env.IDs.Uint64(),
			CreationTimestamp: now,
			Name:              rname,
			Description:       defaultRouteDesc,
			Network:           n.SelfLink,
			DestRange:         "0.0.0.0/0",
			Priority:          1000,
			NextHopGateway:    link("projects/" + project + "/global/gateways/default-internet-gateway"),
			RouteType:         "STATIC",
			SelfLink:          link(rp),
		})
	})
}

// decorateNetwork fills the output-only subnetworks and peerings fields.
func (s *Service) decorateNetwork(tx store.Tx, n *computev1.Network) {
	project, _, _ := pathParts(relPath(n.SelfLink))
	n.Subnetworks = nil
	for _, sn := range list[computev1.Subnetwork](tx, nsSubnets, "projects/"+project+"/regions/") {
		if relPath(sn.Network) == relPath(n.SelfLink) {
			n.Subnetworks = append(n.Subnetworks, sn.SelfLink)
		}
	}
	n.Peerings = nil
	if c, ok := get[psaConnection](tx, nsPSA, relPath(n.SelfLink)); ok {
		n.Peerings = append(n.Peerings, c.peering(s))
	}
}

func (s *Service) loadNetwork(r *http.Request, perm string) (string, *computev1.Network, error) {
	p := r.PathValue("project")
	if err := s.env.EnsureProject(p); err != nil {
		return "", nil, err
	}
	path := networkPath(p, r.PathValue("network"))
	if err := s.check(r.Context(), perm, path); err != nil {
		return "", nil, err
	}
	var n *computev1.Network
	var ok bool
	_ = s.env.Store.View(func(tx store.Tx) error {
		n, ok = get[computev1.Network](tx, nsNetworks, path)
		if ok {
			s.decorateNetwork(tx, n)
		}
		return nil
	})
	if !ok {
		return "", nil, errNotFound(path)
	}
	return path, n, nil
}

func (s *Service) getNetwork(w http.ResponseWriter, r *http.Request) {
	_, n, err := s.loadNetwork(r, "compute.networks.get")
	reply(w, n, err)
}

func (s *Service) listNetworks(w http.ResponseWriter, r *http.Request) {
	p := r.PathValue("project")
	if err := s.env.EnsureProject(p); err != nil {
		apierr.Write(w, err)
		return
	}
	if err := s.check(r.Context(), "compute.networks.list", "projects/"+p); err != nil {
		apierr.Write(w, err)
		return
	}
	var items []listItem
	_ = s.env.Store.View(func(tx store.Tx) error {
		for _, n := range list[computev1.Network](tx, nsNetworks, "projects/"+p+"/global/networks/") {
			s.decorateNetwork(tx, n)
			items = append(items, listItem{key: n.Name, v: n})
		}
		return nil
	})
	writeList(w, r, "compute#networkList", "projects/"+p+"/global/networks", items)
}

// patchNetwork updates the mutable fields (routingConfig, mtu,
// networkFirewallPolicyEnforcementOrder, description).
func (s *Service) patchNetwork(w http.ResponseWriter, r *http.Request) {
	path, cur, err := s.loadNetwork(r, "compute.networks.update")
	if err != nil {
		apierr.Write(w, err)
		return
	}
	raw, err := readBody(r, false)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	var next computev1.Network
	if err := mergePatch(cur, raw, &next, "name", "id", "kind", "selfLink", "selfLinkWithId", "creationTimestamp",
		"autoCreateSubnetworks", "IPv4Range", "gatewayIPv4", "subnetworks", "peerings"); err != nil {
		apierr.Write(w, err)
		return
	}
	if next.Mtu != 0 && (next.Mtu < 1300 || next.Mtu > 8896) {
		apierr.Write(w, errInvalidField("resource.mtu", next.Mtu, "Must be between 1300 and 8896."))
		return
	}
	p, _, _ := pathParts(path)
	op, err := s.startOp(r.Context(), opSpec{project: p, scope: "global", opType: "patch", target: path, targetID: cur.Id},
		func(ctx context.Context) error {
			return s.env.Store.Update(func(tx store.Tx) error {
				if !store.Exists(tx, nsNetworks, path) {
					return errNotFound(path)
				}
				next.Subnetworks, next.Peerings = nil, nil
				return store.PutJSON(tx, nsNetworks, path, &next)
			})
		})
	reply(w, op, err)
}

// switchToCustomMode turns an auto-mode network into a custom-mode one.
func (s *Service) switchToCustomMode(w http.ResponseWriter, r *http.Request) {
	path, cur, err := s.loadNetwork(r, "compute.networks.switchToCustomMode")
	if err != nil {
		apierr.Write(w, err)
		return
	}
	if !cur.AutoCreateSubnetworks {
		apierr.Write(w, apierr.InvalidArgument("Network '%s' is already a custom mode network.", path).WithLegacy("invalid"))
		return
	}
	p, _, _ := pathParts(path)
	op, err := s.startOp(r.Context(), opSpec{project: p, scope: "global", opType: "switchLegacyToCustomModeNetwork", target: path, targetID: cur.Id},
		func(ctx context.Context) error {
			return s.env.Store.Update(func(tx store.Tx) error {
				n, ok := get[computev1.Network](tx, nsNetworks, path)
				if !ok {
					return errNotFound(path)
				}
				n.AutoCreateSubnetworks = false
				n.ForceSendFields = []string{"AutoCreateSubnetworks"}
				return store.PutJSON(tx, nsNetworks, path, n)
			})
		})
	reply(w, op, err)
}

func (s *Service) deleteNetwork(w http.ResponseWriter, r *http.Request) {
	path, cur, err := s.loadNetwork(r, "compute.networks.delete")
	if err != nil {
		apierr.Write(w, err)
		return
	}
	if err := s.networkInUse(r.Context(), path, cur); err != nil {
		apierr.Write(w, err)
		return
	}
	p, _, _ := pathParts(path)
	op, err := s.startOp(r.Context(), opSpec{project: p, scope: "global", opType: "delete", target: path, targetID: cur.Id},
		func(ctx context.Context) error {
			if err := s.networkInUse(ctx, path, cur); err != nil {
				return err
			}
			var subnets []string
			err := s.env.Store.Update(func(tx store.Tx) error {
				for _, sn := range list[computev1.Subnetwork](tx, nsSubnets, "projects/"+p+"/regions/") {
					if relPath(sn.Network) == path {
						sp := relPath(sn.SelfLink)
						subnets = append(subnets, sp)
						if err := tx.Delete(nsSubnets, sp); err != nil {
							return err
						}
					}
				}
				for _, rt := range list[computev1.Route](tx, nsRoutes, "projects/"+p+"/global/routes/") {
					if relPath(rt.Network) == path {
						if err := tx.Delete(nsRoutes, relPath(rt.SelfLink)); err != nil {
							return err
						}
					}
				}
				return tx.Delete(nsNetworks, path)
			})
			if err != nil {
				return err
			}
			s.releaseNetworkRuntime(ctx, path, subnets)
			return nil
		})
	reply(w, op, err)
}

// networkInUse enforces FR-CORE-026 for networks: subnetworks of custom
// networks (and used auto subnetworks), firewalls, user routes, routers,
// addresses, NEGs and private services access connections block deletion.
func (s *Service) networkInUse(ctx context.Context, path string, n *computev1.Network) error {
	p, _, _ := pathParts(path)
	var user string
	_ = s.env.Store.View(func(tx store.Tx) error {
		same := func(ref string) bool { return ref != "" && relPath(ref) == path }
		for _, sn := range list[computev1.Subnetwork](tx, nsSubnets, "projects/"+p+"/regions/") {
			if !same(sn.Network) {
				continue
			}
			sp := relPath(sn.SelfLink)
			if !n.AutoCreateSubnetworks || sn.Name != n.Name {
				user = sp
				return nil
			}
			if u := s.subnetUserTx(tx, sp); u != "" {
				user = u
				return nil
			}
		}
		for _, f := range list[computev1.Firewall](tx, nsFirewalls, "projects/"+p+"/global/firewalls/") {
			if same(f.Network) {
				user = relPath(f.SelfLink)
				return nil
			}
		}
		for _, rt := range list[computev1.Route](tx, nsRoutes, "projects/"+p+"/global/routes/") {
			if same(rt.Network) && rt.Description != defaultRouteDesc {
				user = relPath(rt.SelfLink)
				return nil
			}
		}
		for _, rt := range list[computev1.Router](tx, nsRouters, "projects/"+p+"/regions/") {
			if same(rt.Network) {
				user = relPath(rt.SelfLink)
				return nil
			}
		}
		for _, ns := range []string{nsAddresses, nsGlobalAddresses} {
			for _, a := range list[computev1.Address](tx, ns, "projects/"+p+"/") {
				if same(a.Network) {
					user = relPath(a.SelfLink)
					return nil
				}
			}
		}
		for _, g := range list[computev1.NetworkEndpointGroup](tx, nsNEGs, "projects/"+p+"/") {
			if same(g.Network) {
				user = relPath(g.SelfLink)
				return nil
			}
		}
		if store.Exists(tx, nsPSA, path) {
			user = "projects/" + p + "/global/networks/" + lastSeg(path) + "/peerings/" + psaPeeringName
			return nil
		}
		return nil
	})
	if user == "" {
		user = s.externalUser(ctx, path)
	}
	if user != "" {
		return errInUse("network", path, user)
	}
	return nil
}

// ensureDefaultNetwork creates the project's "default" auto-mode network
// with GCP's default firewall rules if it does not exist, as a new GCP
// project has.
func (s *Service) ensureDefaultNetwork(project string) error {
	path := networkPath(project, "default")
	exists := false
	_ = s.env.Store.View(func(tx store.Tx) error { exists = store.Exists(tx, nsNetworks, path); return nil })
	if exists {
		return nil
	}
	n := &computev1.Network{Name: "default", Description: "Default network for the project", AutoCreateSubnetworks: true}
	if err := s.createNetwork(project, n); err != nil {
		if apierr.From(err).LegacyReason == "alreadyExists" {
			return nil
		}
		return err
	}
	for _, fw := range defaultFirewalls(project) {
		if err := s.createFirewall(project, fw); err != nil && apierr.From(err).LegacyReason != "alreadyExists" {
			return err
		}
	}
	return nil
}

// defaultFirewalls are the rules GCP creates with the default network.
func defaultFirewalls(project string) []*computev1.Firewall {
	net := link(networkPath(project, "default"))
	mk := func(name, desc string, prio int64, src []string, allowed ...*computev1.FirewallAllowed) *computev1.Firewall {
		return &computev1.Firewall{Name: name, Description: desc, Network: net, Priority: prio, Direction: "INGRESS", SourceRanges: src, Allowed: allowed}
	}
	return []*computev1.Firewall{
		mk("default-allow-internal", "Allow internal traffic on the default network", 65534, []string{"10.128.0.0/9"},
			&computev1.FirewallAllowed{IPProtocol: "tcp", Ports: []string{"0-65535"}},
			&computev1.FirewallAllowed{IPProtocol: "udp", Ports: []string{"0-65535"}},
			&computev1.FirewallAllowed{IPProtocol: "icmp"}),
		mk("default-allow-ssh", "Allow SSH from anywhere", 65534, []string{"0.0.0.0/0"}, &computev1.FirewallAllowed{IPProtocol: "tcp", Ports: []string{"22"}}),
		mk("default-allow-rdp", "Allow RDP from anywhere", 65534, []string{"0.0.0.0/0"}, &computev1.FirewallAllowed{IPProtocol: "tcp", Ports: []string{"3389"}}),
		mk("default-allow-icmp", "Allow ICMP from anywhere", 65534, []string{"0.0.0.0/0"}, &computev1.FirewallAllowed{IPProtocol: "icmp"}),
	}
}

// networkOf loads a network by path inside a transaction.
func networkOf(tx store.Tx, path string) (*computev1.Network, error) {
	n, ok := get[computev1.Network](tx, nsNetworks, path)
	if !ok {
		return nil, errNotFound(path)
	}
	return n, nil
}

// resolveNetworkRef canonicalises a network reference of a request body,
// defaulting to the project's "default" network when empty.
func resolveNetworkRef(project, ref string) (string, error) {
	if ref == "" {
		ref = "default"
	}
	p, err := globalRef(project, "networks", ref)
	if err != nil {
		return "", errInvalidField("resource.network", ref, "The URL is malformed.")
	}
	return p, nil
}

// fingerprint returns a GCP-style fingerprint (8 bytes, base64) of v's
// content, ignoring the fingerprint field itself.
func fingerprint(v any) string {
	m := toMap(v)
	delete(m, "fingerprint")
	delete(m, "labelFingerprint")
	b, _ := json.Marshal(m)
	sum := sha256.Sum256(b)
	return base64.StdEncoding.EncodeToString(sum[:8])
}

// labelFingerprint is the fingerprint of a label set; GCP reports
// "42WmSpB8rSM=" for no labels.
func labelFingerprint(labels map[string]string) string {
	if len(labels) == 0 {
		return "42WmSpB8rSM="
	}
	b, _ := json.Marshal(labels)
	sum := sha256.Sum256(b)
	return base64.StdEncoding.EncodeToString(sum[:8])
}
