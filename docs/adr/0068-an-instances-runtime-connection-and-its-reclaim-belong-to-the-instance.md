# An Instance's runtime connection and its Reclaim belong to the Instance

An Instance's first connection to its container runtime includes its Reclaim, which takes as long as the runtime does: a median of 3.2 seconds for a default Instance, 7.2 for a large one and about 17 for one with many files. It ran on the context of whichever caller connected first, and some callers allow 5 seconds, so a Reclaim could be cut short depending on timing, and a caller that gave up abandoned the connection for the next one to start again (#1033). So the connection now belongs to the Instance, as its runtime probe does (ADR 0067): one runs at a time, only shutdown ends it, and each caller waits for it as long as its own context allows. A caller that stops waiting gets a transient error saying the connection is still in progress, so the Instance is not ready until it has connected. The Reclaim has its own limit of 2 minutes, several times the slowest case measured, after which the Instance warns and connects anyway.

A Reclaim runs once per run and is not tried again within it. It removes every container its Instance ID tags, which once connected includes the run's own, so what it cannot remove waits for the next start with that Instance ID.

## Considered options

- **Raise the callers' budgets.** Each short-budget caller, such as a Service's readiness check, would allow enough for a Reclaim. Its time would still depend on which caller came first, and the next caller with a short budget would bring the bug back.
- **Try a Reclaim again in the background, removing only containers older than the run.** Leftovers would go without a restart, but cleanup would need a second way to choose containers, and it would race the Services starting theirs.
- **No limit on a Reclaim.** It would never be cut short, but a runtime that hangs during one would leave the Instance never ready, with nothing in its log until shutdown.

## Consequences

- While a Reclaim runs, `/_emu/v1/ready` reports the Instance not ready, for up to 2 minutes on a slow runtime.
- A caller with a short budget no longer decides how long a Reclaim takes, and no longer makes the next caller start the connection again.
- Leftovers that outlast a Reclaim's limit stay until the next start with that Instance ID; nothing removes them sooner.
- Info, which no caller's context limits any more, has a limit of its own (10 seconds), so a runtime that hangs there ends the connection with a transient error and the next caller connects again.
