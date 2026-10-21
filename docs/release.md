# Release checklist

CI runs the VM relay path on Linux (the `vm-relay` job, `GCPEMU_VM_RELAY=1`), but hosted macOS
runners have no container runtime, and hosted Windows runners run only Windows containers.
Before tagging `vX.Y.Z`, run the Pre-release host checks below on macOS and on Windows.

## Release notes

The release lists the merged pull requests on its own. For a release that users need to act on,
commit `docs/releases/vX.Y.Z.md` before tagging: the workflow puts it above that list.

## Image readiness check

Before it pushes any image tag, the release workflow builds the amd64 image from the release's
binaries and runs it with the runner's Docker socket. The image must report healthy within 120
seconds. If it doesn't, the GitHub release and its binaries stay published, but neither Docker Hub
nor GHCR gets any of that release's tags, `latest` included. The job prints the container's logs
and its last health-check output. Fix the cause and release it under a new patch tag.

To run the check locally after `task release`, on a host with Docker:

```sh
docker build --build-arg TARGETARCH=amd64 -t gcpemu:check -f Dockerfile dist && task image-check IMAGE=gcpemu:check
```

## Signing

The release job signs `SHA256SUMS` with keyless cosign (`SHA256SUMS.sigstore.json`), writes an
SPDX SBOM for each binary (`gcpemu-<os>-<arch>.spdx.json`), and records SLSA provenance and the
SBOM as GitHub attestations for each binary, all before it publishes anything. If any of those
steps fails, there is no GitHub release and the major tag doesn't move. Re-run the workflow once
the cause is fixed, or, if the fix needs a code change, release it under a new patch tag.

The image job signs the pushed manifest list by digest, in Docker Hub and in GHCR, right after the
push. If signing fails, the tags are already pushed but unsigned, and `cosign verify` fails for
them until the job is re-run. Re-run the image job on its own: it pushes the tags again and signs
the digest they then name. Re-running it is safe even after a partial signing.

