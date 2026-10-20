# VPC workloads reach an external load balancer by its public address, routed to the Edge relay

Note (#527): the "Cloud NAT egress gateways" and "NAT gateway" below are the container CONTEXT.md now calls the NAT egress proxy, not a NAT gateway (a Cloud NAT configuration).

The emulated DNS answered a forwarding rule's IP with the listener that serves it on the host, usually `127.0.0.x`. Pods got the same answer, dialled their own loopback and were refused, so nothing in a cluster could call a public host behind the load balancer. That rules out third-party OIDC clients that must fetch a discovery document from the issuer URL, and webhooks to the app's own host (#187).

We decided that VPC workloads use the forwarding rule's own address, as on GCP. The DNS answers a query that carries a VPC network (GKE node DNS relays tag theirs) with the external forwarding rule's IP unmapped. The Edge relay joins the external network, holds every external forwarding rule IP on its loopback and relays their ports to the Data plane with PROXY v2, like its other listeners. Services whose Managed containers send public traffic over the external network implement `emu.PublicRouter` and add host routes for those IPs via the Edge relay. Today these are GKE nodes with external IPs and the Cloud NAT egress gateways. Private nodes therefore reach the load balancer only while a Cloud NAT covers them, and backends see the node's or the NAT's address, as they would on GCP. Host clients are unaffected. Internal forwarding rules keep today's mapping.

## Considered options

- **Answer VPC queries with an Edge relay address on the VPC network.** It needs no routes, but every forwarding rule would take an address from each subnetwork, which GCP does not do, and dialling the forwarding rule's IP, rather than a name, would still fail.
- **DNAT forwarding rule IPs in every node.** It needs the same dynamic programming as routes, and adds iptables rules in containers that k3s also manages.
- **Route the external address pools (`34.16.0.0/12`, `34.120.0.0/14`) via the Edge relay statically.** It would need no updates, but other public addresses in those pools, such as Cloud SQL public IPs, would go to the Edge relay too, and a forwarding rule can use a literal address outside them.

## Consequences

- The Edge relay now also runs while a GKE cluster and an external forwarding rule exist together, even when no privileged port or pod backend needs it.
- Routes are programmed with `ip route ... proto 245`, so a later update finds and removes stale ones. Nodes started later get the current routes when they start.
- Under the offline sink (FR-NAT-005) the NAT gateway lets traffic to forwarding rule IPs through, because the load balancer is emulated and is not the internet.
