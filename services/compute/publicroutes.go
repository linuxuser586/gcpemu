package compute

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	computev1 "google.golang.org/api/compute/v1"

	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// Compute's Public routes (ADR 0059): the External address pools and the
// reserved external addresses outside them never leave the emulated
// network. Every PublicRouter, compute's NAT egress proxies and GKE's
// nodes with external IPs, holds them unreachable; the load balancer's
// routes for forwarding rule addresses are more specific and win.

// unreachableRoutes returns compute's Public routes: the pools, and a /32
// for each reserved external address outside them, which only an explicit
// address creates (a Seed file).
func (s *Service) unreachableRoutes() emu.PublicRoutes {
	r := emu.PublicRoutes{Dests: []string{regionalPool.String(), globalPool.String()}}
	var outside []string
	_ = s.env.Store.View(func(tx store.Tx) error {
		for _, ns := range []string{nsAddresses, nsGlobalAddresses} {
			for _, a := range list[computev1.Address](tx, ns, "projects/") {
				ip, ok := parseIP(a.Address)
				if a.AddressType == "INTERNAL" || !ok || regionalPool.contains(ip) || globalPool.contains(ip) {
					continue
				}
				outside = append(outside, ipString(ip))
			}
		}
		return nil
	})
	slices.Sort(outside)
	r.Dests = append(r.Dests, slices.Compact(outside)...)
	return r
}

// routeUnreachable sends compute's Public routes to the PublicRouters,
// compute's own and GKE's, when they changed since the routers last
// accepted them. It is called at start, after a reset or a State
// snapshot restore, and whenever an address is reserved or deleted.
func (s *Service) routeUnreachable(ctx context.Context) {
	s.unreachMu.Lock()
	defer s.unreachMu.Unlock()
	r := s.unreachableRoutes()
	want := strings.Join(r.Dests, " ")
	if want == s.unreachSent {
		return
	}
	routers := []emu.PublicRouter{s}
	if svc, ok := s.env.Lookup("gke"); ok {
		if pr, ok := svc.(emu.PublicRouter); ok {
			routers = append(routers, pr)
		}
	}
	var errs []error
	for _, pr := range routers {
		if err := pr.RoutePublic(ctx, emu.PublicRoutesFromCompute, r); err != nil {
			errs = append(errs, err)
		}
	}
	if err := errors.Join(errs...); err != nil {
		// A router that failed may hold neither the old routes nor the
		// new: the next call sends them again.
		s.env.Log.Warn("compute: routing the External address pools unreachable", "err", err)
		want = ""
	}
	s.unreachSent = want
}

// RoutePublic implements emu.PublicRouter: every NAT egress proxy holds
// the Public routes of each sender, the load balancer's via the Edge
// relay so that private workloads reach its forwarding rules through
// Cloud NAT as on GCP, with the NAT's address as the client (#187).
func (s *Service) RoutePublic(ctx context.Context, from string, r emu.PublicRoutes) error {
	s.pub.Set(from, r)
	s.egMu.Lock()
	gws := make([]*egressProxy, 0, len(s.egress))
	for _, g := range s.egress {
		gws = append(gws, g)
	}
	s.egMu.Unlock()
	var errs []error
	for _, g := range gws {
		if err := g.sync(ctx); err != nil {
			errs = append(errs, fmt.Errorf("NAT egress proxy of %s: %w", g.network, err))
		}
	}
	return errors.Join(errs...)
}
