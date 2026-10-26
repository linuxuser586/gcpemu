# Autopilot nodes advertise GKE's machine shape, not the host's capacity

An Autopilot cluster adds a node when a pod is Pending for lack of capacity (FR-GKE-015). A node container sees the whole host, and a Standard node's machine type is only a label, so every node would report the host's CPU and memory as allocatable and a node would never be full. We decided that each Autopilot node advertises the allocatable CPU, memory and pods of the machine shape GKE would choose, through kubelet reservations, whatever the host has. A test that fills one node then behaves the same on a laptop and in CI, and scale-up happens where it would on GCP.

## Considered options

- **A fraction of the host** per node. It never overcommits, but where scale-up happens depends on the machine, so tests that count nodes differ between a laptop and CI.
- **A pod count per node.** Simple, but Autopilot scales on resource requests, so a pod with large requests would not trigger a node.

## Consequences

- Nodes can promise more than the host has. A cluster that is full by GCP's measure may still fit on the host, and one that fits by GCP's measure can still exhaust the host.
- Standard nodes are unchanged: their machine type stays Recorded.
- Reservations can only lower what a node advertises, so a host smaller than a shape advertises what it has. The guarantee holds for the shapes the host can hold; a pod that only a larger shape fits gets no node on that host.
