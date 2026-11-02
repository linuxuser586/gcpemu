# The External address pools never leave the emulated network

ADR 0055 kept a route for every external forwarding rule's address, but no other address the emulator hands out as public: a reserved external address no forwarding rule uses, a NAT IP, or a forwarding rule's ephemeral address after the rule is deleted. A pod that dialled one went out through the NAT egress proxy, or a node's external IP, to whoever owns it on the internet, where on GCP nothing would answer (#917). We decided that every NAT egress proxy and every GKE node with an external IP holds `unreachable` Public routes for the External address pools, `34.16.0.0/12` and `34.120.0.0/14`, from the moment it starts, and a /32 `unreachable` route for each reserved external address outside them while it is reserved. Compute is their only source; the load balancer's routes for forwarding rule addresses are unchanged and, being more specific, still win.

## Considered options

- **A route for each external address compute holds.** It keeps real hosts in those ranges reachable, but an address no resource holds any more, such as a deleted forwarding rule's ephemeral one that a pod cached, still leaves for the internet.
- **The load balancer sending compute's addresses with its own.** One sender of Public routes, but the protection would hang on the load balancer having run.
- **Routing the pools via the Edge relay (ADR 0007).** Rejected there because Cloud SQL public IPs were in the pools; they no longer are (Public IPs, Public endpoints and nodes' external IPs are addresses on the External network), but unreachable is still the right answer for an address nothing serves.

## Consequences

- VPC workloads cannot reach real hosts in `34.16.0.0/12` or `34.120.0.0/14`. Clients on the host are not covered: the emulator does not route the host.
- With the Offline sink on, the NAT egress proxy leaves the pools and the reserved addresses outside them to their Public routes, so a client fails at once rather than being recorded as internet egress.
- A reserved external address outside the pools is protected only while it is reserved.
- Inside the pools, a forwarding rule's own `unreachable` route (ADR 0055) is redundant; it stays, for forwarding rules with a literal address outside the pools.
- External addresses are IPv4 only, so there are no IPv6 pool routes.
