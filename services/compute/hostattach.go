package compute

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/netplane"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// The inward half of the Host attachment (ADR 0014): VPC workloads reach
// Offered ports at the Host address of a subnetwork's VPC container
// network. The netplane serves them; compute keeps the Host address and
// the Offered ports of each subnetwork, offers them again at start, and
// serves the admin API:
//
//	GET    /_emu/v1/host/address?subnetwork=S      the Host address
//	GET    /_emu/v1/host/offers[?subnetwork=S]     the Offered ports
//	POST   /_emu/v1/host/offers                    {"subnetwork": S, "ports": ["8080", "53/udp"]}
//	DELETE /_emu/v1/host/offers?subnetwork=S&port=8080&port=53/udp
//
// On a VM runtime the Host address is the VM relay's address on the
// network, reserved in the network's IP allocations and pinned for the
// relay (RelayAddr), so it outlives the relay and the Instance. Where the
// host routes to the network, it is the network's gateway.

const (
	// nsHostAttach holds a hostAttachment per subnetwork path.
	nsHostAttach = "compute/hostattach"
	// hostOwner owns the Host address in the network's IP allocations.
	hostOwner = "gcpemu-host-attachment"
)

// hostAttachment is the persisted inward Host attachment of a subnetwork.
type hostAttachment struct {
	Subnetwork string `json:"subnetwork"`
	// Network is the container network's name.
	Network string `json:"network"`
	// Address is the Host address last served.
	Address string `json:"address"`
	// Relayed: the VM relay holds Address, which is then pinned.
	Relayed bool `json:"relayed,omitempty"`
	// Offers are the Offered ports, as "8080/tcp", sorted.
	Offers []string `json:"offers,omitempty"`
}

// hostPort is an Offered port.
type hostPort struct {
	Port int
	UDP  bool
}

func (p hostPort) proto() string {
	if p.UDP {
		return "udp"
	}
	return "tcp"
}

func (p hostPort) String() string { return strconv.Itoa(p.Port) + "/" + p.proto() }

// parseHostPort parses "8080", "8080/tcp" or "53/udp".
func parseHostPort(s string) (hostPort, error) {
	n, proto, _ := strings.Cut(strings.TrimSpace(s), "/")
	port, err := strconv.Atoi(n)
	if err != nil || port < 1 || port > 65535 {
		return hostPort{}, apierr.InvalidArgument("Invalid port %q: want a port number from 1 to 65535, with /udp for UDP.", s)
	}
	switch strings.ToLower(proto) {
	case "", "tcp":
		return hostPort{Port: port}, nil
	case "udp":
		return hostPort{Port: port, UDP: true}, nil
	}
	return hostPort{}, apierr.InvalidArgument("Invalid protocol in %q: want tcp or udp.", s)
}

// hostOffersJSON is a subnetwork's entry in the admin API.
type hostOffersJSON struct {
	Subnetwork  string      `json:"subnetwork"`
	HostAddress string      `json:"hostAddress"`
	Offers      []offerJSON `json:"offers"`
}

type offerJSON struct {
	Port     int    `json:"port"`
	Protocol string `json:"protocol"`
}

func (a *hostAttachment) json() hostOffersJSON {
	out := hostOffersJSON{Subnetwork: a.Subnetwork, HostAddress: a.Address, Offers: []offerJSON{}}
	for _, o := range a.Offers {
		if p, err := parseHostPort(o); err == nil {
			out.Offers = append(out.Offers, offerJSON{Port: p.Port, Protocol: p.proto()})
		}
	}
	return out
}

func (s *Service) hostRoutes(r emu.Router) {
	r.Handle("GET /_emu/v1/host/address", http.HandlerFunc(s.serveHostAddress))
	r.Handle("GET /_emu/v1/host/offers", http.HandlerFunc(s.serveHostOffers))
	r.Handle("POST /_emu/v1/host/offers", http.HandlerFunc(s.serveOffer))
	r.Handle("DELETE /_emu/v1/host/offers", http.HandlerFunc(s.serveWithdraw))
}

// serveHostAddress is GET /_emu/v1/host/address.
func (s *Service) serveHostAddress(w http.ResponseWriter, r *http.Request) {
	s.hostMu.Lock()
	defer s.hostMu.Unlock()
	a, _, err := s.currentAttachment(r.Context(), r.URL.Query().Get("subnetwork"))
	if err != nil {
		adminError(w, err)
		return
	}
	adminJSON(w, http.StatusOK, a.json())
}

// serveHostOffers is GET /_emu/v1/host/offers.
func (s *Service) serveHostOffers(w http.ResponseWriter, r *http.Request) {
	want := relPath(r.URL.Query().Get("subnetwork"))
	out := []hostOffersJSON{}
	for _, a := range s.hostAttachments() {
		if len(a.Offers) > 0 && (want == "" || a.Subnetwork == want) {
			out = append(out, a.json())
		}
	}
	adminJSON(w, http.StatusOK, map[string]any{"subnetworks": out})
}

