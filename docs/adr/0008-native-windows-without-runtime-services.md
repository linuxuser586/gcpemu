# Native Windows runs every Service except the Runtime Services, which stay on WSL2

Superseded by ADR 0015 (`0015-native-windows-runs-the-runtime-services-on-a-vm-runtime.md`).

The SRS supported Windows through WSL2 only. We decided to ship and support a native `gcpemu.exe` as well, and that on native Windows an Instance treats the container runtime as unusable. By default it skips the Runtime Services, saying that they need WSL2, and it refuses to start when they are requested explicitly. Host mode is not supported there either. Everything else, the load balancer included, runs in-process. Windows has no privileged ports, so forwarding rules need no Edge relay to bind them.

Native Windows treats the runtime as unusable because Docker Desktop on Windows is a VM runtime: its bridge networks are inside the WSL2 VM, and the host cannot route to them. The VM relay (ADR 0005) solves that, but it is not built yet. Supporting the Runtime Services natively waits for the relay, and then needs only its host side and a named-pipe runtime endpoint.

## Considered options

- **Keeping WSL2 as the only path.** It needs no code, but Windows users who need only GCS, Pub/Sub or Secret Manager would have to install a Linux distribution. The build had also broken unnoticed, because no CI job compiled it for Windows.
- **Waiting to support every Service natively until the VM relay exists.** That delays the in-process Services, which need nothing from the relay, for no gain.

## Consequences

- Windows is to be tested in CI on `windows-latest` (unit tests and in-process client compatibility, #207). The `platforms` job already runs the Makefile there, and the Linux jobs cross-build `GOOS=windows`. Releases include `windows/amd64` and `windows/arm64`.
- State lives under `%LOCALAPPDATA%\gcpemu` unless `$XDG_STATE_HOME` is set. File modes such as 0600 are not enforced on Windows, and the per-user ACL of that directory protects the CA key and the Admin token.
