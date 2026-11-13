package gke

import (
	"context"
	"fmt"

	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// The memory a Runtime machine needs for an Instance's GKE clusters, as the
// README's "GKE sizing" section states it: C × 768 MiB + N × 256 MiB + 2 GiB
// for C clusters with N nodes in all, before the pods (#1069, #1070). Keep
// them equal to that section.
const (
	// clusterBytes covers a Cluster server and the system pods only its
	// first node runs.
	clusterBytes = 768 << 20
	// nodeBytes covers a node and what it adds to its Cluster server.
	nodeBytes = 256 << 20
	// headroomBytes covers the Runtime machine's own system, the runtime
	// and the Instance's other Managed containers.
	headroomBytes = 2 << 30
)

// overcommit is the "gke.overcommit" report of GET /_emu/v1/info (#1070).
type overcommit struct {
	// EstimateBytes is the memory the Instance's GKE clusters need by the
	// README's formula.
	EstimateBytes int64 `json:"estimateBytes"`
	// MachineBytes is the memory the runtime reports for its own machine.
	MachineBytes int64 `json:"machineBytes"`
	// Nodes counts the Instance's GKE nodes, in every cluster.
	Nodes int `json:"nodes"`
	// Overcommitted reports EstimateBytes > MachineBytes.
	Overcommitted bool `json:"overcommitted"`
}

// estimateBytes is the README formula for clusters clusters with nodes
// nodes in all.
func estimateBytes(clusters, nodes int) int64 {
	return int64(clusters)*clusterBytes + int64(nodes)*nodeBytes + headroomBytes
}

// reestimate recomputes the Instance's GKE memory estimate from the stored
// clusters and logs one warning when it goes from within the Runtime
// machine's memory to beyond it. It is called after every change to the
// Instance's GKE node count, and holds ocMu throughout, so concurrent
// callers each see the latest stored count and a crossing is logged once.
// GKE overcommit warns and never refuses: nodes start exactly as they
// would without it (ADR 0031). It must be called outside a store
// transaction.
func (s *Service) reestimate(ctx context.Context) {
	// Read before ocMu: the runtime's own lock is held while it connects,
	// and /_emu/v1/info must not wait on that.
	machine := s.machineBytes(ctx)
	s.ocMu.Lock()
	defer s.ocMu.Unlock()
	clusters, nodes := 0, 0
	_ = s.env.Store.View(func(tx store.Tx) error {
		for _, rec := range listClusters(tx, "") {
			clusters++
			nodes += len(rec.Int.Nodes)
		}
		return nil
	})
	if machine <= 0 {
		s.oc = nil
		return
	}
	was := s.oc != nil && s.oc.Overcommitted
	r := &overcommit{EstimateBytes: estimateBytes(clusters, nodes), MachineBytes: machine, Nodes: nodes}
	r.Overcommitted = r.EstimateBytes > r.MachineBytes
	s.oc = r
	if r.Overcommitted && !was {
		s.env.Log.Warn("gke: the Instance's GKE nodes overcommit the Runtime machine's memory; nodes are not refused, but containers may be OOM-killed (see README \"GKE sizing\")",
			"estimate", inGiB(r.EstimateBytes), "machine", inGiB(r.MachineBytes), "nodes", nodes, "clusters", clusters)
	}
}

// machineBytes is the memory the container runtime reports for its own
// machine, or 0 if unknown. It never connects the runtime: a runtime not
// yet connected is unknown, and the bring-up that connects it estimates
// again.
func (s *Service) machineBytes(ctx context.Context) int64 {
	if s.env.Containers == nil {
		return 0
	}
	if cr, ok := s.env.Containers.(emu.ConnectedRuntime); ok {
		if m := cr.Connected(); m != nil {
			return m.Info.MemTotal
		}
		return 0
	}
	m, err := s.env.Containers.Runtime(ctx)
	if err != nil || m == nil {
		return 0
	}
	return m.Info.MemTotal
}

// overcommitReport is the last estimate for /_emu/v1/info, or nil.
func (s *Service) overcommitReport() *overcommit {
	s.ocMu.Lock()
	defer s.ocMu.Unlock()
	if s.oc == nil {
		return nil
	}
	r := *s.oc
	return &r
}

// inGiB formats n bytes in GiB for the log.
func inGiB(n int64) string { return fmt.Sprintf("%.2f GiB", float64(n)/(1<<30)) }
