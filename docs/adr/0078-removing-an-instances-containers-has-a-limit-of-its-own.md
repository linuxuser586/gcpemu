# Removing an Instance's containers has a limit of its own, within `gcpemu stop`'s

Amends ADR 0065.

Teardown's last removal of the Instance's containers, networks and volumes, the runtime's `Cleanup`, had its own 60s, made from `context.Background()`, and removing the LAN relay and the VM relay had 30s each. The runtime client has no request timeout, so against a runtime that hangs after connecting, a shutdown request could take 150s or more, against `gcpemu stop`'s 60s (#1031). We decided that `Cleanup` has a limit of its own of 30s on every path: a shutdown request, a stop by signal, `emutest`, a direct `Shutdown` and a failed startup, which now shuts down with the same 30s as a stop by signal. It does not derive from the context given to `Shutdown`, so `Shutdown` can run past that context by up to 30s. For a shutdown request, `Cleanup` also ends no later than 55s after the shutdown began: the 15s wait for `Start` and the 30s for Teardown leave it at least 10s, and `gcpemu stop`'s 60s is unchanged. Removing the VM relay shares `Cleanup`'s limit, and Teardown removes the LAN relay with its own context; neither keeps 30s of its own there. Off the stop path, `Frontend.Close` and a LAN relay that failed to start keep 30s. Both relays carry the Instance's label, so `Cleanup` removes one that its limit cut short when it can. When `Cleanup` does not finish, the Instance logs once that it could not confirm its containers were removed, that they may still be running, and that the next start with its Instance ID tries to remove them: its Reclaim (ADR 0068).

## Considered options

- **Teardown's context.** A hung runtime would no longer hold a stop, but Teardown runs earlier steps first, so a context that had already ended would make the first list fail at once and remove nothing, even on a healthy runtime. A failed startup's `Shutdown(context.Background())` would have left `Cleanup` with no limit at all.
- **A flat limit that fits beside ADR 0065's.** That leaves 15s. `Cleanup` on a healthy runtime (WSL Docker, Ephemeral Instance, median of 3 runs) took 3.2s for the default 5 nodes, 40 pods, Cloud SQL and Spanner; 7.2s for 16 nodes and 200 pods; 11.4s for that with the CPU saturated; 17.6s with 800k files in pods' emptyDirs; and 17.2s with many files in node volumes. Docker Desktop and macOS were not measured.
- **Keep the separate limits and change ADR 0065's.** Shortening the wait for `Start` or Teardown to make room would cost the slow `Start` and the Managed emulators' saved data that ADR 0065 protects.

## Consequences

- A shutdown request still ends within `gcpemu stop`'s 60s against a hung runtime, but a `Cleanup` cut short leaves containers, networks or volumes for the next start's Reclaim with that Instance ID, if there is one. It is still a Clean stop and a Confirmed stop (ADR 0064).
- `Cleanup` gets 30s only when earlier steps finish quickly; after a `Start` wait and Teardown that use their full limits, including a Service's `Stop` running `emu.StopGrace` past its context (#1116), it gets 9s, less than a large or busy Instance needs.
- A stop by signal can take up to 60s: 30s for the shutdown, and 30s more for `Cleanup`.
- Containers, networks and volumes share the one limit, in the order they always had.
