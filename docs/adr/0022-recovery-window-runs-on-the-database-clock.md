# The Recovery window runs on the database clock, not the Emulator clock

A Cloud SQL instance's Recovery window, both its ends and the transaction log retention that sets its start, is measured on PostgreSQL's clock inside the instance's container, which is wall-clock time. The Emulator clock plays no part: `gcpemu time advance` does not age the WAL archive or move `earliestRecoveryTime`. We chose this because everything else in point-in-time recovery is already on the database clock: Base backups are stamped with it, `latestRecoveryTime` is read from it, and PostgreSQL interprets `recovery_target_time` against it during replay. A window with one end on each clock would be incoherent (#365).

## Considered options

- **Measure retention on the Emulator clock.** `time advance` would then prune, which fits the Emulator clock's role of triggering time-driven behaviour. Rejected because the window's start would be on the Emulator clock and its end on PostgreSQL's: after an advance, `latestRecoveryTime` would lie in the Emulator's past, and in a Deterministic Instance what is pruned would depend on how many API requests had arrived.
- **Fake PostgreSQL's clock to follow the Emulator clock.** Rejected because a container's clock cannot be shifted without intercepting time calls in PostgreSQL itself.

## Consequences

Tests of retention age the WAL archive directly (by back-dating a Base backup) rather than advancing the Emulator clock. This is a Fidelity gap: in Google Cloud the window simply moves with real time, and users who advance the Emulator clock to test time-driven behaviour will not see Cloud SQL's window move.
