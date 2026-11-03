# On Linux, only the runtime's own bridge holding the gateway shows a network is on the host

Amends ADRs 0005 and 0056.

ADR 0056 kept the gateway probe on Linux: an Instance on the host is on a VM runtime when it cannot bind its services network's gateway. A successful bind shows only that some host adapter holds the address, not that the runtime's bridge does. A VPN, a libvirt bridge such as `virbr0`, or a second runtime's bridge can hold it. Docker Desktop for Linux runs beside Docker Engine, and both report a default bridge `docker0` at `172.17.0.1`. Rootful and rootless Podman both report `podman0` at `10.88.0.1`. Doctor, the host-mode probe and the Registry relay's early probe test the default bridge, while the netplane tests the services network, so they can disagree (#915). We decided that on Linux a runtime is a VM runtime unless it does not describe itself as Docker Desktop or rootless (`Info`'s operating system and security options), and the bridge interface it reports for the network holds the network's gateway. Docker reports that interface as `com.docker.network.bridge.name`, or else `br-` and the first 12 characters of the network ID. Rootful Podman reports it only on its libpod network endpoint, as `network_interface`. An interface lookup replaces the bind. `GCPEMU_VM_RELAY` still forces the relay either way.

## Considered options

- **Trusting a bind or an interface match alone.** A matching name and address can belong to another runtime on the same host, as with `docker0` under Engine and Desktop.
- **gcpemu choosing subnets that avoid every address the host holds.** Rejected in ADR 0056: gcpemu would allocate subnets itself, and an adapter that appears later still collides.
- **Deciding once per Instance and reusing the decision everywhere.** Doctor and the host-mode probe decide before any services network exists, so they would still need a rule of their own.

## Consequences

- Recognising a runtime by what it reports only ever turns the relay on. An unknown runtime still gets the interface check.
- Rootless runtimes count as VM runtimes, as the bind probe already made them. Rootless Docker still needs the Registry relay; rootless Podman does not (ADR 0043, #923).
- `runtime.InspectNetwork` reports Podman's real bridge name, so host mode names the right interface too.
