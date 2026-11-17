//go:build !linux

package gke

// sourceRelay records that only a Linux node connects to pods from a
// source (ADR 0088); nodes are Linux, so this is for builds elsewhere.
func sourceRelay(r *clusterIPRelay) {
	r.unpreserved = "the node is not Linux"
}
