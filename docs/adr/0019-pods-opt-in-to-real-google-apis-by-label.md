# Pods opt in to real Google APIs by label

GKE pods reach emulated Services by their real hostnames. The node's DNS relay answers `storage.googleapis.com` and the other served names with 169.254.169.254, and the CA injection webhook makes pods trust the instance CA (FR-INT-007). Containers that a pod starts itself, such as the ones BuildKit runs for a Dockerfile's `RUN` lines, inherit the pod's DNS but not its mounts, so they fail TLS against the Google front end. The commonest victim is `go mod download`: proxy.golang.org redirects every download to the `proxy-golang-org-prod` bucket on `storage.googleapis.com` (issue #427). That bucket doesn't exist in the Instance, so trusting the CA would only swap the TLS failure for a 404. We decided that a pod labelled `gcpemu.dev/googleapis=real` gets `dnsPolicy: None` and a nameserver on the node (169.254.169.253). That nameserver resolves `*.googleapis.com` through Cloud DNS on the node's network, which gives real Google as on GCP, and every other name through the cluster's DNS.

## Considered options

- **Answer only the hosts and paths the Instance serves**, as the issue first asked. DNS sees no paths, and TLS fails before a path is sent. A bucket the Instance doesn't have therefore can't be told apart from one it has on the same host.
- **Read buckets the Instance doesn't have through from real GCS**, and document how to give nested containers the CA (a runc wrapper for BuildKit, or a Dockerfile secret mount). This would fix plain pods too. But the Instance would then call real Google on a client's behalf, and every builder would still need its own CA plumbing.
- **Documentation only**, pointing builders at public resolvers (`buildkitd.toml` `[dns]`). This works, but each builder has to repeat what one label does, and Cloud DNS private zones stop applying.

## Consequences

- Every `googleapis.com` call from a labelled pod is a deliberate Escaped request, so Workload Identity tokens the Instance issues don't work there. The label suits public data and anonymous calls, and on private nodes the calls need Cloud NAT. `LOCATION-docker.pkg.dev` stays emulated, so a builder still pushes to emulated Artifact Registry.
- Cloud DNS private zones bound to the cluster's network still apply to labelled pods. A private `googleapis.com` zone (Private Google Access) therefore behaves as on GCP.
- The label works only on pods the webhook admits: not in `kube-system`, and not with `gcpemu.dev/inject=disabled`. A pod that sets `dnsPolicy: None` keeps its own DNS. A `dnsConfig` of its own is merged in, the way kubelet merges one under `ClusterFirst`.
- The resolver finds the cluster's DNS at the tenth address of the Service range. k3s puts it there because the emulator passes no `--cluster-dns`.
