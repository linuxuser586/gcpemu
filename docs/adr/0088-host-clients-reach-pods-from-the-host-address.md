# Host clients reach pods from the Host address

Amends ADR 0014.

ADR 0014's outward direction promised that host clients reach a GKE cluster's Kubernetes Services, but not what address the pods see them come from. The emulator relays a host client's connection from its Loopback alias to a node's ClusterIP relay, which dials the Service's ClusterIP itself, so the pod sees whatever source that node's kernel picks: the node's pod bridge address when the endpoint is on that node, the node's own address otherwise, and always from the first node that answers. Consumers that run Istio sidecars admit the host's stand-in processes with an AuthorizationPolicy on `remoteIpBlocks`, and no rule could name the host without also naming the nodes' own traffic (#1177). We decided that a pod sees a host client's connection come from the Host address of the cluster's subnetwork, the address at which the same pod reaches the host's Offered ports, so that one address means "the host" in both directions.

The emulator chooses the endpoint itself from the Service's EndpointSlices and relays through the ClusterIP relay of the node the endpoint runs on. That node's agent dials the pod from a transparent socket bound to the Host address and marked, and policy routing returns only that connection's replies to the agent, so the node's other pods still reach the real host at the Host address. Endpoint choice keeps what a host client can observe of kube-proxy: ready endpoints, or serving terminating ones when none is ready; the target port by name through the EndpointSlice; `sessionAffinity: ClientIP` with its timeout, under which every host client is one client; otherwise a random endpoint. `internalTrafficPolicy` and topology hints do not apply, since a host client is on no node.

## Considered options

- **Documenting today's source.** It varies with the relaying node and the endpoint's node, and any rule admitting it admits whatever the nodes send themselves.
- **A dedicated source address** per cluster or network. It would identify the host as well, but it is a second address for the same thing, with a new term and a new way to find it.
- **Dialling the ClusterIP from a transparent socket on any node.** kube-proxy may pick an endpoint on another node, whose replies to the Host address go to the real host, and the connection fails.
- **An L3 path** routing the Service CIDR from the host and SNATting to the Host address. ADR 0014 rejected it for needing root and a tunnel on VM runtimes.
- **Failing the connection** when a node cannot preserve the source. It would trade reachability, which every consumer relies on, for a guarantee only policy-protected pods need.

## Consequences

- The promise covers endpoints in a node's pod CIDR. `hostNetwork` pods and EndpointSlices naming VMs or the Host address itself are relayed as before, from the node.
- LoadBalancer Service ingress and other host relays into the VPC are unchanged; they model GCP paths with their own source semantics.
- A node that cannot preserve the source, for want of kernel support, relays as before; the agent logs why and `gcpemu doctor` warns that the cluster's pods see node addresses for host clients.
- A host client's first connection to a pod reserves the subnetwork's Host address, joining the VM relay to the network on a VM runtime, as `gcpemu host-address` does.
