# A test's own host objects are tagged through a Test data dir

Some tests make runtime objects on the host themselves, outside any Instance: the compute, GKE and Cloud SQL tests create a Docker network on a range so that it overlaps the range they then declare. Each removes its network in `t.Cleanup`, but a killed run (`^C`, `go test -timeout`, a killed CI job) runs no cleanups, and the networks carried no labels, so neither the emutest sweep, `gcpemu prune` nor `doctor` could find them; they held address-pool space until someone ran `docker network prune` (#1035, #1091). We decided that such an object is tagged as an Instance's would be, through a Test data dir of its own: the test makes a dir, holds its owner lock (ADRs 0047, 0057), writes a fresh Instance ID into it that no Instance runs under, and labels the object with that ID (`gcpemu.instance`) and the dir (`gcpemu.dir`). A killed run's dir then holds the ID under a lock that can be taken, so the existing reclaim removes the object, and a concurrent test's object, whose lock is held, is left alone.

## Considered options

- **Tag it with the test's own Instance's ID**, written into the test's Test data dir before the Instance starts. It keeps an Instance ID tied to an Instance, but the Instance lists and removes the networks its ID tags as its own, so it could adopt or delete the network the test relies on, and every such test would have to name a Test data dir.
- **A new label for a test's host objects**, such as `gcpemu.test-owner=<dir>`. It keeps Instance ID meaning only an Instance's, but the sweep, `prune` and `doctor` would each need a second lookup and a second reclaim for the same liveness check.
- **Reclaim by name prefix** (`gcpemu-test-overlap-*`). A name cannot tell a killed run's network from one a concurrent test still uses.

## Consequences

- An Instance ID may tag objects no Instance ever owned. A killed run's are reported by `doctor` as an Abandoned Instance ID and removed by `gcpemu prune`, as ADR 0057 wants for anything using up the address pools.
- The emutest sweep reclaims them only in a test process that starts an Instance, since it runs before the first one; every test that makes such an object today starts one.
- Overlap networks leaked before this change carry no labels and still need a one-time `docker network prune`.
