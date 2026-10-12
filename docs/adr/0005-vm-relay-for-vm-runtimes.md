# On a VM runtime, one VM relay carries all traffic between the host and Managed containers

The SRS left GKE out on macOS, and every container-backed Service assumed the host can route to, and bind addresses on, the runtime's bridge networks. That holds on Linux, but on a VM runtime (Docker Desktop, Colima, OrbStack, Podman machine) those networks live inside a virtual machine. We decided to support every Service on macOS, GKE included, and that an Instance on a VM runtime starts one VM relay: a Managed container joined to every network the Instance uses, which publishes a single control port to the host. The netplane forwarders that Managed containers dial run inside the relay instead of on the bridge gateway, and the emulator's own dials to container IPs (the Kubernetes API, the Edge relay, Cloud SQL's agent, health checks) go through it. Service code keeps addressing containers by their bridge IPs; only the dialer and the listener change.

The relay is used when a probe at start finds the bridge networks unreachable from the host, so Docker Desktop for Linux is treated as a VM runtime too. `GCPEMU_VM_RELAY=1` forces it and `=0` disables it, and Linux CI forces it so that the relay path is tested where a container runtime is available, which hosted macOS runners are not. When the emulator itself runs in a container on a VM runtime (ADR 0003), containers already reach it directly, and the relay only publishes the ports host clients use.

## Considered options

- **A second code path at every place that dials or reports a bridge IP**, using `host.docker.internal` inward and published ports outward, as ADR 0003 did for running in a container. That is about fifteen paths across GKE, the load balancer, the Edge relay, Cloud SQL, Cloud NAT and host mode, and every new Service would add more.
- **Requiring the emulator to run in a container on macOS** for the container-backed Services. It is the least code, since that mode already works, but the native binary would cover only the in-process Services, and the SRS promises it everywhere.
- **Keeping GKE Linux-only**, as the SRS had it. The relay is needed for Cloud SQL, the load balancer and Cloud NAT anyway, and once it exists GKE costs little more.

## Consequences

- Host clients cannot reach bridge IPs, so on a VM runtime the addresses an Instance reports for host use become `127.0.0.1` and a published port: the GKE cluster `endpoint` (with `127.0.0.1` in the API server's certificate), LB forwarding rules and `LoadBalancer` Services (their own port, or a free one when it is taken), and Cloud SQL, where every instance shares `127.0.0.1:3307` and connections are routed by the TLS server name the connector sends. Fidelity levels do not change; only the addresses do.
- Translated networks (ADR 0004) need VLAN support in the VM's kernel. The relay checks for it, and without it only Translated networks fail.
- Managed containers need a linux gcpemu binary for the VM's architecture. On a non-Linux host it is copied out of the published image for the same version and cached in the data directory, unless `GCPEMU_AGENT_BINARY` is set.
- Host mode remains Linux-only: its blocker on macOS is the host's DNS resolver, which the relay does not solve.
