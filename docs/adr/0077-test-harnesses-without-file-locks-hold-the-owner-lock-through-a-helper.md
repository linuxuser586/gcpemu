# Test harnesses without file locks hold the owner lock through a helper

The console e2e harness (Node, Playwright) started its Instances with `gcpemu start --data-dir <mkdtemp>` and never took the owner lock, so a dead run's dir looked like a live Persistent Instance and neither `prune` nor `doctor` reclaimed it (#1035). On Windows every run leaked: Playwright does not support graceful webServer shutdown there and ends the webServer's process tree with `taskkill /T /F`, so no exit handler, no `rmSync` and no Ephemeral cleanup runs, and `child.kill('SIGTERM')` is a hard kill too. Node has no OS file lock, so we decided that a test harness without one holds the owner lock of ADRs 0047 and 0057 through a dev-only Go helper, `gcpemu-testdir`: `hold <dir>` takes the lock and keeps it until its stdin closes, which happens when the harness dies however it dies; `reclaim <root>` runs the shared reclaim on every ended Test data dir under `root`. The console e2e keeps its dirs under one root, `gcpemu-console-e2e` in the OS temp dir, and sweeps it before it starts the shared Instance.

## Considered options

- **A hidden `gcpemu` subcommand that holds the lock.** No second binary, but it ships test plumbing in the product.
- **Sweeping dirs whose `gcpemu.pid` is dead, with no lock.** Pids are reused, and `prune` and `doctor` still could not tell a dead run's dir from a Persistent Instance's.
- **Relying on cleanup at exit.** On Windows, Playwright's webServer never gets a chance to run it.

## Consequences

- `gcpemu-testdir` is built by the `console-e2e` recipe into `bin/`. Releases do not build it.
- `hold` ignores SIGINT and SIGTERM, so a `^C` or Playwright's graceful shutdown on Linux, which signal the whole process group, cannot drop the lock while the harness still stops its Instance.
- The harness removes a dir itself only after its Instance exited cleanly. A dir whose Instance was killed or failed stays, because it records what the Instance left, and the next run's sweep, `gcpemu prune` or an emutest sweep reclaims both.
- The sweep is the shared run-start reclaim that the Pre-release run uses (`instance.ReclaimEndedUnder`): it reclaims every ended Test data dir that an object on the runtime records, wherever it is, not just the console e2e ones, because the owner lock makes that safe. It gives up after 45 s, inside the time Playwright gives the webServer to start, and a later sweep finishes the rest. A failed sweep is logged and never fails the run.
