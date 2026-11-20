# Which scans fail CI and which only report

Until #1165 nothing scanned gcpemu for vulnerabilities or licences. The release built SPDX SBOMs that nothing read, SRS 2.4.2's licence rule was checked by hand at review, and nothing reported the CVEs of Product images (CONTEXT.md), which Renovate never proposes (ADR 0082). We decided on four checks:

| Check | Tool | Runs | On findings |
|---|---|---|---|
| Go vulnerabilities | govulncheck, on linux, darwin and windows | PRs that change `go.mod`, `go.sum` or `.govulncheck-allow.json`; in release before anything is published; weekly on main | **fail** |
| Licences of what gcpemu ships: Go modules, and the console's and setup-gcpemu's production npm packages | Trivy `--scanners license` | PRs that change a manifest, a lockfile or a licence scan exception | **fail** on a licence outside the allowed families, or on a package with none |
| Release images | Trivy image scan, HIGH and CRITICAL, fixable only | beside the Image readiness check in CI, and in release before any push | **fail** |
| Product images | Trivy image scan, HIGH and CRITICAL | weekly | **report only**: a job summary, and one rolling issue |

- **govulncheck for Go, not Trivy.** govulncheck follows the call graph, so it fails only when the code calls a vulnerable symbol, not on every vulnerable module in go.sum. It runs as `go install golang.org/x/vuln/cmd/govulncheck@<exact version>`, so go.mod gains nothing and devtool stays stdlib-only (ADR 0061). It analyses each GOOS the binary ships for, since build constraints hide platform-only calls from the others. The release runs it before publishing the binaries, so they are held to the bar its images are. Trivy still scans the Go binaries inside the Release images, where it reports the same stdlib vulnerabilities by CVE.
- **The licence rule covers what gcpemu ships.** SRS 2.4.2 is about the product's third-party dependencies, so the check reads go.mod and the production dependencies of the console, which the binary embeds, and of setup-gcpemu, whose `dist/` is committed and run by users. go.mod counts in full: Go has one module graph and cannot tell a test-only module from a shipped one. npm devDependencies are left out, as Trivy does by default, and so are the compat suite's Maven POM and client pins, which are never shipped.
- **Licences only, on the Node lockfiles.** Dependabot alerts and Renovate's vulnerability PRs already cover their CVEs; a second report of the same advisories would add noise and no signal.
- **The allowed licences are SRS 2.4.2's list read as families**: Apache-2.0; MIT and its variants (MIT-0, X11); every BSD variant, 0BSD included; ISC, the OpenBSD project's licence, which is the two-clause BSD licence reworded; and MPL-2.0. An SPDX expression passes when an OR has one allowed side and an AND has all. The SRS text is left as it is; this ADR records how it is read.
- **A package with no licence fails.** Without a licence, nothing is granted, which is worse than a disallowed licence. Trivy reports no finding for such a package, so its scan exception lives in `.licence-exceptions.json`, keyed by Trivy's exact `name@version`, so a new version is looked at again. A lockfile Trivy reads no licence from at all also fails, because its dependencies were not installed and nothing was checked.
- **Trivy is installed as a binary checked against a per-platform sha256**, by `.github/install-trivy.sh`, like OpenTofu. Never `aquasecurity/trivy-action` or `setup-trivy`: their tags were hijacked in March 2026, and a mutable action tag in a release job is a supply-chain path into signed images. Renovate lists new Trivy versions as a report-only checksum pin. devtool reads the script's version as the one the local recipes require.
- **Scan exceptions expire.** Trivy findings are suppressed only in `.trivyignore.yaml`, govulncheck findings only in `.govulncheck-allow.json`, and packages without a licence only in `.licence-exceptions.json`. Each scan exception names a gcpemu issue and an expiry date, from which (00:00 UTC) it stops holding, as Trivy applies `expired_at`. An expired one, or one without both, fails its check. `.trivyignore.yaml` stays in block-style YAML, the subset stdlib-only devtool reads.
- **A Product image finding never fails CI.** Refreshing a Product image changes what the emulator promises, so it is a deliberate change (#895), not something a CVE published overnight should force into an unrelated PR.
- **The weekly scans keep rolling issues.** Each keeps one issue open while it has something to report, updates it each week, and closes it on the first run with nothing: "Weekly scan: Product image findings and expiring scan exceptions" for fixable HIGH and CRITICAL vulnerabilities in Product images, and "Go vulnerabilities called on main" when the weekly govulncheck fails. The Product image issue is closed only after a run that scanned every image, so a partial scan is never an all-clear. It also lists every scan exception that stops holding within two weeks, since an expired Release image exception fails the scans of every PR that changes code.
- **The list of Product images comes from the code.** `devtool product-images` prints the values the emulator runs (Cloud SQL's postgres images, GKE's k3s, the system images it preloads and the add-on manifests' images, the Managed emulator images), and a test fails when an image in those files is missing from the output. No workflow keeps its own list.
- **Trivy's database is cached** by `actions/cache`, keyed by day and runner OS, so a runner OS normally downloads it at most once a day.

## Considered options

- **Trivy for Go vulnerabilities as well.** One tool fewer, but it reports by module version, so every vulnerable package anywhere in go.sum would fail CI whether or not gcpemu reaches it.
- **SARIF upload to GitHub code scanning.** Findings would show in the Security tab, but the job summary and the rolling issues already reach the maintainer, and code scanning adds a second place to triage.
- **Failing on Product image CVEs.** It would block unrelated PRs on a CVE published in an upstream image, and the fix, a Product image refresh, needs a deliberate change of its own.
- **The SRS list read literally** (Apache-2.0, MIT, BSD-2/3-Clause, MPL-2.0 only). ISC and 0BSD are the same terms as licences it allows, and an expiring exception for each would be renewed forever.
- **Licences of every dependency**, devDependencies and the compat POM included. Build and test tooling would face a rule written for the product, and Trivy fetches the POM's parents from Maven Central, which answers a burst with 429 and blocks the address for a while.
- **Packages without a licence as warnings.** Trivy can't suppress them, but a warning nobody must act on lets an unlicensed package ship.
- **Exceptions without expiry.** Simpler, but a suppressed finding would outlive its reason unnoticed.

## Consequences

- Main starts with scan exceptions: Go 1.27.3's net/http (GO-2026-6859, CVE-2026-97054 in the images; #1198), hamba/avro/v2, which has no fixed version (#1199), BlueOak-1.0.0 licences under setup-gcpemu's @actions packages (#1200), and buffers@0.1.1, which declares no licence (#1202).
- `just licence-check` needs the dependencies installed (the Go module cache, `node_modules` in console and setup-gcpemu).
- `just doctor` does not require Trivy; the Trivy recipes name the version to install when it is missing. `.github/install-trivy.sh` covers Linux and macOS, where CI runs Trivy; on Windows, install that version by hand. `devtool product-images` runs a separate package, `internal/devtool/productimages`, because the list lives in the emulator's packages and devtool itself stays stdlib-only (ADR 0061); that recipe builds most of the module.
- A Product image that cannot be scanned (a registry's rate limit, say) fails the weekly run after the others are reported.
- A new advisory against a toolchain or module the code calls stops the next release, and fails a Release image's push when its stdlib is affected, until the fix lands or a scan exception is added.
- Helper images are not scanned yet (#1203).
- This replaces ADR 0082's licence reminder on major and replacement PRs: the licence check covers every change to a lockfile, minors and transitive dependencies included.
