# CI does not retry failing Go tests

CI runs every Go test once. When a test fails, the job fails, and nothing reruns the test to turn the job green. In this ADR a flake is a test that fails and then passes on the same commit with no code change. That includes tests that fail on several attempts in a row because of a known timing cause. A test that fails on some commits and passes on neighbouring commits that change neither it nor the code it exercises also counts as a flake, even when nobody reran it. Each flake gets an issue and is fixed where it starts: the test's waits, its port choices, its isolation from parallel packages, or the emulator itself. We decided against automatic retries because many of these failures are real. Slow GKE control-plane bring-up, snapshot races while an Instance is busy, and relay connection resets are bugs a user can hit too, and a retry would hide them while costing runner time on every run.

The one exception is Playwright's single retry in `console-e2e`, which stays because browser timing on shared runners is outside the emulator's control. Performance targets are a different matter. A missed latency or throughput target on a shared runner is not a flake, and CI reports those numbers without failing on them.

## Considered options

- **Rerunning only the failed tests**, for example `gotestsum --rerun-fails`. This would save the reruns people now do by hand: 17 of 60 runs between 2026-10-13 and 2026-10-16. It was rejected because it would make the failures above go quiet, and those failures are real.
- **Quarantining flaky tests behind `t.Skip` or a tag.** This was rejected because it removes coverage of the exact behaviour that is fragile.
