# Costly Products record weekly, not nightly

ADR 0023 has a nightly job make the Conformance suite's Recordings. For GKE that means creating real clusters every night: a regional Autopilot cluster takes several minutes to create and as long to delete, and bills its management fee and nodes while it exists. We decided that a Product whose Recordings need resources like that records weekly, and on demand through `workflow_dispatch`, while the rest of the suite keeps recording nightly. GKE is the first such Product. Each Product's cadence is set where the recording job selects its tests, so a later Product (Cloud SQL, for one) can join the weekly run without another decision.

The cost is that Drift in a weekly Product surfaces up to a week late. A Mismatch caused by a change to the emulator is still caught on the PR that makes it, since CI compares with the committed Recordings whatever their age.

## Considered options

- **Record everything nightly.** It catches Drift soonest, but roughly seven times the GKE spend and run time for changes that GKE ships through release channels over weeks, not overnight.
- **Record costly Products only on demand.** It costs least, but nothing would notice Drift until someone thought to record, so the Recordings would quietly go stale.
