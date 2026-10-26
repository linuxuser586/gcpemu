# setup-gcpemu

GitHub Action that installs [gcpemu](../README.md), starts it in the background and exports
the environment variables that point clients at it (SRS FR-CI-001, FR-CI-002, NFR-SEC-005).

```yaml
jobs:
  test:
    runs-on: ubuntu-24.04
    permissions:
      contents: read
      actions: read # lets the post step read the job's result if the runner gives none
    steps:
      - uses: actions/checkout@v7
      - uses: linuxuser586/gcpemu/setup-gcpemu@v0
        with:
          services: gcs,pubsub,sql
          seed: test/emu-seed.yaml
      - run: go test ./...                # STORAGE_EMULATOR_HOST, PUBSUB_EMULATOR_HOST, … are set
      - run: gcpemu tofu-provider --project my-project > infra/emu_provider.tf
```

The action:

1. Downloads `gcpemu-<os>-<arch>` of the requested release and checks it against the release's
   `SHA256SUMS`. Before it uses any digest from `SHA256SUMS`, it verifies the release's signature
   (see [Signature verification](#signature-verification)). The binary is cached by its digest,
   and its directory is added to `PATH`.
2. Restores the container images (Cloud SQL's PostgreSQL, GKE's k3s, …) that an earlier
   successful job with the same release, platform and services saved, then `docker load`s them.
3. Runs `gcpemu start --detach` with `services`, `seed` and `config`. It waits until every
   service is ready and the seed has been applied. Every listener picks a free port (`--port 0`),
   so parallel jobs on one runner don't collide. `CI=true` also turns on ephemeral state, JSON
   logs and instant long-running operations.
4. Exports `gcpemu env` to `$GITHUB_ENV`: `GCPEMU_GATEWAY`, `STORAGE_EMULATOR_HOST`,
   `PUBSUB_EMULATOR_HOST`, the `CLOUDSDK_API_ENDPOINT_OVERRIDES_*` gcloud settings,
   `GCPEMU_SQL_<INSTANCE>` for seeded Cloud SQL instances, and so on. It also exports
   `GCPEMU_INSTANCE` and `GCPEMU_DATA_DIR`, so later `gcpemu` commands in the job find the
   instance without flags.

The post step always runs. It does three things:

- **Diagnostics.** When the job failed, it uploads a workflow artifact with `gcpemu.log`, the
  request log (`requests.json`), the resource dump (`resources.json`), `containers.json` with
  each container's log, `status.txt` and the GKE `kubeconfig`. To tell whether the job failed it
  reads the runner's `job.status` (`failure` and `cancelled` count as failed). Only when the
  runner gives no status does it read the job's steps through the API, which needs
  `actions: read`; the API's job list can lag a running job, so it is not the first source. It
  retries 5xx, 429, network errors and timeouts (3 attempts). When the result stays unknown it
  uploads the artifact anyway, with a notice that says why: the token lacks `actions: read`
  (HTTP 401, 403 or 404), the API kept failing (for example `HTTP 502 after 3 attempts`), or the
  job could not be found among the run's jobs.
- **Image cache.** After a successful job, it saves the images it pulled.
- **Stop.** It stops the instance.

## Inputs

| Input | Default | |
| --- | --- | --- |
| `version` | `latest` | Release tag, e.g. `v1.2.0` |
| `services` | all | Comma-separated services |
| `seed` | | Seed file applied before the instance reports ready |
| `config` | | `gcpemu.yaml` config file |
| `instance` | `default` | Instance name; use one per instance when starting several in a job |
| `args` | | Extra `gcpemu start` arguments, e.g. `--iam-mode enforce` |
| `wait-timeout` | `120s` | Readiness timeout |
| `binary` | | Use this gcpemu binary instead of downloading a release |
| `download-url` | GitHub releases | Base URL of a mirror serving `<tag>/SHA256SUMS`, `<tag>/SHA256SUMS.sigstore.json` and `<tag>/gcpemu-<os>-<arch>` |
| `verify-signature` | `true` | Verify the release's Sigstore signature on `SHA256SUMS`; `false` skips it (the binary is still checked against `SHA256SUMS`) |
| `cache` | `true` | Cache the binary and container images |
| `diagnostics` | `failure` | When to upload the diagnostics artifact: `failure`, `always` or `never` |
| `artifact-name` | `gcpemu-<job>-<instance>-<os>-<arch>-<random>` | Name of the diagnostics artifact |
| `retention-days` | `7` | Retention of the diagnostics artifact |
| `token` | `github.token` | Used to resolve `latest` and, as a fallback, to read the job's result |
| `job-status` | `job.status` | Leave unset: the runner evaluates it when the post step runs |

Outputs: `version`, `gateway` (host:port), `data-dir`, `bin`.

## Signature verification

Releases are Signed releases (NFR-SEC-005). The release workflow signs `SHA256SUMS` with keyless cosign and
publishes the Sigstore bundle as `SHA256SUMS.sigstore.json`. On every run, cache hits included,
the action fetches that bundle and verifies it against the exact bytes of `SHA256SUMS`, with
Sigstore's trusted root fetched through TUF. The signer must be exactly
`https://github.com/linuxuser586/gcpemu/.github/workflows/release.yml@refs/tags/<tag>`, for
the tag being installed, issued by `https://token.actions.githubusercontent.com`. A bundle
signed for another tag, another workflow or another repository is rejected.

- **v0.10.0 and later** are always Signed releases. A missing bundle, a bad signature, a signer mismatch,
  or a failure to reach Sigstore or the release fails the step.
- **Releases before v0.10.0** are unsigned. When their bundle is missing the action logs a
  warning and checks only the `SHA256SUMS` digests, as before. When one is present, it is
  verified like any other.
- **`verify-signature: false`** skips fetching and verifying the bundle, with a warning. Use it
  on runners that can't reach Sigstore (`tuf-repo-cdn.sigstore.dev`), or with a mirror that
  doesn't serve the bundle. The binary is still checked against `SHA256SUMS`.
- **Mirrors.** A non-default `download-url` keeps verification on. The mirror must serve
  `<tag>/SHA256SUMS.sigstore.json` alongside `<tag>/SHA256SUMS`, or set `verify-signature: false`.
- **`binary`** is used as given: nothing is downloaded or verified.

Use `@v0` until v1.0.0: a `vX.Y.Z` release moves its major tag `vX`, so `@v0` follows the latest
0.x release. Pin a release such as `@v0.1.0` for reproducible builds. `version: latest` installs
the newest release, whichever major it is.

Runs on `ubuntu-24.04`, `ubuntu-24.04-arm` and `macos-latest` runners. Hosted macOS runners have
no container runtime, so only the in-process Services run there: GKE, Cloud SQL and Cloud NAT need
Docker on a Linux runner.

## Development

The runner executes `dist/` as it is, so it is committed. After changing `src/`, run
`task action` from the repository root (it runs `npm ci`, the unit tests and the esbuild bundle)
and commit `dist/`. CI fails when `dist/` doesn't match the sources. The `action` job in
`.github/workflows/ci.yml` is the self-test. It serves the commit's binary as release
`v0.0.0-selftest` (with `verify-signature: false`, since it has no bundle), uses the action with
the fixtures in `test/` (`check.sh` asserts the environment, seed and config), and checks the
diagnostics artifact. It also runs the action against the real latest release with verification
on. The `platforms` job does the same on `macos-latest` with the in-process Services and
`seed-in-process.yaml`. Releases come from `.github/workflows/release.yml` on a `vX.Y.Z` tag,
which also moves the `vX` tag.
