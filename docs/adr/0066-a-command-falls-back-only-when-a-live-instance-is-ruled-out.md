# A command falls back only when a Live Instance is ruled out

`gcpemu start` and `gcpemu ca rotate` each do something with no Instance to ask: one starts a second process on the data directory, the other rotates the Instance CA on disk. Both are costly if a Live Instance is using that directory, and #1018 and #1024 first let them go ahead unless a check said otherwise. An Instance can connect and never answer, which read as "nothing answers", so its CA was rotated from under it. So a command now falls back only when asking rules a Live Instance out, and an Unconfirmed Instance makes it refuse. Foreground `start` asks too, where it used to ask nothing. Nothing locks a data directory, so this check is all that keeps two Instances off one. The refusal names the `endpoints.json` to remove and the PID the data directory last recorded, for a person to check first.

A connection that never forms, an answer from another Instance, and an answer that is no Instance's, including one that isn't HTTP, rule one out. So do a missing or unreadable `endpoints.json`, which an Instance writes atomically. A refused Admin token, a 5xx, and a connection that stalls or drops rule nothing out. The check dials with a timeout of 3 seconds, shorter than the client's own: without it, an address that drops packets and an Unconfirmed Instance that connects and never answers fail alike. A data directory with no `instance-id` file has no ID to compare, so an Instance that answers there is taken as its own, as for `stop`.

## Considered options

- **Go ahead unless told no.** It never strands a data directory, but the mistake it allows is the costly one, and it is silent.
- **Treat every failure to confirm as an Unconfirmed Instance.** It is the simplest rule, but `start` would refuse after every crash, the case the fallback exists for.
- **An override flag.** It would free a stranded data directory in one step, but #1024 rules it out: a script would pass it every time, and removing a file is a deliberate act.
- **Let a dead PID in the PID file rule a Live Instance out.** It would spare the commonest refusal, another Instance with an Admin token on the port after a crash. But a PID can be reused, the file records no PID scope, and a Containerized Instance's PID means nothing here (ADR 0064). The refusal prints it and the check never acts on it.
- **A lighter endpoint than `/_emu/v1/info`.** The check would not wait on the container runtime, but it adds to the Admin API, and an Instance too old to have it would need a fallback.
- **No dial timeout of its own.** It keeps one timeout, but a left-behind LAN address that drops packets would look like an Unconfirmed Instance and refuse `start`.

## Consequences

- An Instance that connects and never answers, or a false alarm such as a 5xx from another server, stops `start` and `ca rotate` until someone acts.
- An Instance on a LAN address that takes over 3 seconds to connect counts as ruled out.
- Commands that need an Instance fail either way; only their message says which.
- `gcpemu wait` with a `--timeout` shorter than the check reports the timeout, not an Unconfirmed Instance, so that `--timeout` holds.
- The rule binds commands that would do something costly in place of a Live Instance. `gcpemu dns install` with a fixed port needs no Instance and goes ahead on an Unconfirmed one; with `--port dns=0` it has no port to use, so it refuses.
- Every command now waits on `/_emu/v1/info`, which asks the container runtime, so a hung runtime can make an Instance Unconfirmed.