// serveOffer is POST /_emu/v1/host/offers.
func (s *Service) serveOffer(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Subnetwork string   `json:"subnetwork"`
		Ports      []string `json:"ports"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		adminError(w, apierr.InvalidArgument("Invalid request body: %v", err))
		return
	}
	a, err := s.offer(r.Context(), req.Subnetwork, req.Ports)
	if err != nil {
		adminError(w, err)
		return
	}
	adminJSON(w, http.StatusOK, a.json())
}

// serveWithdraw is DELETE /_emu/v1/host/offers.
func (s *Service) serveWithdraw(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	a, err := s.withdraw(r.Context(), q.Get("subnetwork"), q["port"])
	if err != nil {
		adminError(w, err)
		return
	}
	adminJSON(w, http.StatusOK, a.json())
}

func adminJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func adminError(w http.ResponseWriter, err error) {
	e := apierr.From(err)
	adminJSON(w, e.HTTP(), map[string]string{"error": e.Message})
}

func parseHostPorts(ports []string) ([]hostPort, error) {
	if len(ports) == 0 {
		return nil, apierr.InvalidArgument("No ports given.")
	}
	out := make([]hostPort, 0, len(ports))
	for _, p := range ports {
		hp, err := parseHostPort(p)
		if err != nil {
			return nil, err
		}
		out = append(out, hp)
	}
	return out, nil
}

// offer offers ports on subnetwork: all of them, or none when one fails.
func (s *Service) offer(ctx context.Context, subnetwork string, ports []string) (*hostAttachment, error) {
	hps, err := parseHostPorts(ports)
	if err != nil {
		return nil, err
	}
	s.hostMu.Lock()
	defer s.hostMu.Unlock()
	a, plane, err := s.currentAttachment(ctx, subnetwork)
	if err != nil {
		return nil, err
	}
	var added []hostPort
	for _, p := range hps {
		if slices.Contains(a.Offers, p.String()) || slices.Contains(added, p) {
			continue
		}
		if err := plane.Offer(ctx, a.Address, p.Port, p.UDP); err != nil {
			for _, d := range added {
				_ = plane.Withdraw(ctx, a.Address, d.Port, d.UDP)
			}
			return nil, apierr.FailedPrecondition("%v", err)
		}
		added = append(added, p)
	}
	for _, p := range added {
		a.Offers = append(a.Offers, p.String())
	}
	slices.Sort(a.Offers)
	if err := s.putHostAttachment(a); err != nil {
		return nil, err
	}
	if len(added) > 0 {
		s.env.Log.Info("compute: ports offered at the Host address", "subnetwork", a.Subnetwork, "address", a.Address, "ports", added)
	}
	return a, nil
}

// withdraw withdraws ports on subnetwork; ports that are not offered are
// ignored.
func (s *Service) withdraw(ctx context.Context, subnetwork string, ports []string) (*hostAttachment, error) {
	hps, err := parseHostPorts(ports)
	if err != nil {
		return nil, err
	}
	s.hostMu.Lock()
	defer s.hostMu.Unlock()
	a, plane, err := s.currentAttachment(ctx, subnetwork)
	if err != nil {
		return nil, err
	}
	for _, p := range hps {
		if i := slices.Index(a.Offers, p.String()); i >= 0 {
			if err := plane.Withdraw(ctx, a.Address, p.Port, p.UDP); err != nil {
				return nil, apierr.Internal("Withdrawing %s: %v", p, err)
			}
			a.Offers = slices.Delete(a.Offers, i, i+1)
		}
	}
	return a, s.putHostAttachment(a)
}

// currentAttachment realises subnetwork's container network and returns its
// attachment with the current Host address, which it records. Callers
// hold hostMu.
func (s *Service) currentAttachment(ctx context.Context, subnetwork string) (*hostAttachment, *netplane.Plane, error) {
	sp := relPath(subnetwork)
	if sp == "" {
		return nil, nil, apierr.InvalidArgument("A subnetwork is required (projects/P/regions/R/subnetworks/S).")
	}
	sn, err := s.SubnetNetwork(ctx, sp)
	if err != nil {
		return nil, nil, err
	}
	plane, err := s.env.Containers.Netplane(ctx)
	if err != nil {
		return nil, nil, apierr.FailedPrecondition("No container runtime is available: %v", err)
	}
	addr, err := plane.HostAddress(ctx, netplane.Net{Name: sn.Name, Subnet: sn.ContainerSubnet, Gateway: sn.Gateway})
	if errors.Is(err, netplane.ErrInContainer) {
		return nil, nil, apierr.FailedPrecondition("%v", err)
	} else if err != nil {
		return nil, nil, apierr.Internal("The Host address of %s: %v", sp, err)
	}
	relayed := plane.Relay() != nil
	var a *hostAttachment
	_ = s.env.Store.View(func(tx store.Tx) error { a, _ = get[hostAttachment](tx, nsHostAttach, sp); return nil })
	if a == nil {
		a = &hostAttachment{Subnetwork: sp}
	}
	if a.Address != "" && a.Relayed && relayed && a.Address != addr {
		s.env.Log.Warn("compute: the Host address moved; EndpointSlices that point at the old one need updating",
			"subnetwork", sp, "was", a.Address, "now", addr)
	}
	if a.Address != addr || a.Relayed != relayed || a.Network != sn.Name {
		a.Address, a.Relayed, a.Network = addr, relayed, sn.Name
		if err := s.reserveHostAddr(sn, a); err != nil {
			return nil, nil, err
		}
	}
	return a, plane, nil
}

// reserveHostAddr records a, with its Host address in the network's IP
// allocations when the relay holds it there, so that no workload is given
// it while the relay is down.
func (s *Service) reserveHostAddr(sn emu.SubnetNet, a *hostAttachment) error {
	s.vpcMu.Lock()
	defer s.vpcMu.Unlock()
	return s.env.Store.Update(func(tx store.Tx) error {
		ip := ""
		if a.Relayed {
			ip = a.Address
		}
		if err := reserveIP(tx, sn, hostOwner, ip); err != nil {
			return err
		}
		return store.PutJSON(tx, nsHostAttach, a.Subnetwork, a)
	})
}

func (s *Service) putHostAttachment(a *hostAttachment) error {
	return s.env.Store.Update(func(tx store.Tx) error { return store.PutJSON(tx, nsHostAttach, a.Subnetwork, a) })
}

func (s *Service) hostAttachments() []*hostAttachment {
	var out []*hostAttachment
	_ = s.env.Store.View(func(tx store.Tx) error { out = list[hostAttachment](tx, nsHostAttach, ""); return nil })
	return out
}

// RelayAddr implements emu.RelayAddresser: the VM relay takes the Host
// address again on the network it was recorded on.
func (s *Service) RelayAddr(network string) string {
	for _, a := range s.hostAttachments() {
		if a.Relayed && a.Network == network {
			return a.Address
		}
	}
	return ""
}

// ReservedAddrs implements emu.AddressReserver: no container started
// without a fixed address takes a Host address the relay holds.
func (s *Service) ReservedAddrs() []string {
	var out []string
	for _, a := range s.hostAttachments() {
		if a.Relayed {
			out = append(out, a.Address)
		}
	}
	return out
}

// restoreOffers offers the stored Offered ports again, at start.
func (s *Service) restoreOffers(ctx context.Context) {
	for _, stored := range s.hostAttachments() {
		if len(stored.Offers) == 0 {
			continue
		}
		s.hostMu.Lock()
		a, plane, err := s.currentAttachment(ctx, stored.Subnetwork)
		if err == nil {
			for _, o := range a.Offers {
				p, _ := parseHostPort(o)
				if perr := plane.Offer(ctx, a.Address, p.Port, p.UDP); perr != nil {
					err = errors.Join(err, fmt.Errorf("%s: %w", o, perr))
				}
			}
		}
		s.hostMu.Unlock()
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			s.env.Log.Warn("compute: Offered ports could not be served again", "subnetwork", stored.Subnetwork, "err", err)
		} else {
			s.env.Log.Info("compute: Offered ports served again", "subnetwork", a.Subnetwork, "address", a.Address, "ports", a.Offers)
		}
	}
}

// Snapshot implements emu.Snapshotter: the Host attachment is all in the
// Store.
func (s *Service) Snapshot(ctx context.Context, dir string) error { return nil }

// Restore implements emu.Snapshotter: Reset withdrew the Offered ports,
// and those of the snapshot are served again.
func (s *Service) Restore(ctx context.Context, dir string) error {
	s.restoreOffers(ctx)
	return nil
}

// withdrawAll stops serving the Offered ports of attachments, which are
// being forgotten (Reset, a deleted subnetwork). It never initialises
// the runtime for attachments without offers.
func (s *Service) withdrawAll(ctx context.Context, as []*hostAttachment) {
	for _, a := range as {
		if len(a.Offers) == 0 || s.env.Containers == nil {
			continue
		}
		plane, err := s.env.Containers.Netplane(ctx)
		if err != nil {
			return
		}
		for _, o := range a.Offers {
			p, _ := parseHostPort(o)
			if err := plane.Withdraw(ctx, a.Address, p.Port, p.UDP); err != nil {
				s.env.Log.Warn("compute: withdrawing an Offered port", "subnetwork", a.Subnetwork, "port", o, "err", err)
			}
		}
	}
}
