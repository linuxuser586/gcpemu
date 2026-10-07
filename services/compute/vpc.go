package compute

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"strconv"
	"strings"

	computev1 "google.golang.org/api/compute/v1"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/locations"
	"github.com/linuxuser586/gcpemu/internal/runtime"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// emu.VPC: VPC subnetworks (and private services access ranges) are
// realised as container bridge networks named
// gcpemu-<id>-vpc-<network>-<subnet>-<hash>, with the subnetwork's range
// and its first host address as the (host-side) gateway, like GCP's .1.
// When the range overlaps another network on the host (another emulator
// instance, a docker network), a free range is used instead and reported
// in SubnetNet.Subnet. Realisation state and IP allocations are persisted,
// and networks are reused idempotently after a restart.
//
// The bridges are not --internal: Docker's internal-network rules drop
// routed traffic, which would make the egress gateway impossible. Instead
// they have IP masquerading disabled (so the host never NATs workload
// traffic to the internet) and gateway mode nat-unprotected (so routing
// through the egress gateway between VPC networks works). Workloads must
// use EgressGateway as their default route.

const (
	nsVPCNets = "compute/vpcnets"
	// nsIPAlloc holds "NET/ip/A.B.C.D" → owner and "NET/owner/OWNER" → ip.
	nsIPAlloc = "compute/ipalloc"
)

// vpcNet is the persisted realisation of a subnetwork or PSA range.
type vpcNet struct {
	Name     string `json:"name"`     // runtime network name
	Subnet   string `json:"subnet"`   // effective CIDR
	Gateway  string `json:"gateway"`  // host-side address
	Range    string `json:"range"`    // the GCP range
	Fallback bool   `json:"fallback"` // Subnet != Range
	Network  string `json:"network"`  // VPC network path
	Region   string `json:"region,omitempty"`
	// Subnetwork is the subnetwork path, empty for PSA ranges.
	Subnetwork string `json:"subnetwork,omitempty"`
}

func psaKey(np string) string { return "psa:" + np }

func shortHash(s string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(s))
	return fmt.Sprintf("%08x", h.Sum32())
}

// bridgeOptions keep workload traffic from being NATed by the host and
// allow routing between the emulator's bridges (see above).
var bridgeOptions = map[string]string{
	"com.docker.network.bridge.enable_ip_masquerade": "false",
	"com.docker.network.bridge.gateway_mode_ipv4":    "nat-unprotected",
}

func (s *Service) runtime(ctx context.Context) (*runtime.Manager, error) {
	if s.env.Containers == nil {
		return nil, apierr.FailedPrecondition("No container runtime is available to realise VPC networks.")
	}
	rt, err := s.env.Containers.Runtime(ctx)
	if err != nil {
		return nil, apierr.FailedPrecondition("No container runtime is available to realise VPC networks: %v", err)
	}
	return rt, nil
}

