# The Emulator clock can be set back to its base

The Emulator clock was only ever advanced: `gcpemu time advance` added to its offset, a restart was the only way to drop it, and `gcpemu reset` left it alone. A client that shares an Instance and signs with the real time then saw its V4 signed URLs refused as `ExpiredToken` once the offset passed their TTL, and nothing told it why. There was also no way to read the clock without moving it, since `time advance 0s` runs every Service's time-driven sweeps (#1172).

We decided that the clock can be set back to its base, but never below it. The base is real time, or in a Deterministic Instance its own clock (ADR 0020), which `gcpemu time reset` and `POST /_emu/v1/time/reset` leave as it is and zero the offset over. `gcpemu reset` zeros the offset too, as part of making the Instance new. A State snapshot restore keeps the offset, as it keeps the rest of the clock. `gcpemu time show` and `GET /_emu/v1/time` read the clock and its offset without moving it or running any sweep.

Setting the clock back only moves the clock. What an advance set off (lifecycle deletions, expired TTLs, retention) stays done, and no sweep runs on a reset. Undoing those is what a State snapshot restore is for.

## Considered options

- **Let `time advance` take a negative duration.** It was rejected because it would allow a clock below real time, where stored timestamps can be in the future. It also asks the client to know the offset to undo it.
- **Undo what an advance set off when the clock goes back.** It was rejected because deletions and expiries cannot be undone without saved state, which is what State snapshots already provide.
- **Leave `gcpemu reset` alone and only add `time reset`.** It was rejected because a reset is meant to give a new Instance, and a clock still ahead is state the reset left behind.

## Consequences

- The glossary's Emulator clock is no longer "never rewound". Code that compares a stored time with the clock must allow the clock to go back to its base, as it already must across a restart.
- Advancing the clock, setting it back, Reset and State snapshots are serialised, so a reset cannot land between an advance's move and its sweeps.
