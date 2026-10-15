# The Host attachment links the host with VPC networks, as a development aid outside GCP

ADR 0004 promises VPC addresses only to VPC workloads, as in GCP, where a laptop cannot reach a VPC's private IPs without a VPN. Local development needs more: a process on the host stands in for a pod, so it must reach the cluster's Kubernetes Services, and pods must reach it. Consumers did this on Linux by routing the Service CIDR through a node and pointing EndpointSlices at the bridge's gateway IP, with sudo, outside the emulator's knowledge. That breaks on a VM runtime, where neither the node nor the gateway is on the host. We decided that the emulator provides both directions itself, on every host, as a development aid that is not modelled on GCP and to which Fidelity levels do not apply.

- **Outward**, host clients reach a GKE cluster's ClusterIP Services by name. The emulator's DNS answers a host query for `<service>.<namespace>.svc.<cluster>.<location>.<project>.gke.internal` with a Loopback alias, and also answers `<service>.<namespace>.svc.cluster.local` while exactly one cluster has that Service (as ADR 0013 does for forwarding rule env vars). It serves the Service's ports on that alias, through the VM relay on a VM runtime. A Service gets its alias on the first host query and keeps it until the Service is deleted. Headless Services are not answered, and ExternalName Services are answered with their CNAME.
- **Inward**, VPC workloads reach Offered ports at the Host address of a VPC container network. On a VM runtime the VM relay holds that address and forwards each Offered port to `127.0.0.1` on the host. Where the host can route to the network, the address is the gateway IP, and the emulator binds the Offered port there and forwards it the same way. Either way, a port that is not offered is not reachable, and a process on the host needs to listen only on loopback.

## Considered options

- **An L3 tunnel** that routes the Service CIDR from the host through the VM relay (a `utun` device with a userspace TCP/IP stack). ClusterIPs would work unchanged, but it needs root and a new privileged component, while consumers already address Services by name.
- **Modelling it on GCP**, with GKE's Cloud DNS in VPC scope and a VPN-like attachment of the host. VPN is out of the SRS's scope, and in GCP nobody puts a laptop process behind a pod's Service, so there is nothing faithful to copy.
- **VM runtimes only**, keeping routed ClusterIPs and the gateway IP on Linux. Consumers would keep two paths. Linux CI already forces the relay path (ADR 0005), so one mechanism is tested everywhere.
- **Every host port reachable** at the Host address, with nothing to offer. It exposes every loopback listener on the host to VPC workloads, and on a VM runtime the relay would need NAT rules to capture any port.
- **`host.docker.internal`** for the inward direction. Each runtime names and routes it differently, which ADR 0005 already rejected for the relay.

## Consequences

- Kubernetes Services draw on the same `127.0.0.2`–`127.0.0.254` pool as Cloud SQL instances and forwarding rules. When the pool is exhausted, or on macOS without `gcpemu loopback install`, the DNS answers SERVFAIL, and `gcpemu doctor` flags it.
- The Host address, Offered ports and Services' Loopback aliases persist across restarts of a Persistent Instance, because consumers write the Host address into EndpointSlices and host resolvers cache answers. On a VM runtime the Host address is reserved in the network's IPAM when the relay first joins.
- An Offered port stays until it is withdrawn. If the process behind it exits, VPC workloads get connection refused.
- On a routable host, offering a port that a process already holds on the wildcard address fails, naming that bind, rather than half-working.
