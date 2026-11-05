# An Instance reports its runtime without waiting on it

`/_emu/v1/info` says which Instance answers and how its container runtime is, and every CLI command asks it first (ADR 0066), so a slow runtime could make a Live Instance Unconfirmed. Its handler probed the runtime itself, and waited for any connection to the runtime in progress, which removes orphaned containers and takes as long as the runtime does (#1025). So the probe now belongs to the Instance rather than to a request: one runs at a time, and `/_emu/v1/info` waits at most a second for it and then gives the last answer. A probe that shutdown cuts short records nothing, since it says nothing about the runtime.

Until a first probe has finished, the runtime's `kind` is `unknown`. It is not `none`, which says that a probe found no usable runtime. The last answer carries no mark of its age: a probe always ends, so it is out of date only briefly, and it is corrected by a true answer.

## Considered options

- **Return the last answer at once.** Identity would never wait at all, as #1025 first asked. But a caller that asks once would be told something an hour old after an idle spell, or `unknown` on every new Instance, however healthy the runtime, and the Web console's e2e specs would have to poll. A hung runtime costing each stale `/_emu/v1/info` a second is the cheaper mistake, well inside the check's 10 seconds.
- **Probe all the time in the background.** Answers would always be fresh, but an idle Instance would ask its runtime every few seconds for nobody.
- **A lighter endpoint than `/_emu/v1/info`.** ADR 0066 rejected it: it adds to the Admin API, and an Instance too old to have it would need a fallback.
- **Mark the answer's age, with a timestamp or a flag.** Nothing would act on it in the few seconds it matters, and it would add to the Admin API.

## Consequences

- Probing does not connect: no orphaned containers are reclaimed until a Service needs the runtime.
- While a runtime stays hung, `/_emu/v1/info` takes up to a second whenever its answer is stale.
- `reachable` can stay `true` for as long as a probe takes after a runtime hangs: about 3 seconds for a connected runtime, longer while detection tries each listed endpoint.
- `unknown` is part of the Admin API, so readers of `kind` must expect it.
