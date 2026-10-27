# A VM is a Managed container

Compute Engine joins the Products for v1.0, and its VMs need something to run as. We decided that each VM is a Managed container booting a systemd-capable OS image, attached to its subnetwork's VPC container network with its internal address, reaching the metadata server at 169.254.169.254 with its own instance attributes and service account, and running its `startup-script`. The guest's processes are Real; the VM API, metadata and networking are Faithful; machine type, disks, CPU platform and live migration are Recorded.

## Considered options

- **A real microVM** (QEMU/KVM or Firecracker). The guest gets its own kernel, but it needs `/dev/kvm`, which hosted CI runners and macOS do not offer, so VMs would be Linux-only where the SRS promises every Product everywhere.
- **Recorded only**, with the API storing VMs and nothing running. It is enough for `tofu plan`, but a VM whose startup script never runs and whose port never answers is no use to an application test.

## Consequences

- There is no guest kernel: kernel modules, custom kernels, nested virtualization and Windows guests are not possible, and a VM's `uname` is the container runtime's.
- A VM runs wherever GKE nodes run, through the VM relay on a VM runtime (ADR 0005), because both are Managed containers on VPC container networks.
- A VM boots only images in the Public image catalogue; on `cos-stable`, the `gce-container-declaration` metadata key runs the declared container.
- Ingress firewall rules are enforced for VMs inside each VM's container, so nothing touches the host's firewall. GKE nodes stay unenforced as before, because enforcing rules there would change existing clusters.
- Compute Engine is a Runtime Service: without a container runtime its VMs are accepted and never become `RUNNING`.
