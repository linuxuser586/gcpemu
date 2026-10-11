# A VPC on a Fallback range keeps its Declared addresses by aliasing them onto Managed containers

When a subnetwork's or private services access allocation's Declared range overlaps another network on the host, its container network is created on a Fallback range. Until now every address on that network then came from the Fallback range, which real GCP never hands out. We decided that the emulator still allocates addresses from the Declared range and reports them, and that each Managed container holding a Declared address also gets that address on its VPC interface, with an on-link route for the Declared range and the Declared address as the source of its traffic. Docker's Fallback-range address stays on the interface for the runtime's own plumbing, and the emulator keeps dialling it internally. Workloads on the VPC reach Declared addresses directly, with no address translation, so servers see the real client address and the Declared and Fallback ranges need not have the same prefix length.

Only workloads on the VPC are promised a route to a Declared address, as in GCP, where a laptop cannot reach a VPC's private IPs without a VPN. Host processes reach resources the way they would from outside GCP: the Cloud SQL Auth Proxy, public IPs, or the Kubernetes API through the Connect gateway. Routing Declared addresses from the host would need root on the host, which rootless runtimes and CI `services:` containers do not have (ADR 0003).

The decision is made per VPC network when it is realised, and it is saved with the network. If a Managed container on it cannot be given `NET_ADMIN`, for example on some rootless runtimes, the whole network falls back to today's behaviour: Fallback-range addresses are reported, with one warning naming the network and the reason. A network never mixes the two.

The Declared address goes on a VLAN of the VPC interface rather than on the interface itself, and the egress gateway's Declared address on the same VLAN is the next hop for routes via the gateway. The host's netfilter sees untagged bridged frames (`bridge-nf-call-iptables`), and the network that holds the Declared range on the host usually masquerades its own range (`-s <range> ! -o <its bridge> -j MASQUERADE`), which would rewrite the source of untagged frames between Managed containers so that replies are lost. Tagged frames bypass it with the default `bridge-nf-filter-vlan-tagged=0`.

## Considered options

- **1:1 NAT (`NETMAP`) at the NAT egress proxy.** Traffic to the Declared range would hairpin through the proxy, which must also SNAT so replies come back through it. Servers would then see every client as the proxy. It also requires the Fallback range to have the Declared prefix length, which a runtime-picked range often does not.
- **NAT in every client container.** It has the same prefix-length constraint, and it needs `NET_ADMIN` in every container that might dial a Declared address.
- **A network namespace per Instance**, isolating VPC networks from the host's routing table. It is a large change to how the emulator and its Managed containers are run, and it gives nothing the aliasing does not, once host reachability is out of scope.
- **Refusing to start on an overlap.** It is the simplest option, but overlaps are common on a developer machine running a second Instance, where the emulator would become unusable.

## Consequences

- Containers the emulator does not manage, such as a compose service attached to a VPC bridge with `docker network connect`, have no on-link route and cannot reach Declared addresses on a network that falls back. This is not supported.
- On a host that sets `bridge-nf-filter-vlan-tagged=1`, Declared addresses overlapping a network that masquerades its range are unreachable. Overlaps with another Instance's VPC networks, which do not masquerade, still work.
- Reserved internal addresses are honoured on a network that falls back, because the emulator allocates from the Declared range itself.
- k3s nodes must advertise their Declared address as the node IP, and the NAT egress proxy's MASQUERADE rules and Cloud NAT attribution must match the Declared range as well as the Fallback range.
