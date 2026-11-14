# A cluster on a Retired version is upgraded off it when it is brought up

GKE clusters and node pools store only their version; the k3s image is looked up in the GKE version table each time a control plane or node is brought up. ADR 0084 therefore never removed a version from the table, since a cluster on a missing one went to ERROR ("unknown version") on the next Instance start or State snapshot restore, and the table grew by a patch per minor at each Product image refresh (#1166). GKE never strands a cluster this way: it upgrades clusters off a version at end of support.

We decided that when a cluster is brought up after an Instance start or a State snapshot restore, a control plane or node pool on a Retired version is upgraded off it before its containers start. A Retired version is any stored version the table no longer holds while holding a newer one; nothing records retirements separately, and `getServerConfig` lists only the table. The control plane moves to the newest patch of its minor, or, when the whole minor is retired, to the newest patch of the oldest minor left, even when that crosses more than one minor. Each node pool moves by the same rule, capped at the control plane's version after its move. The move ignores a pool's `management.autoUpgrade` and the cluster's `maintenancePolicy`, and it is reported as `UPGRADE_MASTER` and `UPGRADE_NODES` Operations that are already DONE, with a status message naming both versions. A stored version newer than every version in the table, as when an older build restores a newer build's data, is not retired: the cluster goes to ERROR as before, since downgrades are not supported.

This supersedes ADR 0084's rule that a GKE version is never removed from the table.

## Considered options

- **Keep every version forever** (ADR 0084). Nothing to build, but the table and the images a refresh must keep grow without end, and `getServerConfig` offers patches GKE has withdrawn.
- **A list of retired versions, each mapped to its replacement.** It states each move explicitly, but every refresh must keep it in step with the table, and the rule above gives the same answers.
- **Move to the release channel's default.** Closer to a channel auto-upgrade, but it can change the minor of a cluster whose minor is still offered, and clusters with no channel have no default of their own.
- **Respect `autoUpgrade: false` and maintenance exclusions,** leaving the cluster in ERROR. That is the stranding this ADR removes; GKE itself overrides them at end of support, and the emulator cannot run an image it no longer has.
- **Hold to one minor per upgrade,** as manual upgrades do. An Instance that skipped releases would then have no version to move to, and would go to ERROR.

## Consequences

- A Product image refresh may drop GKE patches and minors again, and the refresh runbook says so.
- A cluster can come back from a restart on a newer version than it was stopped on, and its Operations list says why.
- The one-minor upgrade limit applies only to upgrades a client asks for.
- Maintenance windows and exclusions are still stored but have no effect; emulating them is separate work.
