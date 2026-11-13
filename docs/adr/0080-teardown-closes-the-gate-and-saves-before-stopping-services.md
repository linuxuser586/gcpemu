# Teardown closes the gate and saves before stopping Services

Amends ADR 0065.

Teardown stopped every Service with its context first, and only then saved the data of a Persistent Instance's Firestore and Datastore Managed emulators, with 20s cut from that same context. A Service whose `Stop` waited out the context, such as a Pub/Sub server with a streaming pull in progress (#1117), left the save a context that had already ended: each export failed at once, and data written since the emulators started was lost on a stop that still counted as Clean (#1118). The API gateway also kept serving Delegated APIs until the end of Teardown, so a write it answered during or after the export was lost with no error at all.

We decided that Teardown begins by closing the API gateway's gate, as Reset does: every request but the Admin API's is refused as UNAVAILABLE, with a message that the Instance is shutting down and no `Retry-After`, and those in progress are cut off. Teardown waits for them for at most 10s of its context; past that it logs a warning and carries on. It then stops starting Managed emulators and saves their data, within 20s, before it stops any Service. Services and the steps after them get what is left of Teardown's 30s: nearly all of it when the save is quick, and at least 10s when the gate's wait and the save both use their limits. The 15s, 30s and 55s limits of a shutdown request, and `gcpemu stop`'s 60s, are unchanged.

## Considered options

- **Reserve the save's time.** Keep the order, and give Services and the steps after them a context ending 20s before Teardown's. It caps Services at 10s even when the save takes a second, and still saves while the gateway answers writes.
- **Save first without closing the gate.** It gives the save its time, but writes the gateway answers after the export are lost silently, which is worse than the error logged before.
- **A limit of the save's own, apart from Teardown's context**, as ADR 0078 did for `Cleanup`. It is the most robust, but runs past Teardown's 30s into the time ADR 0078 keeps for `Cleanup` within `gcpemu stop`'s 60s, so it would cost the removal of containers what it gives the save.
- **A gate wait with no limit of its own.** A handler that ignores its cancellation would use up Teardown's context before the save, the same failure one step earlier.

## Consequences

- During Teardown, clients get UNAVAILABLE through the API gateway at once, and streams through it end at the start of Teardown rather than at its end. Services' own ports are not behind the gate and stop as before.
- A handler that does not return within the gate's 10s may still write after the export; only one that ignores its cancellation can.
- A save that needs more than 20s still loses data written since the emulators started, as the Instance logs.
- A direct `Shutdown` whose `Start` uses up its context still leaves Teardown, and the save, a context that has already ended, as ADR 0065 decided.
