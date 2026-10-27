# Hosts reach VMs through IAP tunnels, not external IPs

A VM's external IP, from its `accessConfigs`, cannot be routed from the host on a VM runtime, and on Linux it would need an address the host does not own. We decided that the external IP is Recorded (allocated, reported and never dialable), and that host clients reach a VM's ports through an IAP tunnel: the Instance serves Identity-Aware Proxy's TCP forwarding (`tunnel.cloudproxy.app`) and carries each connection to the VM, through the VM relay where there is one, checking `iap.tunnelInstances.accessViaIAP`. On Linux without the relay, the host can also dial a VM's internal IP, as it can a GKE node's.

## Considered options

- **Mapping the external IP to a host listener per port**, as forwarding rules are (FR-LB-002). A VM has no declared ports, so the emulator would have to guess which ones to bind or bind all of them, and the address it reports would not be the external IP the API returns.
- **A `gcpemu ssh` command.** It is less work, but it is not a GCP interface, so scripts using it would not run against GCP unchanged.

## Consequences

- `gcloud compute ssh --tunnel-through-iap` and `gcloud compute start-iap-tunnel` work on every platform; `gcloud compute ssh` without the flag works only on Linux without the relay.
- The emulator implements IAP's websocket relay protocol, a wire format it did not serve before.