The signer is `https://github.com/linuxuser586/gcpemu/.github/workflows/release.yml@refs/tags/vX.Y.Z`
with issuer `https://token.actions.githubusercontent.com`. To verify a release, with
[cosign](https://docs.sigstore.dev/cosign/system_config/installation/) and the
[GitHub CLI](https://cli.github.com/), in a directory holding the downloaded `SHA256SUMS`,
`SHA256SUMS.sigstore.json` and binaries:

```sh
cosign verify-blob --bundle SHA256SUMS.sigstore.json \
  --certificate-identity-regexp '^https://github.com/linuxuser586/gcpemu/.github/workflows/release.yml@refs/tags/v' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com SHA256SUMS
sha256sum -c --ignore-missing SHA256SUMS
gh attestation verify gcpemu-linux-amd64 --repo linuxuser586/gcpemu
gh attestation verify gcpemu-linux-amd64 --repo linuxuser586/gcpemu --predicate-type https://spdx.dev/Document/v2.3
```

The last command checks the SBOM attestation rather than the provenance. For the image (replace
`X.Y.Z` with the version):

```sh
cosign verify \
  --certificate-identity-regexp '^https://github.com/linuxuser586/gcpemu/.github/workflows/release.yml@refs/tags/v' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com docker.io/linuxuser586/gcpemu:X.Y.Z
cosign verify \
  --certificate-identity-regexp '^https://github.com/linuxuser586/gcpemu/.github/workflows/release.yml@refs/tags/v' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com ghcr.io/linuxuser586/gcpemu:X.Y.Z
```

## Pre-release host checks

Run `task prerelease` on macOS (Apple Silicon) and on Windows from a clean checkout of the commit
to be tagged. It runs every check below on each runtime, one after the other, through its docker
context, and confirms each choice with `gcpemu doctor`. By default it checks the Required runtimes:
**Docker Desktop** (`desktop-linux`) and **Colima** (`colima`) on macOS, and **Docker Desktop** in
Linux-containers mode on Windows. A Required runtime that isn't installed or running fails the run.
OrbStack and Podman machine are best-effort: name them when one is at hand, and a release doesn't
wait for them:

```sh
task prerelease RUNTIMES=desktop-linux,colima,orbstack
```

It ends with a table of runtime × check (pass, fail or skip) and each runtime's version, which it
also writes to `dist/prerelease-<os>-<commit>.md`. Put it in the release PR. A check that's
skipped didn't run: only the checks that depend on the host say why they skip, such as 5432 being
taken on Windows.

The real `gcloud`, `kubectl` and, on Windows, `cloud-sql-proxy` drive the emulator. The checks
connect to PostgreSQL and HTTP themselves, so `psql` and `curl` aren't needed. Each check runs
its own Ephemeral Instance, or uses its own names in the container, and cleans up after itself
even when it fails.

### macOS

Needs `go`, `task`, `docker`, `gcloud` with `gke-gcloud-auth-plugin`, `kubectl`, and OpenTofu for
the Reference stack. `task prerelease` builds `bin/gcpemu` and asks for your password once
(`sudo -v`) for `gcpemu loopback install`.

1. **Reference stack:** `task e2e`. The Reference stack and the Containerized Instance tests pass,
   and the Reference stack finishes within its 10-minute budget. See "Reference stack (SRS 11.2)"
   in the README.
2. **Two Cloud SQL instances at once:** with the loopback aliases installed (and taken out again
   if the check added them), `GCPEMU_SQL_ONE` and `GCPEMU_SQL_TWO` are on different `127.0.0.x`
   addresses, and both connections answer `select 1` while both are open.
3. **A GKE `LoadBalancer` Service from the host:** the Service's EXTERNAL-IP is a loopback
   address, and nginx answers at `GCPEMU_GKE_LB_DEFAULT_WEB_80` on that address.
4. **VLAN (8021q) support:** the Translated network tests pass on a runtime with VLAN support. On
   one without it they fail with an error that names the runtime and says it has no VLAN
   support, and the check passes too. Any other failure, or a skip, fails the check. The result
   and the runtime's version go into the runtime table under "Container runtime" in the README.
   Commit that change before tagging.
5. **Host attachment:** see below.

The suite also runs on Linux, but only for the runtimes `RUNTIMES` names, so it can be worked on
with Docker Desktop for Linux.

### Windows (Containerized Instance)

Needs `docker`, `gcloud`, `kubectl` and `cloud-sql-proxy`. To build the image it also needs `go`,
`task`, Node and pnpm: `task prerelease` runs `task release` and builds `gcpemu:rc` from `dist/`.
`IMAGE=<ref>` uses an image already built instead. The container publishes `127.0.0.1:4510` and
`127.0.0.1:20000-20019`, so those ports must be free. With Podman machine, the container also
gets `GCPEMU_VM_RUNTIME=true`.

1. **Start:** in the container, `doctor` reports the VM-runtime decision as on (Docker Desktop
   recognized) and the loopback-alias decision as on. The API gateway that
   `gcpemu env --shell powershell` prints answers the host.
2. **Cloud SQL from the host:** `cloud-sql-proxy`, with the static `--token owner` and without
   `GOOGLE_CLOUD_UNIVERSE_DOMAIN`, serves `select 1`. The instance's `PRIMARY` address is a
   `127.0.0.x` of its own, and while 5432 is free on the host a connection straight to it on 5432
   works too.
3. **A GKE `LoadBalancer` Service from the host:** with the kubeconfig from
   `/container/_emu/kubeconfig`, the Service's EXTERNAL-IP is `127.0.0.1`, and nginx answers at
   `GCPEMU_GKE_LB_DEFAULT_WEB_80` on a port of the published range.
4. **A forwarding rule from the host:** a global forwarding rule on port 8080, in front of a
   backend bucket, has `GCPEMU_LB_SITE` on `127.0.0.1` and a port of the published range, and
   `index.html` there reads `lb ok`.
5. **Host attachment:** see below. It runs on the host, not in the container, so it needs `go`
   even with `IMAGE=<ref>`.

### Host attachment (macOS and Windows)

The last check on each runtime runs the Host attachment tests (ADR 0014) with `go test`, against
that runtime: `TestHostAttachmentOffers` and `TestHostAttachmentSurvivesRestart` (a VPC workload
reaches a host listener at its subnetwork's Host address) in `services/compute`, and
`TestHostServices` (host clients reach a cluster's Services at Loopback aliases) in `services/gke`.
On macOS it runs with the Loopback aliases installed, as check 2 does. Any failure fails the check,
and so does a skip: the outward test skips without Loopback aliases, which never happens on Windows.
`TestOfferHeldOnWildcard` runs too and skips on every VM runtime, since the VM relay holds the Host
address.

### What stays manual

Installing the runtimes, and committing the README's runtime table.
