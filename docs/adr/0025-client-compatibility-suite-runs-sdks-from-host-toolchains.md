# The Client compatibility suite runs SDKs from host toolchains, not containers, and CI requires them

IF-007 and IF-008 ask for the `gcloud storage` CLI, an S3 client, and the Node, Python and Java client libraries to work against an Instance with only endpoint overrides. The Client compatibility suite runs every one of them from the host: Python from a venv and Node from npm, both in the compat cache at pinned versions; Java from a small Maven project pinned to one `libraries-bom`, on JDK 21; and gcloud and the `aws` CLI from PATH. Each runs behind the Escape guard. Locally, a client whose toolchain is missing is skipped, as before. In the Linux CI jobs, `GCPEMU_COMPAT_REQUIRE=1` turns that skip into a failure, so a broken setup step can't quietly drop IF-007 or IF-008 coverage. The Windows compat job runs whichever of these clients are present but requires none.

Running from the host keeps the suite on native Windows, which has no container runtime (ADR 0008). The Escape guard catches traffic through `HTTPS_PROXY`, which a client in its own container network wouldn't reach. The pip, npm and Maven caches also make CI cheaper than building an image per language. The cost is that the suite is less hermetic: a host's Python or JDK can differ from CI's, and the version pins are what keep runs reproducible.

## Considered options

- **One container image per language, run with `docker run --network=host`** (what #36 first asked for). It was rejected because host networking isn't dependable on macOS Docker Desktop, it can't run on native Windows, and each image adds a build to every CI run.
- **Skip on a missing toolchain everywhere, as the suite did before.** It was rejected because CI would then report green while covering nothing.
- **`gsutil` as the IF-007 CLI.** It was dropped from the SRS because Google deprecated it in favour of `gcloud storage`, and supporting it needs its own boto configuration for endpoints and HMAC keys.
