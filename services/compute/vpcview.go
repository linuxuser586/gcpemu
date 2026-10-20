package compute

import (
	"cmp"
	"net/http"
	"slices"
	"strconv"

	computev1 "google.golang.org/api/compute/v1"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/runtime"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// The emulator's own view of a VPC network's VPC container networks, which
// no GCP API exposes (#322):
//
//	GET /_emu/v1/vpc/containerNetworks?project=P[&network=N]
//
// It lists the realised networks only (a subnetwork gets one when a
// Managed container first uses it) and never realises one itself.

// containerNetworkJSON is a VPC container network in the admin API.
type containerNetworkJSON struct {
	Network string `json:"network"`
	// Subnetwork is the subnetwork it backs; Allocation the private
	// services access range, for the other kind.
	Subnetwork    string `json:"subnetwork,omitempty"`
	Allocation    string `json:"allocation,omitempty"`
	Region        string `json:"region,omitempty"`
	Bridge        string `json:"bridge"`
	DeclaredRange string `json:"declaredRange"`
	// FallbackRange is empty when the network is on the Declared range.
	FallbackRange string `json:"fallbackRange"`
	Translated    bool   `json:"translated"`
	// HostAddress is empty when it is not known yet, or the host has none
	// (a Containerized Instance).
	HostAddress string          `json:"hostAddress"`
	Containers  []containerJSON `json:"containers"`
}

// containerJSON is a Managed container attached to a VPC container network.
type containerJSON struct {
	Name    string `json:"name"`
	Service string `json:"service"`
	Role    string `json:"role"`
	// DeclaredAddress is the address the emulator reports for it, as GCP
	// would; Address the one it holds on the container network. They
	// differ on a Translated network only.
	DeclaredAddress string `json:"declaredAddress"`
	Address         string `json:"address"`
}

// ipOwner is the owner that ct's address on a VPC container network is
// allocated to (AllocateIP), by the conventions of the Services that
// allocate them; "" when ct has no allocation.
func ipOwner(ct runtime.Container) string {
	l := ct.Labels
	switch svc, res, role := l[runtime.LabelService], l[runtime.LabelResource], l[runtime.LabelRole]; {
	case svc == "sql":
		return "sql:" + res
	case svc == "gke" && role == "server":
		return res + "/control-plane"
	case svc == "gke" && role == "node" && l["gcpemu.gke.node"] != "":
		return res + "/" + l["gcpemu.gke.node"]
	case svc == "lb" && role == "edge":
		return "lb-edge"
	case svc == "core" && res == "vmrelay":
		return hostOwner
	}
	return ""
}

// containerNetwork is st in the admin API, with those of cts attached to
// it; owners are its IP allocations. hostAttached is the Host address last
// served for it, and direct says whether the host routes to it (no VM
// relay, and not a Containerized Instance), where the Host address is its
// gateway.
func containerNetwork(st *vpcNet, owners []ownedIP, cts []runtime.Container, hostAttached string, direct bool) containerNetworkJSON {
	out := containerNetworkJSON{
		Network: st.Network, Subnetwork: st.Subnetwork, Region: st.Region, Bridge: st.Name,
		DeclaredRange: st.Range, Translated: st.translated(), Containers: []containerJSON{},
	}
	if st.Fallback {
		out.FallbackRange = st.Subnet
	}
	declared := map[string]string{}
	for _, o := range owners {
		declared[o.owner] = o.ip
	}
	relay := ""
	for _, ct := range cts {
		ip := ct.IPs[st.Name]
		if ip == "" {
			continue
		}
		c := containerJSON{
			Name: ct.Name, Service: ct.Labels[runtime.LabelService], Role: ct.Labels[runtime.LabelRole],
			Address: ip, DeclaredAddress: ip,
		}
		owner := ipOwner(ct)
		if st.translated() {
			c.DeclaredAddress = declared[owner]
			if c.Service == "compute" && c.Role == natRole {
				// The NAT egress proxy is aliased at the reserved tail.
				dc, _ := parseCIDR(st.Range, false)
				c.DeclaredAddress = ipString(dc.reservedTail())
			}
		}
		if owner == hostOwner {
			relay = ip
		}
		out.Containers = append(out.Containers, c)
	}
	slices.SortFunc(out.Containers, func(a, b containerJSON) int { return cmp.Compare(a.Name, b.Name) })
	if st.Subnetwork != "" {
		switch {
		case hostAttached != "":
			out.HostAddress = hostAttached
		case relay != "":
			out.HostAddress = relay
		case direct:
			out.HostAddress = st.Gateway
		}
	}
	return out
}

// serveContainerNetworks is GET /_emu/v1/vpc/containerNetworks.
func (s *Service) serveContainerNetworks(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	project := s.projectIDOf(q.Get("project"))
	if project == "" {
		adminError(w, apierr.InvalidArgument("A project is required."))
		return
	}
	np := ""
	if n := q.Get("network"); n != "" {
		p, err := globalRef(project, "networks", n)
		if err != nil {
			adminError(w, apierr.InvalidArgument("Invalid network %q.", n))
			return
		}
		np = p
	}
	var states []*vpcNet
	owners := map[string][]ownedIP{}
	allocs := map[string]string{}
	hosts := map[string]string{}
	_ = s.env.Store.View(func(tx store.Tx) error {
		for _, st := range list[vpcNet](tx, nsVPCNets, "") {
			if p, _, _ := pathParts(st.Network); p != project || (np != "" && st.Network != np) {
				continue
			}
			states = append(states, st)
			owners[st.Name] = ownersOf(tx, st.Name)
			if st.Subnetwork == "" {
				allocs[st.Name] = allocationOf(tx, st)
			}
		}
		for _, a := range list[hostAttachment](tx, nsHostAttach, "") {
			hosts[a.Subnetwork] = a.Address
		}
		return nil
	})
	out := []containerNetworkJSON{}
	if len(states) > 0 {
		cts, direct := s.vpcContainers(r)
		for _, st := range states {
			c := containerNetwork(st, owners[st.Name], cts, hosts[st.Subnetwork], direct)
			c.Allocation = allocs[st.Name]
			out = append(out, c)
		}
	}
	slices.SortFunc(out, func(a, b containerNetworkJSON) int {
		return cmp.Or(cmp.Compare(a.Network, b.Network), cmp.Compare(a.Subnetwork, b.Subnetwork), cmp.Compare(a.Allocation, b.Allocation))
	})
	adminJSON(w, http.StatusOK, map[string]any{"containerNetworks": out})
}

// vpcContainers lists the Instance's containers, and whether the host
// routes to its VPC container networks. It never connects the runtime
// (which reclaims orphans): until something has, there are none.
func (s *Service) vpcContainers(r *http.Request) ([]runtime.Container, bool) {
	c, ok := s.env.Containers.(emu.ConnectedRuntime)
	if !ok {
		return nil, false
	}
	rt := c.Connected()
	if rt == nil {
		return nil, false
	}
	ctx := r.Context()
	cts, err := rt.ListContainers(ctx, map[string]string{runtime.LabelInstance: rt.InstanceID})
	if err != nil {
		s.env.Log.Warn("compute: listing the containers on VPC container networks", "err", err)
	}
	direct := false
	if plane, err := s.env.Containers.Netplane(ctx); err == nil && rt.Self() == "" {
		d, ok := plane.RelayDecision()
		direct = ok && !d.On
	}
	return cts, direct
}

// allocationOf is the private services access range that the PSA network
// st realises: the connection's reserved range with st's range, else its
// first, as PrivateServicesNetwork picks.
func allocationOf(tx store.Tx, st *vpcNet) string {
	c, ok := get[psaConnection](tx, nsPSA, st.Network)
	if !ok || len(c.ReservedPeeringRanges) == 0 {
		return ""
	}
	p, _, _ := pathParts(st.Network)
	path := func(name string) string { return "projects/" + p + "/global/addresses/" + name }
	for _, name := range c.ReservedPeeringRanges {
		if a, ok := get[computev1.Address](tx, nsGlobalAddresses, path(name)); ok && a.Address+"/"+strconv.Itoa(int(a.PrefixLength)) == st.Range {
			return path(name)
		}
	}
	return path(c.ReservedPeeringRanges[0])
}
