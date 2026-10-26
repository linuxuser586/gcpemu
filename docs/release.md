# Release checklist

CI runs the VM relay path on Linux (the `vm-relay` job, `GCPEMU_VM_RELAY=1`), but hosted macOS
runners have no container runtime, and hosted Windows runners run only Windows containers.
Before tagging `vX.Y.Z`, run the Pre-release host checks below on macOS and on Windows.

## Release notes

The release lists the merged pull requests on its own. For a release that users need to act on,
commit `docs/releases/vX.Y.Z.md` before tagging: the workflow puts it above that list.

## Image readiness check

Before it pushes any image tag, the release workflow checks each platform's image natively: the
amd64 image on an amd64 runner and the arm64 image on an arm64 runner. Each is built from the
release's binaries and run with its runner's Docker socket, and must report healthy within 120
seconds. If either doesn't, the GitHub release and its binaries stay published, but neither Docker
Hub nor GHCR gets any of that release's tags, `latest` included. The failing job prints the
container's logs and its last health-check output. Fix the cause and release it under a new patch
tag.

To run the check locally after `task release`, on a host with Docker and Go, for the host's architecture:

```sh
docker build --build-arg TARGETARCH="$(go env GOARCH)" -t gcpemu:check -f Dockerfile dist && task image-check IMAGE=gcpemu:check
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
Each runtime's VM needs 8 GiB of memory for the Reference stack (Colima's and Podman machine's
default 2 GiB runs out): one with less than 7 GiB as `docker info` reports it fails setup if
Required and is skipped otherwise, and a note under the table says why and how to give it more. OrbStack and Podman machine are best-effort: name them
when one is at hand, and a release doesn't wait for them:

```sh
task prerelease RUNTIMES=desktop-linux,colima,orbstack
```

It ends with a table of runtime × check (pass, fail or skip) and each runtime's version, which it
also writes to `dist/prerelease-<os>-<commit>.md`. Put it in the release PR. A check that's
skipped didn't run: only the checks that depend on the host say why they skip, such as 5432 or 53
being taken on Windows.

The real `gcloud`, `kubectl` and, on Windows, `cloud-sql-proxy` drive the emulator. The checks
connect to PostgreSQL and HTTP themselves, so `psql` and `curl` aren't needed. Each check runs
its own Ephemeral Instance, or uses its own names in the container, and cleans up after itself
even when it fails.

### macOS

Needs `go`, `task`, `docker`, `gcloud` with `gke-gcloud-auth-plugin`, `kubectl`, and OpenTofu for
the Reference stack. `task prerelease` builds `bin/gcpemu` and asks for your password once
(`sudo -v`) for `gcpemu loopback install` and `gcpemu dns install`.

1. **Reference stack:** `task e2e`. The Reference stack and the Containerized Instance tests pass,
   and the Reference stack finishes within its 10-minute budget. See "Reference stack (SRS 11.2)"
   in the README.
2. **Two Cloud SQL instances at once:** with the loopback aliases installed (and taken out again
   if the check added them), the instances' `PRIMARY` addresses are different `127.0.0.x`, and
   connections to both on port 5432 answer `select 1` while both are open.
3. **A GKE `LoadBalancer` Service from the host:** the Service's EXTERNAL-IP is a loopback
   address, and nginx answers at `GCPEMU_GKE_LB_DEFAULT_WEB_80` on that address.
4. **VLAN (8021q) support:** the Translated network tests pass on a runtime with VLAN support. On
   one without it they fail with an error that names the runtime and says it has no VLAN
   support, and the check passes too. Any other failure, or a skip, fails the check. The result
   and the runtime's version go into the runtime table under "Container runtime" in the README.
   Commit that change before tagging.
5. **Host attachment:** see below.
6. **Resolver routes:** see below. The names resolve through `dscacheutil -q host -a name`, and
   after `uninstall` the `/etc/resolver` files gcpemu wrote are gone.
7. **VM relay:** see below.

The suite also runs on Linux, but only for the runtimes `RUNTIMES` names, so it can be worked on
with Docker Desktop for Linux. Check 6 changes the host's resolver there only with
`GCPEMU_RESOLVER_TESTS=1`.

### Windows (Containerized Instance)

Needs `go`, `task`, `docker`, `gcloud`, `kubectl` and `cloud-sql-proxy`, with or without
`IMAGE=<ref>`: `task prerelease` always builds `bin/gcpemu.exe` for the checks that run on the host.
To build the image it also needs Node and pnpm: it runs `task release` and builds `gcpemu:rc` from
`dist/`. `IMAGE=<ref>` uses an image already built instead. The container publishes `127.0.0.1:4510` and
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
5. **Host attachment:** see below. It runs on the host, not in the container.
6. **Resolver routes:** see below. Unlike checks 1–4, it runs an Instance on the host,
   `gcpemu.exe`, on the same runtime, with `--port dns=53`, since NRPT can't name a port
   (ADR 0026). The names resolve through `Resolve-DnsName`, and after `uninstall` no NRPT rule
   gcpemu wrote is left. `install` and `uninstall` each show a UAC dialog: accept both. When port
   53 is in use on the host (ICS, Hyper-V or WSL's DNS proxy may hold it), the check is skipped and
   says so.
7. **VM relay:** see below. Like checks 5 and 6, it runs on the host, not in the container.

### Host attachment (macOS and Windows)

Check 5 on each runtime runs the Host attachment tests (ADR 0014) with `go test`, against
that runtime: `TestHostAttachmentOffers` and `TestHostAttachmentSurvivesRestart` (a VPC workload
reaches a host listener at its subnetwork's Host address) in `services/compute`, and
`TestHostServices` (host clients reach a cluster's Services at Loopback aliases) in `services/gke`.
On macOS it runs with the Loopback aliases installed, as check 2 does. Any failure fails the check,
and so does a skip: the outward test skips without Loopback aliases, which never happens on Windows.
`TestOfferHeldOnWildcard` runs too and skips on every VM runtime, since the VM relay holds the Host
address.

### Resolver routes (macOS and Windows)

Check 6 on each runtime starts an Ephemeral Instance of its own, `prerelease-dns`, with
Cloud DNS and GKE. It creates a public zone `prerelease.test.` with an A record, and a GKE
ClusterIP Service `web`. Then `gcpemu dns install --yes --suffix prerelease.test` adds the Resolver
routes (#608), and the host's resolver answers `www.prerelease.test` with the record's address and
`web.default.svc.cluster.local` with a Loopback alias. `gcpemu dns uninstall --yes` takes the routes
away again. The Instance is stopped and its routes uninstalled even when the check fails.

When another Instance already has Resolver routes on the host, `install` refuses, and the check
fails naming that Instance: run `gcpemu --instance <name> dns uninstall` for it first. The check
never removes routes it didn't add.

CI runs the same check on Linux (`TestResolverRoutes`, in the `compat` job), through
systemd-resolved and without the GKE Service: after `uninstall`, the `gcpemu-dnsN` link and its
routing domains are gone.

### VM relay (macOS and Windows)

CI runs the VM relay's own tests (`internal/vmrelay`) only on Linux, where `GCPEMU_VM_RELAY=1`
forces the relay on. Check 7 runs them on each runtime, where the relay is really used (#653).
With `GCPEMU_VM_RELAY` cleared, `gcpemu doctor` on the host must report the VM relay as on because
the bridge gateway isn't bindable: off, undetermined or forced fails the check. Then every test in the
package runs with `go test` against the runtime, and any failure or skip fails the check.

### What stays manual

Installing the runtimes, and committing the README's runtime table.
