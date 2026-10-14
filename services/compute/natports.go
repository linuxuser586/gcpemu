package compute

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	computev1 "google.golang.org/api/compute/v1"

	"github.com/linuxuser586/gcpemu/internal/store"
)

// Cloud NAT port allocation (FR-NAT-003). Each workload a NAT serves gets
// a block of minPortsPerVm ports (default 64, or 32 with dynamic port
// allocation), laid out in address order over ports 1024-65535 of the
// NAT's IPs; a workload that does not fit gets no ports and cannot use the
// NAT. Drain IPs carry the same layout, standing for the blocks workloads
// held on them before they were drained.
//
// NAT IPs are emulated, so the NAT egress proxy translates every
// workload to its own external address, onto the port block the workload was allocated:
// getNatMappingInfo, getRouterStatus and the NAT logs name the NAT IP
// and port that the block stands for.

const (
	natFirstPort  = 1024
	natLastPort   = 65535
	natPortsPerIP = natLastPort + 1 - natFirstPort
)

// natPortRange is a block of ports on one NAT IP.
type natPortRange struct {
	ip     string
	lo, hi int64
}

func (r natPortRange) String() string { return fmt.Sprintf("%s:%d-%d", r.ip, r.lo, r.hi) }

// vmNatPorts is the allocation of one workload.
type vmNatPorts struct {
	natVM
	// port is the active block, drain the block on a drain IP, if any.
	port, drain *natPortRange
}

func (v vmNatPorts) active() []string  { return rangeStrings(v.port) }
func (v vmNatPorts) drained() []string { return rangeStrings(v.drain) }

func rangeStrings(r *natPortRange) []string {
	if r == nil {
		return nil
	}
	return []string{r.String()}
}

// natPorts is the port allocation of one NAT.
type natPorts struct {
	nat *computev1.RouterNat
	// ips and drain are the NAT's active and drain IP addresses.
	ips, drain []string
	perVM      int64
	vms        []vmNatPorts
	// extraIPs is how many more NAT IPs every workload needs to get a
	// block (minExtraNatIpsNeeded).
	extraIPs int64
}

// natIPOf is the NAT IP of src's block, or "" if src has none.
func (a natPorts) natIPOf(src string) string {
	for _, vm := range a.vms {
		if vm.ip == src && vm.port != nil {
			return vm.port.ip
		}
	}
	return ""
}

// natPortsPerVM is the block size of a NAT, at most one NAT IP's ports.
func natPortsPerVM(nat *computev1.RouterNat) int64 {
	per := nat.MinPortsPerVm
	if per == 0 {
		per = 64
		if nat.EnableDynamicPortAllocation {
			per = 32
		}
	}
	return min(per, natPortsPerIP)
}

// natMaxPortsPerVM is how many ports a workload may grow into: its block,
// or maxPortsPerVm (default 65536, every port) with dynamic port
// allocation.
func natMaxPortsPerVM(nat *computev1.RouterNat) int64 {
	if !nat.EnableDynamicPortAllocation {
		return natPortsPerVM(nat)
	}
	if nat.MaxPortsPerVm == 0 {
		return natLastPort + 1
	}
	return nat.MaxPortsPerVm
}

// natIPsFor is how many NAT IPs n workloads need.
func natIPsFor(nat *computev1.RouterNat, n int) int64 {
	per := natPortsPerIP / natPortsPerVM(nat)
	return (int64(n) + per - 1) / per
}

// allocateNatPorts lays out the blocks of vms (in address order) over the
// NAT's active ips and its drain IPs.
func allocateNatPorts(nat *computev1.RouterNat, vms []natVM, ips, drain []string) natPorts {
	per := natPortsPerVM(nat)
	blocks := natPortsPerIP / per
	at := func(pool []string, i int) *natPortRange {
		k := int64(i) / blocks
		if k >= int64(len(pool)) {
			return nil
		}
		lo := natFirstPort + int64(i)%blocks*per
		return &natPortRange{ip: pool[k], lo: lo, hi: lo + per - 1}
	}
	out := natPorts{nat: nat, ips: ips, drain: drain, perVM: per, extraIPs: max(0, natIPsFor(nat, len(vms))-int64(len(ips)))}
	for i, vm := range vms {
		out.vms = append(out.vms, vmNatPorts{natVM: vm, port: at(ips, i), drain: at(drain, i)})
	}
	return out
}

