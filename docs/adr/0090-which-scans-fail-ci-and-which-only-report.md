# Which scans fail CI and which only report

Until #1165 nothing scanned gcpemu for vulnerabilities or licences. The release built SPDX SBOMs that nothing read, SRS 2.4.2's licence rule was checked by hand at review, and nothing reported the CVEs of Product images (CONTEXT.md), which Renovate never proposes (ADR 0082). We decided on four checks:

| Check | Tool | Runs | On findings |
|---|---|---|---|
| Go vulnerabilities | govulncheck, on linux, darwin and windows | PRs that change `go.mod`, `go.sum` or its allowlist; weekly on main | **fail** |
| Licences of Go modules, the console's and setup-gcpemu's npm packages, and the compat suite's Maven POM | Trivy `--scanners license` | PRs that change a manifest, a lockfile or `.trivyignore.yaml` | **fail** on a licence outside the allowed families |
| Release images (gcpemu, gcpemu-echo) | Trivy image scan, HIGH and CRITICAL, fixable only | beside the Image readiness check in CI, and in release before any push | **fail** |
| Product images | Trivy image scan, HIGH and CRITICAL | weekly | **report only**: a job summary, and one rolling issue while a fixable one exists |

- **govulncheck for Go, not Trivy.** govulncheck follows the call graph, so it fails only when the code calls a vulnerable symbol, not on every vulnerable module in go.sum. It runs as `go install golang.org/x/vuln/cmd/govulncheck@<exact version>`, so go.mod gains nothing and devtool stays stdlib-only (ADR 0061). It analyses each GOOS the binary ships for, since build constraints hide platform-only calls from the others. Trivy still scans the Go binaries inside the release images, where it reports the same stdlib vulnerabilities by CVE.
- **Licences only on the Node and Maven lockfiles.** Dependabot alerts and Renovate's vulnerability PRs already cover their CVEs; a second report of the same advisories would add noise and no signal.
- **The allowed licences are SRS 2.4.2's list read as families**: Apache-2.0; MIT and its variants (MIT-0, X11); every BSD variant, 0BSD included; ISC, the OpenBSD project's licence, which is the two-clause BSD licence reworded; and MPL-2.0. An SPDX expression passes when an OR has one allowed side and an AND has all. Packages Trivy reads no licence for are listed as warnings, since Trivy reports no finding that an exception could name. A lockfile Trivy reads no licence from at all fails, because its dependencies were not installed and nothing was checked. Dev dependencies are left out, as Trivy does by default: SRS 2.4.2 is about what gcpemu ships.
- **Trivy is installed as a binary checked against a per-platform sha256**, by `.github/install-trivy.sh`, like OpenTofu. Never `aquasecurity/trivy-action` or `setup-trivy`: their tags were hijacked in March 2026, and a mutable action tag in a release job is a supply-chain path into signed images. Renovate lists new Trivy versions as a report-only checksum pin. devtool reads the script's version as the one the local recipes require.
- **Exceptions expire.** Trivy findings are suppressed only in `.trivyignore.yaml`, govulncheck findings only in `.govulncheck-allow.json`. Each entry names a gcpemu issue and an expiry date. An entry past its date, or without either, fails its check, which devtool checks before running Trivy or after running govulncheck. `.trivyignore.yaml` stays in block-style YAML, the subset stdlib-only devtool reads.
- **A Product image finding never fails CI.** Refreshing a Product image changes what the emulator promises, so it is a deliberate change (#895), not something a CVE published overnight should force into an unrelated PR. The weekly scan opens or updates one issue, so the maintainer triages findings into refreshes or follow-up issues.
- **The list of Product images comes from the code.** `devtool product-images` prints the values the emulator runs (Cloud SQL's postgres images, GKE's k3s, the system images it preloads and the add-on manifests' images, the Managed emulator images), and a test fails when an image in those files is missing from the output. No workflow keeps its own list.
- **Trivy's database is cached** by `actions/cache`, keyed by day and runner OS, so a runner OS normally downloads it at most once a day.

## Considered options

- **Trivy for Go vulnerabilities as well.** One tool fewer, but it reports by module version, so every vulnerable package anywhere in go.sum would fail CI whether or not gcpemu reaches it.
- **SARIF upload to GitHub code scanning.** Findings would show in the Security tab, but the job summary and the rolling issue already reach the maintainer, and code scanning adds a second place to triage.
- **Failing on Product image CVEs.** It would block unrelated PRs on a CVE published in an upstream image, and the fix, a Product image refresh, needs a deliberate change of its own.
- **The SRS list read literally** (Apache-2.0, MIT, BSD-2/3-Clause, MPL-2.0 only). ISC and 0BSD are the same terms as licences it allows, and an expiring exception for each would be renewed forever.
- **Exceptions without expiry.** Simpler, but a suppressed finding would outlive its reason unnoticed.

## Consequences

- Main starts with exceptions: Go 1.27.3's net/http (GO-2026-6859, CVE-2026-97054 in the images; #1198), hamba/avro/v2, which has no fixed version (#1199), BlueOak-1.0.0 licences under setup-gcpemu's @actions packages (#1200), and javax.annotation-api's CDDL + GPLv2 licence in the compat suite's Java POM (#1201).
- `just licence-check` needs the dependencies installed (the Go module cache, `node_modules` in console and setup-gcpemu, `~/.m2`), and Trivy reaches Maven Central for POM parents missing from `~/.m2`. Central answers a burst with 429 and blocks the address for a while, so CI resolves the POM with Maven first.
- `just doctor` does not require Trivy; the Trivy recipes name the version to install when it is missing.
- A release image built with a toolchain that has a fixed HIGH stdlib CVE fails the release until the toolchain moves or an exception is added.
- This replaces ADR 0082's licence reminder on major and replacement PRs: the licence check covers every change to a lockfile, minors and transitive dependencies included.
