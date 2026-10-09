# setup-gcpemu

GitHub Action that installs [gcpemu](../README.md), starts it in the background and exports
the environment variables that point clients at it (SRS FR-CI-001, FR-CI-002).

```yaml
jobs:
  test:
    runs-on: ubuntu-24.04
    permissions:
      contents: read
      actions: read # lets the post step tell whether the job failed
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
   `SHA256SUMS`. The binary is cached by its digest, and its directory is added to `PATH`.
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
  reads the job's steps through the API, which needs `actions: read`. Without that permission it
  uploads the artifact anyway, with a notice.
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
| `download-url` | GitHub releases | Base URL of a mirror serving `<tag>/SHA256SUMS` and `<tag>/gcpemu-<os>-<arch>` |
| `cache` | `true` | Cache the binary and container images |
| `diagnostics` | `failure` | When to upload the diagnostics artifact: `failure`, `always` or `never` |
| `artifact-name` | `gcpemu-<job>-<instance>-<os>-<arch>-<random>` | Name of the diagnostics artifact |
| `retention-days` | `7` | Retention of the diagnostics artifact |
| `token` | `github.token` | Used to resolve `latest` and to read the job's result |

Outputs: `version`, `gateway` (host:port), `data-dir`, `bin`.

Use `@v0` until v1.0.0: a `vX.Y.Z` release moves its major tag `vX`, so `@v0` follows the latest
0.x release. Pin a release such as `@v0.1.0` for reproducible builds. `version: latest` installs
the newest release, whichever major it is.

Runs on `ubuntu-24.04`, `ubuntu-24.04-arm` and macOS runners. GKE, Cloud SQL and Cloud NAT need
Docker.

## Development

The runner executes `dist/` as it is, so it is committed. After changing `src/`, run
`make action` from the repository root (it runs `npm ci`, the unit tests and the esbuild bundle)
and commit `dist/`. CI fails when `dist/` doesn't match the sources. The `action` job in
`.github/workflows/ci.yml` is the self-test. It serves the commit's binary as release
`v0.0.0-selftest`, uses the action with the fixtures in `test/` (`check.sh` asserts the
environment, seed and config), and checks the diagnostics artifact. Releases come from
`.github/workflows/release.yml` on a `vX.Y.Z` tag, which also moves the `vX` tag.
