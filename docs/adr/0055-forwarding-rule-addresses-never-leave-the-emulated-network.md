# Forwarding rule addresses never leave the emulated network

VPC workloads reach an external forwarding rule by its own address through routes via the Edge relay (ADR 0007). The load balancer removed those routes whenever the Edge relay was not serving the addresses, and once kept them removed after a failed update: traffic from pods to the forwarding rule's address then left through Cloud NAT to the internet, where a real server owns `34.120.0.1` and answered with its own certificate (#912). The emulator's forwarding rule addresses are real public addresses that belong to someone else. We decided that while an external forwarding rule exists, the containers that route public traffic (`emu.PublicRouter`) always hold a route for its address: via the Edge relay while it serves the address on the External network, and `unreachable` otherwise. Only a forwarding rule's deletion removes its route. A failed update is sent again on the next pass, and the routes are sent again whenever the Edge relay is recreated.

## Considered options

- **Removing the routes while the Edge relay cannot serve them (ADR 0007 as it was).** Pods' traffic, and the SNI of every TLS connection, goes to whoever owns the address on the internet for as long as the gap lasts.
- **`blackhole` routes.** Packets are dropped silently, so clients hang until they time out instead of failing at once and retrying.
- **Routing the external address pools statically.** Rejected in ADR 0007: other public addresses in the pools, such as Cloud SQL public IPs, would go to the Edge relay too.

## Consequences

- `emu.PublicRouter.RoutePublic` has three meanings: an address to route via names the Edge relay; no address to route via, with addresses, asks for `unreachable` routes; no addresses removes every route tagged `proto 245`.
- The addresses come from the load balancer's configuration on every pass, whether or not a listener serves them yet or any GKE cluster reports pod routes.
- When the Edge relay dies or cannot be updated, the routes via it stay. A route via an address no container holds fails with "no route to host" and does not leak, and the next pass repairs it.
- A reserved external address without a forwarding rule is not covered: traffic to it still goes to the internet.
