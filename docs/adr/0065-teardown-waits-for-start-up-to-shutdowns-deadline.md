# Teardown waits for `Start`, up to the shutdown's deadline

The Admin API is served before Services start, so `POST /_emu/v1/shutdown` could run alongside `Instance.Start`, and nothing ordered the two. #1011 showed the result: `Start` wrote the PID and endpoints files after `shutdown` removed them, and started Services that had already been stopped, which for Cloud SQL and Artifact Registry revives background work. A stop by signal never had this problem, because `host` shuts down only after `Start` returns. We decided that every shutdown works that way. Shutdown begins at once, so `WaitReady` and Seeding hear of it immediately and the context `Start` gives its steps is cancelled. Teardown, which stops Services and removes runtime files, runs only once `Start` has returned, and `Start` returns `ErrShutDown` at its next check: it starts no further Service, writes no runtime files and does not log that it started. Once shutdown has begun, a call to `Start` does nothing at all.

The wait is limited by the context given to `Shutdown`. If that ends first, the Instance logs a warning and tears down with `Start` still running, so a stuck `Start` never stops the Instance's own shutdown from finishing. A shutdown request sets its own limits: 15s for `Start`, then a separate 30s for Teardown, which together stay within `gcpemu stop`'s 60s. `emu.Service` states what follows: `Stop` overlaps `Start` only past that deadline, and `Start` is not called after `Stop` during a shutdown.

## Considered options

- **Tear down at once and have `Start` skip what is left.** This is the smaller change and fixes the runtime files, but it can't fix a Service's `Start` already running when its `Stop` runs. Services read and write fields such as their server and cancel function with no lock, on the understanding that the two don't overlap.
- **Lock inside every Service.** It would make the overlap safe, but every Service would have to get it right, and it would still leave `Start` running after `Stop`.
- **One limit for the wait and Teardown.** `ctx` would stay a simple limit on the whole shutdown, but a `Start` that used it up would leave Teardown a context that had already ended: saving Managed emulators' data fails at once, so a slow `Start` costs a stop that still counts as Clean its data. A direct call to `Shutdown` still works this way, because its caller set the limit and hears the errors.
- **Wait with no limit.** It needs no warning and never overlaps, but a Service's `Start` that doesn't honour its context would hang a stop, as ADR 0064 refuses for `gcpemu stop`.

## Consequences

- A shutdown request can take 45s, up from 30s, and a Service's `Start` that needs longer than 15s to return past its context overlaps its `Stop`.
- `Shutdown`'s `ctx` limits the wait and Teardown together, so a caller that passes one short deadline can still run Teardown with a context that has already ended.
- An Instance that has begun to shut down never starts, so a `Start` that comes after the request returns `ErrShutDown` and can't be retried.
- Past the deadline, what the wait prevents can still happen: a `Start` still running may touch a Service that was stopped or a store that was closed. It starts no further Service and no step after them, and it can't leave host mode running. Steps before the Services, such as the Google front end, have no check: only a direct `Shutdown` can reach them, as the Admin API isn't served yet.
- `Instance.Start` itself returns only when the steps it is running do, so a `gcpemu start` whose Service's `Start` never returns stays in `Start` after `Done` closes, and `gcpemu stop` reports that the process did not exit (ADR 0064). A signal is no better, as before: `host` calls `Shutdown` only after `Start` returns, so the signal cancels `Start`'s context and nothing more. A Service's `Start` that honours its context avoids this; making every one do so is not part of this decision. #1020 lets the process exit anyway.
