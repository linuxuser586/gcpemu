# A node's image GC works against its node pool's boot disk size

A GKE node's kubelet collects images at 85% of the image filesystem it sees. On GKE that is the node's boot disk. A node container's `/var/lib/rancher/k3s` is a volume on the container runtime's own disk, which under Docker Desktop is as big as the host's, so the kubelet's GC fired only once the host was already full. One machine's five node volumes held 42 GB, mostly old image tags, and the host ran out of space (#1052).

So the emulator runs image GC itself, against the node pool's boot disk: `diskSizeGb` (100 GB by default, as on GKE, counted in GiB as Compute Engine does). When a node's image storage reaches the pool's `kubeletConfig.imageGcHighThresholdPercent` (85 by default) of that size, unused images are removed, oldest-used first, until it is under `imageGcLowThresholdPercent` (80 by default). This follows the kubelet's own algorithm. The emulator records when it first saw each image and when it last saw a container use it, on the Emulator clock and in memory, so after a restart every image counts as just seen. Images are ranked by last use, then by first sight. An image first seen less than `imageMinimumGcAge` ago (2 minutes by default) is kept, so an image pulled for a container about to start survives.

A node's image storage is what containerd's snapshots use (the CRI's `imagefsinfo`, which the kubelet reads), plus the image content containerd keeps alongside (the sum of image sizes). The kubelet counts only the snapshots. On the emulator, the content takes space on the same disk, so it is counted too. The CRI does not report an image's snapshots on their own. So a removal is credited with the image's share of all image storage: its content size scaled by the ratio of image storage to content.

These images are never removed: one any container on the node was created from, running or exited, by ID, tag or digest; a pinned one; and a system image nodes import at start (the version's preload list). A private node has no route to pull a system image again.

The emulator reaches a node's images by running `crictl` in the node container. GC runs on each running cluster's nodes once a minute of real time, and from `gcpemu time advance`. In a Deterministic Instance it runs only when the clock is advanced, as the Registry cleanup pass does (ADR 0070). The kubelet's own GC is left on. On a host whose disk is smaller than the boot disk it still fires at 85% of the host's disk.

## Considered options

- **Set the kubelet's thresholds from the pool.** The kubelet measures the runtime's disk, so the thresholds would still be percentages of the host's disk.
- **Give each node a filesystem of `diskSizeGb`.** A loop device or a size-limited volume needs runtime support that Docker Desktop and Podman lack. A full node would also fail writes instead of collecting images.
- **Measure with `du` over containerd's directories.** Exact, but it walks every file of every image each minute on every node.
- **`crictl rmi --prune`.** Removes every unused image at once, including the ones the next deploy is about to use again.

## Consequences

- A node that held more than its boot disk's high threshold before this change shrinks on the first pass.
- An image last used before a restart may be removed before one that was never used. Each is first seen when the emulator starts, as with a kubelet restart.
- Freed space is estimated, so a pass can remove one image more or fewer than needed. Snapshot usage includes containers' writable layers, which removing images cannot free. A node held over the threshold by those keeps losing its unused images, one pass at a time. The kubelet does the same.
- In a Deterministic Instance, the minimum age counts on the Emulator clock from the first pass that saw an image. Two clock advances longer than it can remove an image that was pulled for a container not yet created.
