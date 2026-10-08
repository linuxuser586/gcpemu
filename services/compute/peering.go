package compute

import (
	"context"
	"net/http"

	computev1 "google.golang.org/api/compute/v1"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// VPC Network Peering (networks.addPeering, updatePeering, removePeering).
// Peerings are recorded: a peering is ACTIVE once the peer network has a
// peering back, as in GCP, but no routes are exchanged between the
// container networks (VPC peering is out of scope for v1, SRS 10.3).

// nsPeerings holds each network's peerings ([]*storedPeering) keyed by
// the network path.
const nsPeerings = "compute/networkPeerings"

// storedPeering is a peering's configuration; state is derived on read.
type storedPeering struct {
	Peering    *computev1.NetworkPeering `json:"peering"`
	CreateTime string                    `json:"createTime"`
}

// peeringsOf returns the user peerings of a network.
func peeringsOf(tx store.Tx, np string) []*storedPeering {
	var out []*storedPeering
	_ = store.GetJSON(tx, nsPeerings, np, &out)
	return out
}

// renderPeering fills a peering's output-only fields: it is ACTIVE while
// the peer network exists and peers back.
func renderPeering(tx store.Tx, np string, sp *storedPeering) *computev1.NetworkPeering {
	p := *sp.Peering
	p.State, p.StateDetails, p.PeerMtu = "INACTIVE", "["+sp.CreateTime+"]: Waiting for peer network to connect.", 0
	peer := relPath(p.Network)
	if pn, ok := get[computev1.Network](tx, nsNetworks, peer); ok {
		for _, back := range peeringsOf(tx, peer) {
			if relPath(back.Peering.Network) == np {
				p.State, p.StateDetails, p.PeerMtu = "ACTIVE", "["+later(sp.CreateTime, back.CreateTime)+"]: Connected.", pn.Mtu
			}
		}
	}
	p.ForceSendFields = []string{"ExportCustomRoutes", "ImportCustomRoutes", "ExportSubnetRoutesWithPublicIp", "ImportSubnetRoutesWithPublicIp"}
	return &p
}

func later(a, b string) string {
	if b > a {
		return b
	}
	return a
}

// addPeering implements networks.addPeering, in both the networkPeering
// form and the legacy name/peerNetwork form.
func (s *Service) addPeering(w http.ResponseWriter, r *http.Request) {
	path, cur, err := s.loadNetwork(r, "compute.networks.addPeering")
	if err != nil {
		apierr.Write(w, err)
		return
	}
	var req computev1.NetworksAddPeeringRequest
	if _, err := decode(r, &req); err != nil {
		apierr.Write(w, err)
		return
	}
	np := req.NetworkPeering
	if np == nil {
		np = &computev1.NetworkPeering{Name: req.Name, Network: req.PeerNetwork, ExchangeSubnetRoutes: true, ExportSubnetRoutesWithPublicIp: true}
	}
	if err := validName("networkPeering.name", np.Name); err != nil {
		apierr.Write(w, err)
		return
	}
	if np.Network == "" {
		apierr.Write(w, errRequired("networkPeering.network"))
		return
	}
	p, _, _ := pathParts(path)
	peer, err := globalRef(p, "networks", np.Network)
	if err != nil {
		apierr.Write(w, errInvalidField("networkPeering.network", np.Network, "The URL is malformed."))
		return
	}
	if peer == path {
		apierr.Write(w, errInvalidField("networkPeering.network", np.Network, "A network cannot be peered with itself."))
		return
	}
	np.Network = link(peer)
	np.AutoCreateRoutes, np.ExchangeSubnetRoutes = true, true
	if np.StackType == "" {
		np.StackType = "IPV4_ONLY"
	}
	if np.StackType != "IPV4_ONLY" && np.StackType != "IPV4_IPV6" {
		apierr.Write(w, errInvalidField("networkPeering.stackType", np.StackType, ""))
		return
	}
	if np.UpdateStrategy == "" {
		np.UpdateStrategy = "INDEPENDENT"
	}
	np.State, np.StateDetails, np.PeerMtu, np.ConnectionStatus = "", "", 0, nil
	check := func(tx store.Tx) error {
		if _, ok := get[computev1.Network](tx, nsNetworks, peer); !ok {
			return errNotFound(peer)
		}
		for _, o := range peeringsOf(tx, path) {
			if o.Peering.Name == np.Name {
				return apierr.InvalidArgument("There is already a peering %s on network %s.", np.Name, path).WithLegacy("invalid")
			}
			if relPath(o.Peering.Network) == peer {
				return apierr.InvalidArgument("There is already a peering %s to network %s.", o.Peering.Name, peer).WithLegacy("invalid")
			}
		}
		for _, back := range peeringsOf(tx, peer) {
			if relPath(back.Peering.Network) == path {
				return checkPeeringOverlap(tx, path, peer)
			}
		}
		return nil
	}
	if err := s.env.Store.View(check); err != nil {
		apierr.Write(w, err)
		return
	}
	op, err := s.startOp(r.Context(), opSpec{project: p, scope: "global", opType: "addPeering", target: path, targetID: cur.Id},
		func(ctx context.Context) error {
			return s.env.Store.Update(func(tx store.Tx) error {
				if err := check(tx); err != nil {
					return err
				}
				sp := &storedPeering{Peering: np, CreateTime: stamp(s.env.Clock.Now())}
				return store.PutJSON(tx, nsPeerings, path, append(peeringsOf(tx, path), sp))
			})
		})
	reply(w, op, err)
}

// checkPeeringOverlap fails when the two networks' subnetwork ranges
// overlap (GCP refuses to activate such a peering).
func checkPeeringOverlap(tx store.Tx, a, b string) error {
	ranges := func(np string) []*computev1.Subnetwork {
		p, _, _ := pathParts(np)
		var out []*computev1.Subnetwork
		for _, sn := range list[computev1.Subnetwork](tx, nsSubnets, "projects/"+p+"/regions/") {
			if relPath(sn.Network) == np {
				out = append(out, sn)
			}
		}
		return out
	}
	theirs := ranges(b)
	for _, x := range ranges(a) {
		cx, err := parseCIDR(x.IpCidrRange, false)
		if err != nil {
			continue
		}
		for _, y := range theirs {
			if cy, err := parseCIDR(y.IpCidrRange, false); err == nil && cx.overlaps(cy) {
				return apierr.InvalidArgument("An IP range in the peer network (%s) overlaps with an IP range in the local network (%s) allocated by resource (%s).",
					y.IpCidrRange, x.IpCidrRange, relPath(x.SelfLink)).WithLegacy("invalid")
			}
		}
	}
	return nil
}

// updatePeering implements networks.updatePeering: the route exchange
// settings and stack type of an existing peering change.
func (s *Service) updatePeering(w http.ResponseWriter, r *http.Request) {
	path, cur, err := s.loadNetwork(r, "compute.networks.updatePeering")
	if err != nil {
		apierr.Write(w, err)
		return
	}
	var req computev1.NetworksUpdatePeeringRequest
	if _, err := decode(r, &req); err != nil {
		apierr.Write(w, err)
		return
	}
	in := req.NetworkPeering
	if in == nil || in.Name == "" {
		apierr.Write(w, errRequired("networkPeering.name"))
		return
	}
	// update changes the peering in ps; put stores the result.
	update := func(tx store.Tx, put bool) error {
		ps := peeringsOf(tx, path)
		for _, sp := range ps {
			o := sp.Peering
			if o.Name != in.Name {
				continue
			}
			if in.Network != "" && lastSeg(in.Network) != lastSeg(o.Network) {
				return errInvalidField("networkPeering.network", in.Network, "The peer network of a peering cannot be changed.")
			}
			if !put {
				return nil
			}
			o.ExportCustomRoutes, o.ImportCustomRoutes = in.ExportCustomRoutes, in.ImportCustomRoutes
			o.ExportSubnetRoutesWithPublicIp, o.ImportSubnetRoutesWithPublicIp = in.ExportSubnetRoutesWithPublicIp, in.ImportSubnetRoutesWithPublicIp
			if in.StackType != "" {
				o.StackType = in.StackType
			}
			if in.UpdateStrategy != "" {
				o.UpdateStrategy = in.UpdateStrategy
			}
			return store.PutJSON(tx, nsPeerings, path, ps)
		}
		return errNotFound(path + "/peerings/" + in.Name)
	}
	if err := s.env.Store.View(func(tx store.Tx) error { return update(tx, false) }); err != nil {
		apierr.Write(w, err)
		return
	}
	p, _, _ := pathParts(path)
	op, err := s.startOp(r.Context(), opSpec{project: p, scope: "global", opType: "updatePeering", target: path, targetID: cur.Id},
		func(ctx context.Context) error {
			return s.env.Store.Update(func(tx store.Tx) error { return update(tx, true) })
		})
	reply(w, op, err)
}

// removeUserPeering removes a user peering; ok is false when the network
// has no peering of that name.
func (s *Service) removeUserPeering(w http.ResponseWriter, r *http.Request, path string, cur *computev1.Network, name string) bool {
	found := false
	_ = s.env.Store.View(func(tx store.Tx) error {
		for _, o := range peeringsOf(tx, path) {
			found = found || o.Peering.Name == name
		}
		return nil
	})
	if !found {
		return false
	}
	p, _, _ := pathParts(path)
	op, err := s.startOp(r.Context(), opSpec{project: p, scope: "global", opType: "removePeering", target: path, targetID: cur.Id},
		func(ctx context.Context) error {
			return s.env.Store.Update(func(tx store.Tx) error {
				ps := peeringsOf(tx, path)
				kept := ps[:0]
				for _, o := range ps {
					if o.Peering.Name != name {
						kept = append(kept, o)
					}
				}
				if len(kept) == len(ps) {
					return errNotFound(path + "/peerings/" + name)
				}
				if len(kept) == 0 {
					return tx.Delete(nsPeerings, path)
				}
				return store.PutJSON(tx, nsPeerings, path, kept)
			})
		})
	reply(w, op, err)
	return true
}
