package gke

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/linuxuser586/gcpemu/internal/emu"
)

var _ emu.PublicRouter = (*Service)(nil)

// PodRoute tells a peer data plane (the M3 load balancer) how to reach a
// node's pods: pod addresses in CIDR are routed via the node address Via
// on the VPC subnetwork Subnetwork ("projects/P/regions/R/subnetworks/S",
// realised by compute's emu.VPC). NEG endpoints (FR-GKE-008) are pod IPs,
// so a proxy container attached to that subnetwork needs
// `ip route add CIDR via Via` for each route to reach them.
type PodRoute struct {
	Cluster    string // projects/P/locations/L/clusters/C
	Node       string
	CIDR       string
	Via        string
	Subnetwork string
}

// PodRoutes returns the pod routes of every running cluster.
func (s *Service) PodRoutes(ctx context.Context) []PodRoute {
	s.mu.Lock()
	keys := make([]string, 0, len(s.clusters))
	for k := range s.clusters {
		keys = append(keys, k)
	}
	s.mu.Unlock()
	sort.Strings(keys)
	var out []PodRoute
	for _, key := range keys {
		kc, err := s.kube(key)
		if err != nil {
			continue
		}
		rec, err := s.loadKey(key)
		if err != nil {
			continue
		}
		ips := map[string]string{}
		for _, n := range rec.Int.Nodes {
			ips[n.Name] = n.dialIP()
		}
		var nodes kubeList[kubeNode]
		if err := kc.get(ctx, "/api/v1/nodes", &nodes); err != nil {
			continue
		}
		name := ref{Project: rec.Int.Project, Location: rec.Int.Location, Cluster: rec.Int.Name}.clusterName()
		for _, n := range nodes.Items {
			if n.Spec.PodCIDR == "" || ips[n.Metadata.Name] == "" {
				continue
			}
			out = append(out, PodRoute{Cluster: name, Node: n.Metadata.Name, CIDR: n.Spec.PodCIDR, Via: ips[n.Metadata.Name], Subnetwork: rec.Int.Subnetwork})
		}
	}
	return out
}

// RoutePublic implements emu.PublicRouter: nodes with external IPs route
// the load balancer's forwarding rule addresses via the Edge relay, so
// pods reach load balancers by their public address with the node's
// address as the client, as on GCP (#187). Private nodes reach them
// through Cloud NAT instead (compute).
func (s *Service) RoutePublic(ctx context.Context, via string, ips []string) error {
	s.pubMu.Lock()
	s.pubVia, s.pubIPs = via, append([]string(nil), ips...)
	s.pubMu.Unlock()
	if s.env.Containers == nil {
		return nil
	}
	d, err := s.deps(ctx)
	if err != nil {
		return nil
	}
	s.mu.Lock()
	keys := make([]string, 0, len(s.clusters))
	for k := range s.clusters {
		keys = append(keys, k)
	}
	s.mu.Unlock()
	sort.Strings(keys)
	var errs []error
	for _, key := range keys {
		rec, err := s.loadKey(key)
		if err != nil {
			continue
		}
		c := rec.cluster()
		for _, n := range rec.Int.Nodes {
			if n.ExternalIP == "" {
				continue
			}
			// A node that is not running gets the routes when it starts.
			name := nodeContainer(d, c, n.Name)
			if ct, err := d.rt.InspectContainer(ctx, name); err != nil || !ct.Running {
				continue
			}
			if err := s.routePublic(ctx, d, name); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// routesPublic reports whether there are public routes to program.
func (s *Service) routesPublic() bool {
	s.pubMu.Lock()
	defer s.pubMu.Unlock()
	return s.pubVia != ""
}

// routePublic programs the current public routes into a node container.
func (s *Service) routePublic(ctx context.Context, d *deps, name string) error {
	s.pubMu.Lock()
	script := emu.PublicRoutesScript(s.pubVia, s.pubIPs)
	s.pubMu.Unlock()
	res, err := d.rt.Exec(ctx, name, []string{"sh", "-c", script}, nil)
	if err == nil && res.ExitCode != 0 {
		err = fmt.Errorf("exit %d: %s%s", res.ExitCode, res.Stdout, res.Stderr)
	}
	if err != nil {
		return fmt.Errorf("public routes in %s: %w", name, err)
	}
	return nil
}
