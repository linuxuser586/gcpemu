package gke

import (
	"context"
	"sort"
)

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
			ips[n.Name] = n.IP
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