// ResolveSubnetwork implements emu.VPC.
func (s *Service) ResolveSubnetwork(ctx context.Context, project, region, network, subnetwork string) (string, error) {
	if err := s.env.EnsureProject(project); err != nil {
		return "", err
	}
	if !locations.IsRegion(region) {
		return "", apierr.InvalidArgument("Invalid region %q.", region)
	}
	var np string
	if network != "" {
		p, err := globalRef(project, "networks", network)
		if err != nil {
			return "", apierr.InvalidArgument("Invalid network %q.", network)
		}
		np = p
	}
	if subnetwork != "" {
		sp, err := regionalRef(project, region, "subnetworks", subnetwork)
		if err != nil {
			return "", apierr.InvalidArgument("Invalid subnetwork %q.", subnetwork)
		}
		var sn *computev1.Subnetwork
		var ok bool
		_ = s.env.Store.View(func(tx store.Tx) error { sn, ok = get[computev1.Subnetwork](tx, nsSubnets, sp); return nil })
		if !ok {
			if np == "" && lastSeg(sp) == "default" {
				if err := s.ensureDefaultNetwork(project); err != nil {
					return "", err
				}
				return s.ResolveSubnetwork(ctx, project, region, network, "")
			}
			return "", apierr.NotFound("Subnetwork %q not found.", sp)
		}
		if np != "" && relPath(sn.Network) != np {
			return "", apierr.InvalidArgument("Subnetwork %q is not in network %q.", sp, np)
		}
		if _, r, _ := pathParts(sp); r != region {
			return "", apierr.InvalidArgument("Subnetwork %q is not in region %q.", sp, region)
		}
		return sp, nil
	}
	if np == "" || np == networkPath(project, "default") {
		if err := s.ensureDefaultNetwork(project); err != nil {
			return "", err
		}
		np = networkPath(project, "default")
	}
	var found []string
	var n *computev1.Network
	_ = s.env.Store.View(func(tx store.Tx) error {
		n, _ = get[computev1.Network](tx, nsNetworks, np)
		for _, sn := range list[computev1.Subnetwork](tx, nsSubnets, "projects/"+project+"/regions/"+region+"/subnetworks/") {
			if relPath(sn.Network) == np {
				found = append(found, relPath(sn.SelfLink))
			}
		}
		return nil
	})
	if n == nil {
		return "", apierr.NotFound("Network %q not found.", np)
	}
	if n.AutoCreateSubnetworks {
		auto := subnetPath(project, region, n.Name)
		if contains(found, auto) {
			return auto, nil
		}
	}
	switch len(found) {
	case 0:
		return "", apierr.FailedPrecondition("Network %q has no subnetwork in region %q.", np, region)
	case 1:
		return found[0], nil
	}
	return "", apierr.InvalidArgument("Network %q has several subnetworks in region %q; specify one.", np, region)
}

// SubnetNetwork implements emu.VPC: it realises the subnetwork as a
// container network on first use.
func (s *Service) SubnetNetwork(ctx context.Context, subnetwork string) (emu.SubnetNet, error) {
	sp := relPath(subnetwork)
	var sn *computev1.Subnetwork
	var ok bool
	_ = s.env.Store.View(func(tx store.Tx) error { sn, ok = get[computev1.Subnetwork](tx, nsSubnets, sp); return nil })
	if !ok {
		return emu.SubnetNet{}, apierr.NotFound("Subnetwork %q not found.", sp)
	}
	rt, err := s.runtime(ctx)
	if err != nil {
		return emu.SubnetNet{}, err
	}
	_, region, name := pathParts(sp)
	np := relPath(sn.Network)
	st, err := s.realize(ctx, rt, sp, rt.Name("vpc", lastSeg(np), name, shortHash(sp)), sn.IpCidrRange, np, region, sp)
	if err != nil {
		return emu.SubnetNet{}, err
	}
	out := emu.SubnetNet{
		Name: st.Name, Subnet: st.Subnet, Gateway: st.Gateway,
		SubnetworkName: sp, NetworkName: np, Region: region,
		SecondaryRanges: map[string]string{},
	}
	for _, sr := range sn.SecondaryIpRanges {
		out.SecondaryRanges[sr.RangeName] = sr.IpCidrRange
	}
	s.attachIfGateway(ctx, np)
	return out, nil
}

