# Renovate updates dependencies, except Product images and checksum pins

Amended by ADR 0090, whose licence check replaces the licence reminder on majors and replacements.

Until #896 no bot updated any dependency, so pins drifted until someone updated them by hand, as #894 did. We decided that Renovate, run by the hosted Mend Renovate app from `renovate.json` at the repository root, proposes dependency updates, within these rules:

- **Weekly, grouped, never merged by the bot.** It runs on Monday morning (America/New_York) with at most 5 PRs open. Patch and minor updates come as one PR per ecosystem: Go modules, console packages, setup-gcpemu packages, Maven, GitHub Actions, and Docker images. Nothing automerges. Merges already bypass red CI at times (#1109), and the helper images are exercised only by the pre-release host checks, so a person reads every update.
- **Majors behind approval.** Each major is its own PR, outside every group, monorepo groups included, opened only after the maintainer ticks it on the Dependency Dashboard. Majors and replacements carry a reminder to check the licence is Apache-2.0, MIT, BSD, ISC, MPL-2.0 or BlueOak-1.0.0 (SRS 2.4.2, read as ADR 0090 does).
- **Security first.** A vulnerability-alert PR skips the schedule, the groups and approval.
- **Pin conventions stay as they are.** Actions stay on major tags (ADR 0061), and the exact `just-version` and cosign-installer pins and distroless's tag + digest are updated in place. Renovate's own `github-actions` and `dockerfile` managers read all three exactly, so they need no custom manager. In go.mod only the `toolchain` line moves; the `go` directive stays at what `go mod tidy` requires.
- **Helper images are matched by file path, not by name.** These are the images of Managed containers that only carry traffic or run helpers, which CONTEXT.md separates from Product images. One custom manager per image (busybox, `docker:*-dind`, `alpine/git`, and the Edge relay's and NAT egress proxy's `rancher/k3s`) lists the exact files that hold its tag, tests and devtool included. GKE's k3s has the same name but lives in a file no manager lists. A Go test (`internal/devtool/renovate_test.go`) fails when a tracked file gains one of these tags without being listed, so a new use can't be left behind on the next update.
- **Product images (CONTEXT.md) are never proposed** (ADR 0084). The files that set them are in `ignorePaths`, which binds every manager, default or custom. `enabledManagers` names the managers in use, so one Renovate adds or enables later can't start reading them. The same Go test checks each such file against `ignorePaths`.
- **Checksum-pinned tools are report only.** Helm, crane, ko, the Cloud SQL Auth Proxy, Sonobuoy and OpenTofu are pinned by URL with a per-platform sha256 table that Renovate can't recompute. Custom managers find their versions, and a rule puts every update behind dashboard approval, so a new version is listed on the Dependency Dashboard and never opens a PR by itself. They are updated by hand.
- **Holds name their issue**, in each rule's `description`: `github.com/miekg/dns` stays on v1 (#901), the console's TypeScript stays below 7 (#903), and setup-gcpemu is tested on Node 24, the newest action runtime the runner accepts (#904). `codeberg.org/miekg/dns` is listed on the dashboard once it tags v1.0, which unblocks #901; no bot moves a dependency between two modules.
- **CI validates the config** when `renovate.json` changes, with `renovate-config-validator --strict` from an exact Renovate release.

## Considered options

- **Dependabot.** It covers go.mod, npm, Maven, actions and Dockerfiles, but it can't reach image tags in Go source or action inputs such as `just-version`, and it has no report-only mode for the checksum pins.
- **Matching helper images by name.** Simpler lists, but GKE's `rancher/k3s` would be bumped with the relay's, and a Product image refresh is a deliberate change (ADR 0084).
- **Letting Renovate edit the checksum pins.** Each PR would change the version and leave the sha256 table stale, and recomputing it means downloading every platform's archive.
- **Automerging patches.** Patch updates of the helper images can still break host checks that CI doesn't run.

## Consequences

- The maintainer installs the Mend Renovate app, turns on Dependabot alerts (which Renovate reads for vulnerability PRs), and leaves Dependabot's own PRs off, so Renovate is the only bot opening them.
- Approving a checksum-pinned tool on the dashboard would open a PR whose checksums don't match. Such entries are reminders, not approvals to tick.
- A new file holding a helper image's tag, or a new file that sets a Product image, needs a line in `renovate.json`; the Go test says which one is missing in the first case, but only someone adding the Product image file to its list in the test catches the second.
- The licence reminder reaches majors and replacements only. Renovate can't tell when a minor or patch update brings in a new transitive dependency through a lockfile, go.sum or the POM, so that check stays with the reviewer.
- setup-gcpemu's `runs.using: node24` isn't something Renovate manages. The #904 hold keeps CI's Node 24, which tests the action on that runtime, from moving.
- The validator's Renovate version is pinned in CI by hand; Renovate doesn't update it.
- Pins nothing manages stay manual: the OpenTofu provider ranges in `e2e/`, Istio, the compat SDK pins (`pythonPins`, `nodePins`), `hashicorp/tls`, and the googleapis commit `just generate` reads.