// natAllocation is the port allocation of nat on router rt.
func (s *Service) natAllocation(tx store.Tx, rt *computev1.Router, nat *computev1.RouterNat) natPorts {
	var drain []string
	for _, ref := range nat.DrainNatIps {
		if a, ok := get[computev1.Address](tx, nsAddresses, relPath(ref)); ok {
			drain = append(drain, a.Address)
		}
	}
	return allocateNatPorts(nat, s.natVMs(tx, rt, nat), natIPs(tx, relPath(rt.SelfLink), nat), drain)
}

// topUpNatIPs auto-allocates the NAT IPs an AUTO_ONLY NAT's workloads
// need, as GCP adds them while workloads join; it never releases any. When
// the external address pool runs out, the NAT keeps the IPs it has and
// reports the rest as minExtraNatIpsNeeded.
func (s *Service) topUpNatIPs(tx store.Tx, rt *computev1.Router, nat *computev1.RouterNat) error {
	if nat.NatIpAllocateOption != "AUTO_ONLY" {
		return nil
	}
	k := relPath(rt.SelfLink) + "/" + nat.Name
	var ips []string
	_ = store.GetJSON(tx, nsNatIPs, k, &ips)
	need := natIPsFor(nat, len(s.natVMs(tx, rt, nat)))
	if int64(len(ips)) >= need {
		return nil
	}
	for int64(len(ips)) < need {
		ip, err := nextExternalIP(tx, "extip/regional", regionalPool)
		if err != nil {
			s.env.Log.Warn("compute: Cloud NAT IPs exhausted", "nat", k, "need", need, "have", len(ips), "err", err)
			break
		}
		ips = append(ips, ip)
	}
	return store.PutJSON(tx, nsNatIPs, k, ips)
}

// routerNatPorts tops up the IPs of a router's AUTO_ONLY NATs and returns
// the port allocation of each of its NATs, in order.
func (s *Service) routerNatPorts(tx store.Tx, rt *computev1.Router) ([]natPorts, error) {
	var out []natPorts
	for _, nat := range rt.Nats {
		if err := s.topUpNatIPs(tx, rt, nat); err != nil {
			return nil, err
		}
		out = append(out, s.natAllocation(tx, rt, nat))
	}
	return out, nil
}

// natAllocations returns the port allocations of every NAT on a router of
// VPC network np, by "routerPath/natName".
func (s *Service) natAllocations(np string) (map[string]natPorts, error) {
	out := map[string]natPorts{}
	err := s.env.Store.Update(func(tx store.Tx) error {
		p, _, _ := pathParts(np)
		for _, rt := range list[computev1.Router](tx, nsRouters, "projects/"+p+"/regions/") {
			if relPath(rt.Network) != np {
				continue
			}
			ports, err := s.routerNatPorts(tx, rt)
			if err != nil {
				return err
			}
			for i, nat := range rt.Nats {
				out[relPath(rt.SelfLink)+"/"+nat.Name] = ports[i]
			}
		}
		return nil
	})
	return out, err
}

// natPortRules renders the gateway's per-workload rules: filter rules
// dropping workloads without ports, nat rules translating each other
// workload onto its block, and commands capping a workload's connections
// at its block under endpoint-independent mapping.
func natPortRules(g *gateway, nats map[string]natPorts) (filter, nat, limits string) {
	var f, n, l strings.Builder
	for _, k := range slices.Sorted(maps.Keys(nats)) {
		a := nats[k]
		grow := natMaxPortsPerVM(a.nat)
		for _, vm := range a.vms {
			if vm.port == nil {
				fmt.Fprintf(&f, "-A GCPEMU-FWD -s %s/32 -o $EXT -j GCPEMU-DROP\n", vm.ip)
				continue
			}
			hi := min(natLastPort, vm.port.lo+grow-1)
			for _, proto := range []string{"tcp", "udp"} {
				fmt.Fprintf(&n, "-A GCPEMU-POST -s %s/32 -o $EXT -p %s -j SNAT --to-source %s:%d-%d\n", vm.ip, proto, g.extIP, vm.port.lo, hi)
			}
			if a.nat.EnableEndpointIndependentMapping {
				// Best effort, like NFLOG: a kernel without connlimit
				// still translates onto the block.
				fmt.Fprintf(&l, "iptables -I GCPEMU-FWD 3 -s %s/32 -o \"$EXT\" -m conntrack --ctstate NEW -m connlimit --connlimit-above %d --connlimit-mask 32 -j GCPEMU-DROP 2>/dev/null || echo 'connlimit unavailable: endpoint-independent mapping is not capped' >&2\n", vm.ip, a.perVM)
			}
		}
	}
	return f.String(), n.String(), l.String()
}