// PrivateServicesNetwork implements emu.VPC: it realises the first
// reserved range of the network's servicenetworking connection.
func (s *Service) PrivateServicesNetwork(ctx context.Context, network string) (emu.SubnetNet, error) {
	segs := strings.Split(relPath(network), "/")
	if len(segs) != 5 || segs[2] != "global" || segs[3] != "networks" {
		return emu.SubnetNet{}, apierr.InvalidArgument("Invalid network %q.", network)
	}
	np := networkPath(s.projectIDOf(segs[1]), segs[4])
	var rng string
	err := s.env.Store.View(func(tx store.Tx) error {
		c, ok := get[psaConnection](tx, nsPSA, np)
		if !ok || len(c.ReservedPeeringRanges) == 0 {
			return apierr.FailedPrecondition("Network '%s' has no private services access connection; create one with servicenetworking (google_service_networking_connection).", np).
				WithReason("compute.googleapis.com", "PRIVATE_SERVICE_ACCESS_NOT_CONFIGURED")
		}
		p, _, _ := pathParts(np)
		a, ok := get[computev1.Address](tx, nsGlobalAddresses, "projects/"+p+"/global/addresses/"+c.ReservedPeeringRanges[0])
		if !ok {
			return apierr.FailedPrecondition("Allocated range %q of network '%s' no longer exists.", c.ReservedPeeringRanges[0], np)
		}
		rng = a.Address + "/" + strconv.Itoa(int(a.PrefixLength))
		return nil
	})
	if err != nil {
		return emu.SubnetNet{}, err
	}
	rt, err := s.runtime(ctx)
	if err != nil {
		return emu.SubnetNet{}, err
	}
	key := psaKey(np)
	st, err := s.realize(ctx, rt, key, rt.Name("vpc", lastSeg(np), "psa", shortHash(key)), rng, np, "", "")
	if err != nil {
		return emu.SubnetNet{}, err
	}
	s.attachIfGateway(ctx, np)
	return emu.SubnetNet{Name: st.Name, Subnet: st.Subnet, Gateway: st.Gateway, NetworkName: np}, nil
}

// realize creates (or adopts) the container network for key.
func (s *Service) realize(ctx context.Context, rt *runtime.Manager, key, name, rng, np, region, sp string) (*vpcNet, error) {
	s.vpcMu.Lock()
	defer s.vpcMu.Unlock()
	labels := rt.Labels("compute", key, "vpc")
	n, err := rt.InspectNetwork(ctx, name)
	if errors.Is(err, runtime.ErrNotFound) {
		n, err = s.createBridge(ctx, rt, name, rng, labels)
	}
	if err != nil {
		return nil, apierr.Internal("Realising VPC range %s as container network %s: %v", rng, name, err)
	}
	st := &vpcNet{Name: n.Name, Subnet: n.Subnet, Gateway: n.Gateway, Range: rng, Network: np, Region: region, Subnetwork: sp}
	st.Fallback = st.Subnet != rng
	if err := s.env.Store.Update(func(tx store.Tx) error { return store.PutJSON(tx, nsVPCNets, key, st) }); err != nil {
		return nil, err
	}
	return st, nil
}

// createBridge creates a bridge with the requested range, falling back to
// a free range on overlap.
func (s *Service) createBridge(ctx context.Context, rt *runtime.Manager, name, rng string, labels map[string]string) (runtime.Network, error) {
	c, err := parseCIDR(rng, false)
	if err != nil {
		return runtime.Network{}, err
	}
	opts := bridgeOptions
	try := func(subnet, gw string) error {
		_, err := rt.CreateNetwork(ctx, runtime.NetworkSpec{Name: name, Subnet: subnet, Gateway: gw, Labels: labels, Options: opts})
		if err != nil && opts != nil && !isOverlap(err) && !isExists(err) {
			// Runtimes that do not know the Docker bridge options (Podman).
			s.env.Log.Warn("compute: runtime rejected bridge options; NAT isolation relies on the egress gateway route only", "network", name, "err", err)
			opts = nil
			_, err = rt.CreateNetwork(ctx, runtime.NetworkSpec{Name: name, Subnet: subnet, Gateway: gw, Labels: labels})
		}
		return err
	}
	err = try(c.String(), ipString(c.gateway()))
	if err != nil && isOverlap(err) {
		var picked string
		picked, err = s.pickFreeRange(ctx, rt, name, c, labels, try)
		if err == nil {
			s.env.Log.Warn("compute: VPC range overlaps another network on this host; using a different range for the container network",
				"network", name, "range", c.String(), "effective", picked)
		}
	}
	if err != nil && !isExists(err) {
		return runtime.Network{}, err
	}
	return rt.InspectNetwork(ctx, name)
}

