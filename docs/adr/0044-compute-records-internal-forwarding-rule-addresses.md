# Compute records internal forwarding rule addresses

The load balancer stores forwarding rules, but compute hands out every other internal address on a subnetwork, and an ephemeral internal forwarding rule's address was invisible to it: a Managed container joining later could be given the same address, and an ephemeral forwarding rule could take one a container held (#825). We decided that compute records each internal forwarding rule's address, keyed by the rule's subnetwork, in the same transaction in which it picks an ephemeral one, and that every internal address allocation on that subnetwork skips the recorded addresses. Compute is then the one authority on which internal addresses in a subnetwork are taken, as GCP is, whichever service owns the resource holding them.

## Considered options

- **Compute asks the load balancer for its forwarding rules' addresses** on each allocation. Nothing is stored twice, but the question can't be asked inside compute's store transaction, and the load balancer stores a rule only after compute picked its address, so an allocation in between misses it.
- **Recording them in compute's allocations for the container network.** Those are keyed by VPC container network, which may not exist yet when the forwarding rule is created.

## Consequences

- The load balancer tells compute when a rule is deleted or its address changes, and replaces the whole set at startup, so a crash between the two writes leaves no stale record.
- A rule that names an internal Address records nothing: the Address already holds the IP.
- A record never keeps its subnetwork from being deleted; the forwarding rule's own reference already does, and the record goes with the subnetwork.
- A global internal forwarding rule records against its subnetwork like a regional one.
