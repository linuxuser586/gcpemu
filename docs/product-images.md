# Product image refresh

A Product image refresh (CONTEXT.md) moves Product images to the version the real Product runs
(ADR 0084). Renovate never proposes a Product image (`renovate.json`, #896), so each refresh is
a deliberate change with its own issue. Start one when:

- GCP has moved: a new Cloud SQL minor or maintenance version, or GKE release notes that add patches
  or change channel defaults;
- the weekly scan's Rolling issue (#1165, `.github/workflows/scan.yml`) reports a fixable
  vulnerability in a Product image. A CVE never justifies going past the version GCP runs; where
  GCP publishes no version, take the latest upstream release.

Read GCP's sources on the day you start, not the day the issue was written.

## Cloud SQL `postgres` images

- **Pin:** `pgVersions` in `services/sql/images.go`: one `postgres:<major>.<minor>-alpine` image
  and `Installed` string per `databaseVersion`.
- **Source:** the Current minor version column of Cloud SQL's
  [database versions page](https://docs.cloud.google.com/sql/docs/postgres/db-versions). Never
  take a newer minor from Docker Hub.
- **Checks:**
  - `Installed` matches the image: `postgres:17.11-alpine` → `POSTGRES_17_11`.
  - `maintenanceSuffix`, next to the table, is the newest release in the
    [maintenance changelog](https://docs.cloud.google.com/sql/docs/postgres/maintenance-changelog)
    (headings such as `POSTGRES_17_11.R20261013.02_04`); its suffix is `.R20261013.02_04`.
  - A refresh counts as maintenance already applied. An existing instance reports the new values
    the next time its container is created: Instance start, `instances.restart` of a FAILED
    instance, a backup or State snapshot restore. `TestRefreshCountsAsMaintenance` covers this.

## GKE versions

- **Pin:** `services/gke/versions.go`: `versions` (GKE version → `rancher/k3s` image and preloaded
  system images), `defaultVersion`, `channelDefaults` and the minors in `channelVersions`.
- **Source:** GKE's [release notes](https://docs.cloud.google.com/kubernetes-engine/docs/release-notes),
  in particular the Current versions table:
  - each channel's default and its available minors (limited to the minors the table holds);
  - the newest patch GKE offers per minor. Upstream Kubernetes `X.Y.Z` is GKE `X.Y.Z-gke.N`; the
    table uses `X.Y.Z-gke.100`.
- **Checks:**
  - Never add a k3s patch GKE does not offer, even when k3s has released it.
  - `defaultVersion` is the Regular channel's default.
  - Each version's `Preload` list matches its k3s release's `k3s-images.txt`
    (`https://github.com/k3s-io/k3s/releases/download/v<X.Y.Z>%2Bk3s1/k3s-images.txt`) minus
    traefik, which the emulator disables, and klipper-helm, which only installs traefik. Check each tag; don't copy the
    previous version's entry.
  - A refresh may drop patches and whole minors, and need not keep old k3s images for them (ADR
    0085): a cluster or node pool on a Retired version is upgraded off it when it is next brought
    up. Only a version newer than every table entry still strands a cluster, so never drop the
    newest version without a newer one.

## Managed emulators

- **Pin:** `cliEmulatorsImage` and `spannerImage` in `internal/instance/managed.go`.
- **Source:** GCP publishes no version for them, so they take the latest upstream release: the
  newest `google-cloud-cli:<version>-emulators` tag (see the
  [Cloud SDK release notes](https://docs.cloud.google.com/sdk/docs/release-notes)) and the newest
  [cloud-spanner-emulator release](https://github.com/GoogleCloudPlatform/cloud-spanner-emulator/releases).
- **Checks:**
  - **Export format.** A new Cloud CLI can change the Firestore and Datastore export format the
    emulators save data in. Before merging, start an Instance on the old image, write documents
    to a Firestore and a Datastore Managed emulator, stop it, start it on the new image, and read
    the documents back. Report the result in the PR. If they don't import, stop and report it;
    don't work around it.
  - The Cloud Build tests' `cloudSDK` (`services/build/agent_test.go`) uses the same Cloud CLI
    version with the `-alpine` suffix, so the repo carries one gcloud version.

## Vendored GKE add-ons

The embedded manifests in `services/gke/addons/`. Each file's header names its upstream source.

| Add-on | Pin | Source |
|--------|-----|--------|
| Gateway API CRDs | `gatewayAPIVer` in `services/gke/addons.go`, `gateway-api-<ver>-{standard,experimental}.yaml.gz` | The CRD bundle GKE installs (GKE's `gatewayclass-capabilities` page and release notes), matched at the minor. Take the newest upstream patch of that minor. |
| Managed Service for Prometheus | `managedPrometheusManifest`, `managed-prometheus-<ver>.yaml.gz` | GKE's own builds are not published: take the newest non-rc `prometheus-engine` tag (`git ls-remote --tags`; tags may have no GitHub release) and copy the two `PodMonitoring`/`ClusterPodMonitoring` CRDs from its `manifests/setup.yaml` unchanged. |
| Secret Manager add-on | `secret-manager.yaml` | The newest `secrets-store-csi-driver` release: its `deploy/` CRDs, RBAC and DaemonSet, renamed to `secrets-store-gke.csi.k8s.io`. Diff the old and new `deploy/` trees and apply that diff. |

The `sigs.k8s.io/secrets-store-csi-driver` Go module in `go.mod` moves in step with the Secret
Manager manifest. Renovate may move the module first; then bring the manifest to the same
version.

When a manifest's file name changes, update `productImageFiles` in
`internal/devtool/renovate_test.go`.

## Copies to update

Tests in the same package as a version table read the table (for example
`sql.PostgresImage("POSTGRES_17")` or `gke.DefaultVersion` in the external tests), unless the
literal is the point of the test, such as `resolveVersion` cases, the expected values from GCP's
docs, or an upgrade between two named versions. These copies outside the packages stay literal,
and a refresh updates them:

- `.github/workflows/ci.yml`, the `perf` job's Pre-pull images step: the `POSTGRES_17` image, and
  the `defaultVersion` k3s image with its preloads.
- `setup-gcpemu/test/check.sh` and `setup-gcpemu/src/lib.test.js`: postgres image tags.
- `services/build/agent_test.go`: `cloudSDK`.
- `compat/clients_test.go` (the psql client image), `compat/kubeconformance_test.go`,
  `e2e/sonobuoy_test.go`, `e2e/autopilot_test.go` and `internal/sonobuoytest`: versions in tests
  and examples in comments, when the named version is gone.
- Code comments that name a version as an example.
- README, "GKE sizing": the version the figures were measured on. Change it only when you measure
  again.

Console test fixtures are UI data; leave them alone.

To find stragglers, grep for each old version (for example `17.10-alpine`, `POSTGRES_17_10`, the
old Cloud CLI version, the old maintenance suffix and add-on versions), excluding `console/`,
`docs/adr/` and `docs/releases/`.

## Tests

```sh
just vet
just test                      # includes TestServerConfigChannels, TestInsertReportsInstalledVersions,
                               # TestRefreshCountsAsMaintenance
go run ./internal/devtool product-images   # the images the weekly scan will see
just compat                    # psql, gcloud and kubectl against the new images
just kube-conformance          # Sonobuoy at each minor's newest patch
just e2e
```

CI's GKE e2e and conformance jobs and the Cloud SQL tests must pass on the new images before
merging.