// pickFreeRange finds a free range: first one the runtime chooses from its
// default pools, then same-sized ranges in 10.0.0.0/8 above 10.240.0.0.
func (s *Service) pickFreeRange(ctx context.Context, rt *runtime.Manager, name string, want cidr, labels map[string]string, try func(subnet, gw string) error) (string, error) {
	probe := name + "-probe"
	if _, err := rt.CreateNetwork(ctx, runtime.NetworkSpec{Name: probe, Labels: labels}); err == nil {
		n, ierr := rt.InspectNetwork(ctx, probe)
		_ = rt.RemoveNetwork(ctx, probe)
		if ierr == nil && n.Subnet != "" {
			if c, err := parseCIDR(n.Subnet, false); err == nil {
				if err := try(c.String(), ipString(c.gateway())); err == nil {
					return c.String(), nil
				}
			}
		}
	}
	bits := want.bits
	if bits < 16 {
		bits = 16
	}
	start, _ := parseCIDR("10.240.0.0/12", true)
	step := uint32(1) << (32 - bits)
	off := uint32(0)
	if h, err := strconv.ParseUint(shortHash(name), 16, 32); err == nil {
		off = (uint32(h) % uint32(start.size()/uint64(step))) * step
	}
	for i := uint32(0); i < 64; i++ {
		base := start.base + (off+i*step)%uint32(start.size())
		c := cidr{base: base, bits: bits}
		err := try(c.String(), ipString(c.gateway()))
		if err == nil {
			return c.String(), nil
		}
		if !isOverlap(err) {
			return "", err
		}
	}
	return "", fmt.Errorf("no free address range for %s", name)
}

func isOverlap(err error) bool {
	m := strings.ToLower(err.Error())
	return strings.Contains(m, "overlap") || strings.Contains(m, "already used") || strings.Contains(m, "in use")
}

func isExists(err error) bool {
	m := strings.ToLower(err.Error())
	return strings.Contains(m, "already exists")
}

// stateOf finds the realisation of a SubnetNet.
func stateOf(tx store.Tx, net emu.SubnetNet) (*vpcNet, string) {
	if net.SubnetworkName != "" {
		st, _ := get[vpcNet](tx, nsVPCNets, relPath(net.SubnetworkName))
		return st, relPath(net.SubnetworkName)
	}
	if net.NetworkName != "" {
		k := psaKey(relPath(net.NetworkName))
		st, _ := get[vpcNet](tx, nsVPCNets, k)
		return st, k
	}
	return nil, ""
}

// AllocateIP implements emu.VPC: it reserves the lowest free address of
// the container network for owner, skipping the network and gateway
// addresses, GCP's reserved second-to-last address (where the egress
// gateway lives) and the broadcast address, plus internal addresses
// reserved through the compute API.
func (s *Service) AllocateIP(ctx context.Context, net emu.SubnetNet, owner string) (string, error) {
	if owner == "" {
		return "", apierr.InvalidArgument("AllocateIP: owner is required")
	}
	c, err := parseCIDR(net.Subnet, false)
	if err != nil {
		return "", apierr.InvalidArgument("AllocateIP: invalid subnet %q", net.Subnet)
	}
	s.vpcMu.Lock()
	defer s.vpcMu.Unlock()
	var ip string
	err = s.env.Store.Update(func(tx store.Tx) error {
		if b, ok := tx.Get(nsIPAlloc, net.Name+"/owner/"+owner); ok {
			ip = string(b)
			return nil
		}
		used := allocatedIPs(tx, net.Name)
		if st, _ := stateOf(tx, net); st != nil && !st.Fallback && st.Subnetwork != "" {
			for _, a := range list[computev1.Address](tx, nsAddresses, "projects/") {
				if a.AddressType == "INTERNAL" && relPath(a.Subnetwork) == st.Subnetwork {
					if v, ok := parseIP(a.Address); ok {
						used[v] = true
					}
				}
			}
		}
		for v := c.base + 2; v < c.last(); v++ {
			if c.usable(v) && !used[v] {
				ip = ipString(v)
				if err := tx.Put(nsIPAlloc, net.Name+"/ip/"+ip, []byte(owner)); err != nil {
					return err
				}
				return tx.Put(nsIPAlloc, net.Name+"/owner/"+owner, []byte(ip))
			}
		}
		return apierr.New(8, "IP space of %s (%s) is exhausted.", net.SubnetworkName, net.Subnet).WithLegacy("ipSpaceExhausted")
	})
	return ip, err
}

