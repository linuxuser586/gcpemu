# The Admin kubeconfig is not a user on Autopilot clusters

An Autopilot cluster refuses writes to `kube-system` by users, as GKE does (FR-GKE-016), and lets the cluster's own components write there. In the emulator, three things share the `system:admin` identity of k3s's admin client certificate: k3s's in-server controllers, which apply the manifests of `kube-system`'s components; the emulator, which installs add-ons there; and the Admin kubeconfig. We decided that `system:admin` counts as the cluster itself, so the refusal applies to Google identities and to service accounts of other namespaces, while the Admin kubeconfig can still write to `kube-system`. Refusing it would refuse k3s's own controllers and break the cluster.

## Considered options

- **Give the Admin kubeconfig its own client certificate**, minted from the cluster's client CA, and refuse it like a user. This is the faithful option, and it may still be made. It was not done here because it changes every cluster's kubeconfig, Standard clusters included, and needs that certificate kept across restarts and rotations.
- **Tell the emulator's writes apart by `fieldManager`**, refusing other `system:admin` writes. This was rejected because k3s's controllers write as `system:admin` with field managers the emulator does not control, so they would be refused.

## Consequences

- A user working through the Admin kubeconfig can create and change objects in `kube-system` on an Autopilot cluster, which GKE never allows. Users authenticated through gcloud or the Connect gateway are refused as on GKE.
- The webhook's match condition and the emulator's check both encode the same rule, so changing who counts as the cluster means changing both.
