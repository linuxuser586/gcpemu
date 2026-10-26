# Instances run Google's emulators as Managed emulators, in memory only

Amends ADR 0027. Amended by ADR 0036, which has Firestore's and Datastore's keep their data.

ADR 0027 has an Instance forward a Delegated API to an External emulator that the user runs and configures by address. Users want Firestore, Datastore, Spanner and Bigtable with no setup, like the rest of the emulator. We decided that `--external API=managed` has the Instance run Google's own emulator for that API as a Managed emulator: a Managed container that the Instance starts with itself, owns and removes when it stops.

**Google's images, pinned by tag.** Firestore and Bigtable run from Google's `google-cloud-cli` image at a versioned `-emulators` tag, and Spanner from `gcr.io/cloud-spanner-emulator/emulator` at a versioned tag. Both are pinned in the source like every other Managed container image. Datastore runs the Firestore emulator in Datastore mode, since Google deprecated the Datastore emulator. Each Delegated API gets its own container, even when two of them come from the same image. Only these four APIs can be managed. Pub/Sub has its own Service, and no other API has an emulator from Google.

**Owned like any Managed container.** Unlike an External emulator configured by address, a Managed emulator is part of the Instance: the Instance is not ready until each one accepts connections, and one that the runtime stops or restarts is a Disrupted container. Clients still reach it only through the API gateway, as the same passthrough with no IAM check and no Fidelity claim, so it has no address on the host or the LAN of its own. With no usable container runtime, the Instance starts, and each managed Delegated API answers UNAVAILABLE naming the missing runtime, as Runtime Services are skipped.

**In memory only.** Reset replaces each Managed emulator's container, which clears its data. State snapshots do not hold that data, and a Persistent Instance loses it on every stop. This is the same for all four APIs, though Firestore's and Datastore's emulators can import and export data.

## Considered options

- **Starting the emulator lazily** on the first request to its API. Startup would be quicker, but the first request would wait for an image pull and an emulator start, and tests that wait on readiness would still meet an emulator that isn't ready yet.
- **Leaving a Managed emulator out of readiness**, as for one configured by address. The Instance owns it, so it can and should report it the way it reports Cloud SQL's Postgres.
- **Leaving its data alone on Reset**, as ADR 0027 does for Delegates. Reset promises that all data is removed, and a Managed emulator's data lives inside the Instance.
- **Persisting Firestore's and Datastore's data** with their import and export flags. Two APIs would then keep their data and two would not, and export on exit loses data on a crash. It is left for a later decision.
- **Pinning images by digest.** It would make Managed emulators the only Managed containers pinned that way. A change of convention covers all of them, or none.
- **A separate flag**, such as `--emulators firestore,spanner`. Two settings would then each say how one API is delegated, and could disagree.

## Consequences

- ADR 0027's "An External emulator is always configured by address" no longer holds, and Reset does act on the Managed emulators among the Delegates.
- The first start with a Managed emulator pulls a large image (the `google-cloud-cli` emulators image is over 1 GB), and the Instance is not ready until the pull is done.
- Pulling these images from gcr.io is a registry pull of a Managed container image, not a call to a Google API, so NFR-SEC-002 still holds.