// ReleaseIP implements emu.VPC.
func (s *Service) ReleaseIP(ctx context.Context, net emu.SubnetNet, owner string) error {
	s.vpcMu.Lock()
	defer s.vpcMu.Unlock()
	return s.env.Store.Update(func(tx store.Tx) error {
		b, ok := tx.Get(nsIPAlloc, net.Name+"/owner/"+owner)
		if !ok {
			return nil
		}
		if err := tx.Delete(nsIPAlloc, net.Name+"/ip/"+string(b)); err != nil {
			return err
		}
		return tx.Delete(nsIPAlloc, net.Name+"/owner/"+owner)
	})
}

func allocatedIPs(tx store.Tx, name string) map[uint32]bool {
	out := map[uint32]bool{}
	tx.Scan(nsIPAlloc, name+"/ip/", func(k string, _ []byte) bool {
		if v, ok := parseIP(strings.TrimPrefix(k, name+"/ip/")); ok {
			out[v] = true
		}
		return true
	})
	return out
}

type ownedIP struct{ owner, ip string }

func ownersOf(tx store.Tx, name string) []ownedIP {
	var out []ownedIP
	tx.Scan(nsIPAlloc, name+"/owner/", func(k string, v []byte) bool {
		out = append(out, ownedIP{owner: strings.TrimPrefix(k, name+"/owner/"), ip: string(v)})
		return true
	})
	return out
}

func firstOwner(tx store.Tx, name string) string {
	if o := ownersOf(tx, name); len(o) > 0 {
		return o[0].owner
	}
	return ""
}

// releaseNetworkRuntime removes the container networks of deleted
// subnetworks / PSA ranges (keys) and, when np is a deleted VPC network,
// its egress gateway. It never initialises the runtime itself.
func (s *Service) releaseNetworkRuntime(ctx context.Context, np string, keys []string) {
	var states []*vpcNet
	_ = s.env.Store.Update(func(tx store.Tx) error {
		for _, k := range keys {
			if st, ok := get[vpcNet](tx, nsVPCNets, k); ok {
				states = append(states, st)
				_ = tx.Delete(nsVPCNets, k)
				var stale []string
				tx.Scan(nsIPAlloc, st.Name+"/", func(key string, _ []byte) bool { stale = append(stale, key); return true })
				for _, key := range stale {
					_ = tx.Delete(nsIPAlloc, key)
				}
			}
		}
		if np != "" {
			if st, ok := get[vpcNet](tx, nsVPCNets, psaKey(np)); ok {
				states = append(states, st)
				_ = tx.Delete(nsVPCNets, psaKey(np))
			}
		}
		return nil
	})
	if np != "" {
		s.removeGateway(ctx, np)
	}
	if len(states) == 0 {
		return
	}
	rt, err := s.runtime(ctx)
	if err != nil {
		return
	}
	for _, st := range states {
		if g := s.gatewayFor(st.Network); g != nil {
			g.detach(ctx, st.Name)
		}
		if err := rt.RemoveNetwork(ctx, st.Name); err != nil {
			s.env.Log.Warn("compute: removing VPC container network", "network", st.Name, "err", err)
		}
	}
}

// netsOf lists the realised container networks of a VPC network.
func (s *Service) netsOf(np string) []*vpcNet {
	var out []*vpcNet
	_ = s.env.Store.View(func(tx store.Tx) error {
		for _, st := range list[vpcNet](tx, nsVPCNets, "") {
			if st.Network == np {
				out = append(out, st)
			}
		}
		return nil
	})
	return out
}
