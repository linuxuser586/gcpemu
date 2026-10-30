# emutest reclaims Test data dirs whose test has ended

A test process that dies (`^C`, `go test -timeout`, a killed CI job) runs no cleanups, so its Test data dirs stay on disk still holding their Instance IDs, and its Managed containers keep running. `gcpemu prune` reclaims neither: a dir that holds the ID and a running container each mark an ID as live (#846). We decided that `emutest` reclaims them itself. Each test holds an exclusive lock on an owner lock file in its Test data dir, from before the Instance is created until the test has removed the Instance's resources. Once per test process, before its first Instance starts, `emutest` sweeps: for every dir that a `gcpemu.dir` label names and that has an owner lock file it can lock, it removes that Instance ID's containers, networks and volumes while holding the lock, then deletes the dir. It also reclaims Abandoned Instance IDs whose labelled dir was under the OS temp dir. The operating system releases the lock when the owner dies, so a lock that can be taken is proof that the test has ended.

## Considered options

- **A scoped `gcpemu prune` in `task test`**, as #846 proposed. Tests also run as plain `go test`, from an IDE and from agents, and none of those go through the Taskfile. And `prune` would still need its own way to tell a dead test's Instance from a live one.
- **A PID file as the liveness check.** A cleanly stopped Instance deletes `gcpemu.pid`, PIDs are reused, and an Instance between `New` and `Start` has none yet, so a sweep would either miss dead tests or reclaim live ones.
- **Reclaiming every Instance under the OS temp dir.** That would remove a developer's own stopped Instance in `/tmp`, and a concurrent test run's.

## Consequences

- The owner lock file is a contract between test processes, including ones built from older and newer versions of `emutest`. Changing its name or locking scheme leaves the other version's leftovers unreclaimed until the OS clears the temp dir.
- `WithDataDir(dir)` takes no lock, so its dirs are never swept, because its caller owns their lifetime. Instances that the `gcpemu` CLI starts, an agent's included, are left to `gcpemu prune`, except one whose data dir was in the OS temp dir and has been deleted: no one can restart it, so the sweep reclaims it with the Abandoned Instance IDs of tests.
- An interrupted run's resources stay on the host until the next test process that starts an Instance, which pays for the sweep once. A failed sweep is logged and never fails that test.
