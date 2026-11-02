# Test data dirs cover CLI-started Instances, and reclaiming one stops its process

ADR 0047 left Instances that the `gcpemu` CLI starts to `gcpemu prune`. The Pre-release host checks start theirs that way, in `t.TempDir()`, and when a check fails, its `gcpemu stop` fails, or `go test -timeout` kills the run, the detached Instance keeps running: one held port 53 and made the next run skip its DNS check, and others kept their networks until Docker's address pools ran low (#914). `prune` could not help, because the dir still held the Instance ID and a container still ran. We decided that a data dir a test hands to a CLI-started Instance that must not outlive the test is a Test data dir too: the test holds its owner lock, and the same reclaim serves the emutest sweep, `gcpemu prune` and the Pre-release run's own sweep at its start. An ended Test data dir's Instance ID is Abandoned even while its containers run, so `doctor` reports it. Reclaiming one first asks a still-running Instance process to stop through the dir's endpoints, then kills the pid in `gcpemu.pid` only if that pid's executable is named `gcpemu` (any case, with or without `.exe`, from any directory), and only then removes its containers, networks and volumes and the dir.

## Considered options

- **Only escalate in the check's own cleanup** (stop, then kill, then remove). It fixes a failed check but not a killed run, which runs no cleanups at all.
- **Kill the pid in `gcpemu.pid` without checking it.** A pid file a crashed Instance left behind can name a reused pid, so the reclaim could kill an unrelated process.
- **Match the exact executable path the Instance recorded at start.** Stricter, but it needs a new field and misses every Instance started before it. A name that does not match only means no kill: the reclaim still removes the objects.
- **Narrow Abandoned Instance ID** so that only emutest reclaims ended Test data dirs. `doctor` would then keep reporting no abandoned IDs while leftovers used up the address pools.

## Consequences

- The owner lock file leaves `emutest` for shared internal code. Its name and locking scheme stay the contract ADR 0047 describes.
- `gcpemu prune` removes ended Test data dirs as well as their objects, after the same confirmation as any other Abandoned Instance ID.
