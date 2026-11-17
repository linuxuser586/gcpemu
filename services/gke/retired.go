package gke

import (
	"slices"
	"strings"

	"cloud.google.com/go/container/apiv1/containerpb"

	"github.com/linuxuser586/gcpemu/internal/store"
)

// Retired versions (ADR 0085). A stored GKE version that the version table
// no longer holds, while it holds a newer one, is Retired: a cluster on it
// is upgraded off it when it is brought up after an Instance start or a
// State snapshot restore, since its image is gone.

// retiredTarget returns the version a stored version v moves to when it is
// Retired in table (newest first): among the table's versions newer than
// v, the newest patch of the oldest minor. That is v's own minor when a
// newer patch of it is left, and may cross more than one minor. ok is
// false when v is not Retired: it is in the table, nothing in the table is
// newer (downgrades are not supported), or it is not a full version.
func retiredTarget(table []k8sVersion, v string) (string, bool) {
	if len(versionNums(v)) < 3 {
		return "", false
	}
	target := ""
	for _, e := range table {
		if e.GKE == v {
			return "", false
		}
		if compareVersions(e.GKE, v) <= 0 {
			continue
		}
		// Newest first: the first entry of each minor is its newest patch,
		// and a later one of an older minor replaces it.
		if target == "" || (minorOf(e.GKE) != minorOf(target) && compareVersions(e.GKE, target) < 0) {
			target = e.GKE
		}
	}
	return target, target != ""
}

// versionMove is a control plane's or node pool's upgrade off a Retired
// version. Pool names the node pool a pool's move is of.
type versionMove struct{ Pool, From, To string }

func (m versionMove) message() string {
	return "Automatic upgrade from retired version " + m.From + " to " + m.To
}

// moveOffRetired upgrades c's control plane and node pools off Retired
// versions in table, in place, and returns the moves. A pool moves by the
// same rule as the control plane, capped at the control plane's version
// after its own move. A pool's autoUpgrade and the cluster's maintenance
// policy do not stop either move. A control plane that is neither in the
// table nor Retired (newer than the table) is left, with its pools, to
// fail its bring-up.
func moveOffRetired(table []k8sVersion, c *containerpb.Cluster) (master *versionMove, pools []versionMove) {
	if to, ok := retiredTarget(table, c.CurrentMasterVersion); ok {
		master = &versionMove{From: c.CurrentMasterVersion, To: to}
		c.CurrentMasterVersion = to
	}
	if !slices.ContainsFunc(table, func(e k8sVersion) bool { return e.GKE == c.CurrentMasterVersion }) {
		return master, nil
	}
	for _, np := range c.NodePools {
		to, ok := retiredTarget(table, np.Version)
		if !ok {
			continue
		}
		if compareVersions(to, c.CurrentMasterVersion) > 0 {
			to = c.CurrentMasterVersion
		}
		pools = append(pools, versionMove{Pool: np.Name, From: np.Version, To: to})
		np.Version = to
	}
	if master != nil || len(pools) > 0 {
		// With no pools (Autopilot), the nodes run the control plane's
		// version.
		c.CurrentNodeVersion = lowestNodeVersion(c)
	}
	return master, pools
}

// upgradeRetired moves a stored cluster about to be brought up off Retired
// versions (moveOffRetired) and records each part of the move as an
// Operation that is already DONE: UPGRADE_MASTER for the control plane and
// UPGRADE_NODES for its pools. They take no Operation lock and ignore
// Operation latency, as the move is only a change to the Store; the
// bring-up that follows starts the new images.
func (s *Service) upgradeRetired(key string) {
	table := s.versionTable
	if table == nil {
		table = versions
	}
	var master *versionMove
	var pools []versionMove
	err := s.env.Store.Update(func(tx store.Tx) error {
		rec, err := getCluster(tx, key)
		if err != nil {
			return err
		}
		c := rec.cluster()
		master, pools = moveOffRetired(table, c)
		if master == nil && len(pools) == 0 {
			return nil
		}
		c.Etag = s.env.IDs.Hex(8)
		rec.setCluster(c)
		if err := putCluster(tx, rec); err != nil {
			return err
		}
		r := rec.ref()
		cluster := selfLink(r.Project, r.Location, "clusters/"+r.Cluster)
		now := s.now()
		if master != nil {
			if err := putOpTx(tx, r.Project, s.doneOp(r, containerpb.Operation_UPGRADE_MASTER, cluster, master.message(), now)); err != nil {
				return err
			}
		}
		if len(pools) > 0 {
			target := cluster
			if len(pools) == 1 {
				target += "/nodePools/" + pools[0].Pool
			}
			var msgs []string
			for _, m := range pools {
				if msg := m.message(); !slices.Contains(msgs, msg) {
					msgs = append(msgs, msg)
				}
			}
			if err := putOpTx(tx, r.Project, s.doneOp(r, containerpb.Operation_UPGRADE_NODES, target, strings.Join(msgs, "; "), now)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		s.env.Log.Warn("gke: upgrading cluster off retired versions failed", "cluster", key, "err", err)
		return
	}
	if master != nil {
		s.env.Log.Warn("gke: control plane upgraded off retired version", "cluster", key, "from", master.From, "to", master.To)
	}
	for _, m := range pools {
		s.env.Log.Warn("gke: node pool upgraded off retired version", "cluster", key, "nodePool", m.Pool, "from", m.From, "to", m.To)
	}
}

// doneOp is an Operation of type typ on target that started and ended at
// now, with statusMessage msg.
func (s *Service) doneOp(r ref, typ containerpb.Operation_Type, target, msg, now string) *containerpb.Operation {
	op := newOp(r, s.newOpName(), typ, containerpb.Operation_DONE, target, now)
	op.EndTime = now
	op.StatusMessage = msg
	return op
}
