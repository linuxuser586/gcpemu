# The Conformance suite compares the emulator with Recordings, not live GCP

SRS 11.1 describes the Conformance suite as the same Go tests run against the emulator and, nightly, against a real GCP sandbox project, with the responses diffed. We do not diff the two live. A nightly job runs the suite against the sandbox project and saves GCP's normalised responses as Recordings committed to the repo. Every CI run compares an in-process Instance with those Recordings, without credentials and behind the Escape guard. A difference there is a Mismatch, and the PR fails. When a nightly Recording differs from the committed one, that is Drift: the job puts the new Recordings on one rolling PR, whose CI then shows the Mismatches the Drift causes.

This catches a regression in Faithful behaviour on the PR that introduces it, not the next morning, and runs in pull requests from forks, which cannot have GCP credentials. It also turns a change in GCP into a reviewable diff of files instead of a red nightly run. The cost is that the Recordings are only as fresh as the last successful nightly run, and a test has no comparison until a Recording for it has been made against GCP. Until then the test skips and says how to record it.

## Considered options

- **Live diff each night.** Simplest to build, and always compares with today's GCP. It was rejected because a Mismatch would surface only overnight, detached from the change that caused it, and Drift and Mismatch would look the same: a failed nightly run.
- **Live diff on every PR.** It was rejected because it would need GCP credentials, real resources and real latency in every CI run, including on forks.
