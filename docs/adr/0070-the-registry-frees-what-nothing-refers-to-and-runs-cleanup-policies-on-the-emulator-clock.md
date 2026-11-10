# The registry frees what nothing refers to, and runs cleanup policies on the Emulator clock

The registry stored a repository's cleanup policies but never ran them, and deleting a version freed only its manifest: the blob links of its image kept every layer and config blob referenced until the whole package or repository was deleted. A workflow that pushes a new tag on every build grew one machine's blob store to 7.3 GB (#1039). So the registry now frees what nothing refers to, and runs cleanup policies as Artifact Registry does, in one Registry cleanup pass.

Deleting a version, through the API, a cleanup policy or a registry manifest `DELETE`, drops in the same change each of its image's blob links that no remaining manifest of that image uses, and the blob content goes once nothing in the registry uses it. Artifact Registry frees unreferenced layers daily instead; freeing them at once changes nothing a client can see. The Registry cleanup pass also drops links that no manifest of their image uses once they are older than a grace period of 1 hour on the Emulator clock, which repairs stores that leaked before this change and pushes abandoned before their manifest; the grace period keeps a push that has uploaded its layers but not yet its manifest. It deletes upload files with no live session and sessions untouched for longer than the grace period. This covers standard and remote repositories and the Mirror cache alike.

Cleanup policies run on standard repositories only, as Artifact Registry documents; on remote and virtual repositories they are stored and do nothing. Artifact Registry runs them about once a day. The pass runs at startup and once a minute of real time, so a machine that is often off still cleans up and a policy change shows its effect quickly. In a Deterministic Instance it runs only when the Emulator clock is advanced: on a real-time ticker, whether a version that has just become old enough vanished before the next request would depend on how fast the client ran.

Matching follows Artifact Registry's documentation, and where it is silent: a version matches `tagPrefixes` when any of its tags has one of the prefixes; `olderThan` and `newerThan` count from the version's create time on the Emulator clock; `mostRecentVersions` ranks a package's versions by create time, newest first, ties broken by version name; a version a policy deletes loses its tags, as a forced delete does; and with `immutableTags` no tagged version is deleted. A version that a remaining index in its image references is never deleted, as Artifact Registry documents, and becomes eligible once that index is gone. Going beyond the documentation, neither is a referrer whose subject remains, so that a policy deleting untagged versions does not strip every tagged image of its signatures and attestations.

Artifact Registry reports cleanup, and dry runs as `BatchDeleteVersions` with `validateOnly`, in Data Access audit logs, which the emulator does not have. Each version deleted, or under `cleanupPolicyDryRun` that would be, is logged once, naming the repository, the version, its tags and the matching policies; a dry run reports a version only the first time a pass in the running process finds it. Deletions are not Operations.

## Considered options

- **Free layers only in the sweep, daily, as Artifact Registry does.** Matches the service, but space would come back a day late for no visible gain.
- **Free layers only on delete.** Leaves the links that already leaked, and the uploads of abandoned pushes, for ever.
- **Run policies once a day.** A laptop that is off at the daily run would never clean up, and a user trying out a policy would wait a day to see it work.
- **Run the pass on a real-time ticker in a Deterministic Instance too.** Golden tests would see versions disappear depending on timing, as Cloud Storage lifecycle already can.
- **Grace period on real time.** A slow test could lose a push's links in a Deterministic Instance, where the Emulator clock barely moves.
- **Leave referrers unprotected.** Exactly what Artifact Registry documents, but a common policy would delete every image signature.
- **Report each dry-run match on every pass.** A pass a minute would repeat the same lines without end.

## Consequences

- A store that leaked before this change shrinks on the first pass after the upgrade.
- A push reusing a layer in the instant its last other user is deleted fails with `MANIFEST_BLOB_UNKNOWN`, as it can on any registry, and succeeds when pushed again.
- In a Deterministic Instance nothing is cleaned up, and no abandoned upload or orphaned link freed, until `gcpemu time advance` runs.
- After a restart, a dry run reports each matching version once more.
- Referrer protection is a deliberate departure from Artifact Registry; a referrer whose subject is deleted is cleaned up like any other untagged version.
