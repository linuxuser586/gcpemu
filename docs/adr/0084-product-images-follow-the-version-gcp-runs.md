# Product images follow the version GCP runs, not the latest upstream

Its rule that a GKE version is never removed from the table is superseded by ADR 0085 (`0085-a-cluster-on-a-retired-version-is-upgraded-off-it-when-brought-up.md`).

A Product image (CONTEXT.md) decides how a Product behaves, so its version is part of what the emulator promises. Upstream usually runs ahead of GCP: in November 2026 Docker Hub had `postgres:17.12` while Cloud SQL ran 17.11, k3s had 1.36.6 while GKE offered 1.36.5 at most, and Gateway API had v1.7.0 while GKE installed v1.5.0 CRDs (#895). An emulator newer than GCP accepts what GCP rejects, and an older one rejects what GCP accepts; both are wrong.

We decided that a Product image refresh moves each Product image to the version GCP publishes for that Product: Cloud SQL's minor version per major and its maintenance version, the Kubernetes patch versions and per-channel defaults and minors in GKE's release notes, and the Gateway API CRD bundle GKE installs, matched at the minor. Where GCP publishes nothing, such as the Cloud CLI emulators, the Secret Manager CSI driver, or the public Managed Service for Prometheus manifest, the image takes the latest upstream release. Renovate never proposes a Product image (#896); a refresh is a deliberate change with its own issue.

A refresh changes what an existing resource runs, because a Cloud SQL instance's container and a GKE cluster's k3s image are resolved from the version tables each time they start. A refresh is therefore treated as maintenance already applied: a Cloud SQL instance that starts on a newer minor reports it in `databaseInstalledVersion` and `maintenanceVersion`. A GKE version is never removed from the table until clusters on a retired version are moved off it automatically, as GKE's auto-upgrade does, because a cluster whose version is missing goes to ERROR when it is next brought up.

## Considered options

- **Latest upstream for everything.** Simplest to keep current, and Renovate could do it, but the emulator would run ahead of GCP and accept specs, SQL and API versions GCP does not yet have.
- **Latest upstream, capped at GCP's version.** The same result as following GCP wherever GCP publishes a version, with a second source to check.
- **Store the image per resource,** so existing instances and clusters keep their old image until upgraded. Old tags would stay in use indefinitely, and every refresh would need an upgrade path the emulator does not have.

## Consequences

- Every refresh starts from GCP's docs (Cloud SQL's database versions page and release notes, GKE's release notes and `gatewayclass-capabilities` page, the Cloud CLI release notes), recorded in the refresh runbook.
- The emulator stays up to one GCP maintenance cycle behind upstream on purpose; a newer upstream image is not a reason to refresh unless GCP has moved too or the image has a fixable vulnerability (#1165).
- The GKE version table grows by a patch per minor at each refresh until automatic moves off retired versions exist.
- A Managed emulator refresh can change the Firestore and Datastore export format the emulator saves data in; a refresh checks that data saved on the old image imports on the new one.
