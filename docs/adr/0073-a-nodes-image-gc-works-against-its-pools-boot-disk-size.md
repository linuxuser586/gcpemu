# A node's image GC works against its node pool's boot disk size

A GKE node's kubelet collects images at 85% of the image filesystem it sees. On GKE that is the node's boot disk. A node container's `/var/lib/rancher/k3s` is a volume on the Runtime disk, which under Docker Desktop is as big as the host's disk, so the kubelet's GC fired only once the host was already full. One machine's five node volumes held 42 GB, mostly old image tags, and the host ran out of space (#1052).

So the emulator runs image GC itself, against the node pool's boot disk: `diskSizeGb` (100 GB by default, as on GKE, counted in GiB as Compute Engine does). When a node's image storage reaches the pool's `kubeletConfig.imageGcHighThresholdPercent` (85 by default) of that size, unused images are removed, oldest-used first, until it is under `imageGcLowThresholdPercent` (80 by default). This follows the kubelet's own algorithm. The emulator records when it first saw each image and when it last saw a container use it, on the Emulator clock and in memory, so after a restart every image counts as just seen. Images are ranked by last use, then by first sight. An image first seen less than `imageMinimumGcAge` ago (2 minutes by default) is kept, so an image pulled for a container about to start survives.

A node's image storage is what containerd's snapshots use (the CRI's `imagefsinfo`, which the kubelet reads), plus the image content containerd keeps alongside (the sum of image sizes). The kubelet counts only the snapshots. On the emulator, the content takes space on the same disk, so it is counted too. The CRI does not report an image's snapshots on their own. So a removal is credited with the image's share of all image storage: its content size scaled by the ratio of image storage to content.

These images are never removed: one any container on the node was created from, running or exited, by ID, tag or digest; a pinned one; and a system image nodes import at start (the version's preload list). A private node has no route to pull a system image again.

The emulator reaches a node's images by running `crictl` in the node container. GC runs on each running cluster's nodes once a minute of real time, and from `gcpemu time advance`. In a Deterministic Instance it runs only when the clock is advanced, as the Registry cleanup pass does (ADR 0070). The kubelet's own GC is left on.

## The free-space floor

Each node's boot disk caps only that node. A pool's nodes all keep their volumes on the one Runtime disk, each with its own copy of the same images, so five nodes at the 100 GB default may hold 425 GB before any of them collects. One laptop's five nodes held 89 GB of Docker Desktop's 123 GB, the host went down to 116 MiB free, and every node exited (#1227).

So GC also has a floor of free space on the Runtime disk: the Instance setting `gkeImageGcFreeSpace` (`--gke-image-gc-free-space`, `GCPEMU_GKE_IMAGE_GC_FREE_SPACE`), a size such as `10GiB` or a byte count, 10 GiB by default, with `0` turning it off. A value that is not a size stops `gcpemu start`. After the boot-disk passes, the emulator reads the Runtime disk's free space with `df` over the k3s data directory in a node. Every node's volume is on that disk, so one node's reading stands for all of them. When it is under the floor, the unused images of every node of every running cluster in the Instance are ranked together, by last use and then first sight, as each node's GC ranks its own. A copy of an image on each of two nodes is two entries, each ranked by its own node's use. They are removed in that order, free space being read again after each, until it is the floor plus 5 GiB, so GC does not hover at the floor. The images boot-disk GC never removes, including ones younger than the pool's `imageMinimumGcAge`, are kept here too.

A pass that ends still under the floor logs one warning, naming the free space, the floor and each node's image storage, and logs it again only after a pass has found free space back at or above the floor. An Instance's floor touches only its own nodes: two Instances on one Runtime disk each run their own. The floor runs with boot-disk GC: once a minute after a cluster's pass, and on a clock advance after every cluster's, so in a Deterministic Instance only then.

## Considered options

- **Set the kubelet's thresholds from the pool.** The kubelet measures the Runtime disk, so the thresholds would still be percentages of that whole disk.
- **Give each node a filesystem of `diskSizeGb`.** A loop device or a size-limited volume needs runtime support that Docker Desktop and Podman lack. A full node would also fail writes instead of collecting images.
- **Measure with `du` over containerd's directories.** Exact, but it walks every file of every image each minute on every node.
- **`crictl rmi --prune`.** Removes every unused image at once, including the ones the next deploy is about to use again.
- **One image store shared by every node.** An image would be stored once rather than once per node. Each k3s node runs its own containerd, and containerd locks its metadata database to one daemon, so nodes cannot share its content store. The registry's pull-through cache already saves pulling an image again, but every node still unpacks its own copy.
- **A floor as a percentage of the Runtime disk,** as the kubelet's `imagefs.available` eviction signal is. On a large disk mostly full of other files, a percentage is breached by files GC cannot remove, so it would remove every unused image on every pass while freeing little.
- **Free each node's share of the shortfall.** It removes images still in use elsewhere before older ones, where the issue asks for the oldest-used first across every node.

## Consequences

- A node that held more than its boot disk's high threshold before this change shrinks on the first pass.
- An image last used before a restart may be removed before one that was never used. Each is first seen when the emulator starts, as with a kubelet restart.
- Freed space is estimated, so a pass can remove one image more or fewer than needed. Snapshot usage includes containers' writable layers, which removing images cannot free. A node held over the threshold by those keeps losing its unused images, one pass at a time. The kubelet does the same.
- In a Deterministic Instance, the minimum age counts on the Emulator clock from the first pass that saw an image. Two clock advances longer than it can remove an image that was pulled for a container not yet created.
- The floor removes only images. Node volumes, local-path PersistentVolumes, Cloud SQL data and its WAL archive, and other Managed containers' data take space on the same disk, and when they hold it under the floor, the warning says so.
- In a Deterministic Instance, free space is read from the real disk, not the Emulator clock, so the Runtime disk can fill between clock advances.
- The floor can remove a node's images while that node's boot disk is far from its high threshold, which GKE's kubelet would not do.
