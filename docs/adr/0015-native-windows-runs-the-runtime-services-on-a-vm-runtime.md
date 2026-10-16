# Native Windows runs the Runtime Services on a VM runtime, without WSL2

Supersedes ADR 0008.

ADR 0008 treated the container runtime as unusable on native Windows. The reason was that Docker Desktop's bridge networks live inside its WSL2 VM, and the host cannot route to them. It said native support would wait for the VM relay's host side (ADR 0005) and a named-pipe runtime endpoint. The relay now exists, and its host side is plain TCP and yamux over the Docker API, with nothing tied to an OS. We decided that Docker Desktop and Podman machine on Windows are VM runtimes, like their macOS counterparts. `gcpemu.exe` then runs Cloud SQL, GKE and Cloud NAT through the relay, and users never install or enter a WSL2 distribution. WSL2 may still be what backs the runtime's VM, but that is the runtime's business, not the emulator's.

- The runtime client dials `npipe://` endpoints with `github.com/Microsoft/go-winio`, which only Windows builds link.
- Detection keeps the order used on every OS: `DOCKER_HOST`, then the Docker context, then well-known endpoints. On Windows, the well-known endpoints are `//./pipe/dockerDesktopLinuxEngine`, `//./pipe/docker_engine` and `//./pipe/podman-machine-default`.
- On every OS, an endpoint whose daemon reports an `OSType` other than `linux` is unusable, because the Managed containers' images are Linux images.
  - If it was named explicitly, start fails and tells the user to switch to Linux containers.
  - If it was found by probing, detection moves on to the next endpoint.
- Host mode stays Linux-only. It needs the host to route to the container networks and to point its resolver at them, and native Windows can do neither.

## Considered options

- **Keeping ADR 0008, with WSL2 for the Runtime Services.** Users who need GKE or Cloud SQL would still have to install a distribution and run the Linux binary inside it. The only blockers left were a gate on the OS name and a missing dialer.
- **A dialer of our own on `golang.org/x/sys/windows`** instead of go-winio. That avoids a dependency, but deadlines, cancellation and pipe-busy retries on attached exec streams are where hand-written pipe code goes wrong. Docker, Podman and containerd all use go-winio.
- **Windows containers, or HCS without a Linux VM.** k3s, Postgres and the Sidecar agent are Linux-only.
- **Shipping behind an opt-in flag.** The gate lifts only once the runtime-backed suites pass natively on a Windows host. A flag would add nothing except something to retire later.

## Consequences

- On Windows, a bind refused with WSAEACCES means the port is in one of Windows' excluded port ranges, not that it is privileged. A forwarding rule on such a port fails as unavailable, pointing at `netsh int ipv4 show excludedportrange`, and is never sent to the Edge relay. Windows still has no privileged ports, so the Edge relay is used there only to reach backends the host cannot route to.
- Hosted `windows-*` CI runners have Docker Engine for Windows containers only. CI covers the named-pipe transport and the refusal of Windows-container runtimes. The runtime-backed suites are checked on a Windows host with Docker Desktop until a self-hosted runner is worth having.
- Translated networks need VLAN (8021q) support in the runtime's VM. Docker Desktop's WSL2 kernel has it.
