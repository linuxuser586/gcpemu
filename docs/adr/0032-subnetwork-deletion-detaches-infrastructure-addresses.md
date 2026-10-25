# Subnetwork deletion detaches Infrastructure addresses rather than waiting for them

The Edge relay joins a GKE cluster's subnetwork while the cluster has pod routes, holding an Infrastructure address there. Its route loop leaves the subnetwork a few seconds after the cluster is gone, but a client deletes the subnetwork as soon as the cluster delete completes, which found the address and refused (#683). We decided that an Infrastructure address never makes a subnetwork in use: once nothing else uses it, compute asks the service holding the address to leave, synchronously, before it deletes the subnetwork. Deleting an auto-mode network does the same for its subnetworks. A delete then behaves as on GCP, where no hidden resource holds an address.

## Considered options

- **Wait for the route loop.** The delete would block, or fail, for up to the route loop's interval plus the Edge relay's idle grace (about 7 s), and any later change to those timings would change delete latency.
- **Ignore the address and remove the network anyway.** Compute would drop the allocation, but the container runtime refuses to remove a network a container is still attached to.

## Consequences

- Compute calls into another service while it checks a delete. Services register a releaser for their own addresses; today only the load balancer does.
- The release waits for a route pass in progress, so a delete can take up to about 20 s longer when one is reading GKE's pod routes.
