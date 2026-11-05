# Teardown waits for `Start`, up to the shutdown's deadline

The Admin API is served before Services start, so `POST /_emu/v1/shutdown` could run alongside `Instance.Start`, and nothing ordered the two. #1011 showed the result: `Start` wrote the PID and endpoints files after `shutdown` removed them, and started Services that had already been stopped, which for Cloud SQL and Artifact Registry revives background work. A stop by signal never had this problem, because `host` shuts down only after `Start` returns. We decided that every shutdown works that way. Shutdown begins at once, so `WaitReady` and Seeding hear of it immediately and the context `Start` gives its steps is cancelled. Teardown, which stops Services and removes runtime files, runs only once `Start` has returned, and `Start` returns `ErrShutDown` at its next check: it starts no further Service, writes no runtime files and does not log that it started. Once shutdown has begun, a call to `Start` does nothing at all.

The wait is limited by the context given to `Shutdown`. If that ends first, the Instance logs a warning and tears down with `Start` still running, so a stuck `Start` never stops the Instance's own shutdown from finishing. `emu.Service` states what follows: `Stop` overlaps `Start` only past that deadline, and `Start` is not called after `Stop` during a shutdown.

## Considered options

- **Tear down at once and have `Start` skip what is left.** This is the smaller change and fixes the runtime files, but it can't fix a Service's `Start` already running when its `Stop` runs. Services read and write fields such as their server and cancel function with no lock, on the understanding that the two don't overlap.
- **Lock inside every Service.** It would make the overlap safe, but every Service would have to get it right, and it would still leave `Start` running after `Stop`.
- **Wait with no limit.** It needs no warning and never overlaps, but a Service's `Start` that doesn't honour its context would hang a stop, as ADR 0064 refuses for `gcpemu stop`.

## Consequences

- A slow `Start` uses up the time `Shutdown` was given: the Admin API's request allows 30s in all, so a `Start` that returns late leaves less for `Stop`, draining the gateway and saving Managed emulators' data, and one that outlasts the deadline leaves them a context that has already ended.
- An Instance that has begun to shut down never starts, so a `Start` that comes after the request returns `ErrShutDown` and can't be retried.
- Past the deadline, what the wait prevents can still happen: a `Start` still running may touch a Service that was stopped or a store that was closed. Its later steps are skipped, and it can't leave host mode running.
- `Instance.Start` itself returns only when the steps it is running do, so a `gcpemu start` whose Service's `Start` never returns stays in `Start` after `Done` closes, and `gcpemu stop` reports that the process did not exit (ADR 0064). A Service's `Start` that honours its context avoids this; making every one do so is not part of this decision.
