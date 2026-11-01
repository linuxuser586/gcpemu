# Edge aliases are reserved addresses

The Edge relay takes an Edge alias on the load balancer's container network for each forwarding rule IP it serves, with `ip addr add` inside its container, so the runtime never sees it. The VM relay joins that network at the highest address the runtime reports free, and the Edge relay allocated Edge aliases from the top of the same range, so both took the same address. The VM relay's connections to the Edge relay then came from one of the Edge relay's own addresses, the kernel dropped them as martian, and the Edge relay's configuration push hung until pod routes never reached it (#900). It showed only where forwarding rules use the Edge relay and the VM relay is on, as on CI runners that refuse privileged ports with `GCPEMU_VM_RELAY=1`. We decided that the Edge relay skips every address the runtime reports held on the network when it takes an Edge alias, and that the lb service reserves its live Edge aliases (`emu.AddressReserver`), so that a container joining the network after them, such as a VM relay recreated (ADR 0050), skips them too. If the runtime cannot report the held addresses, the forwarding rule gets a high port instead of an Edge alias.

## Considered options

- **Checking only the runtime's held addresses.** It covers an Edge alias taken after the VM relay joined, not a VM relay that joins after the Edge alias, which a recreated relay does.
- **Moving an Edge alias that a container now holds.** The forwarding rule's listener address changes while clients use it, and the VM relay's traffic is lost until the next push notices.
- **Separate ranges: Edge aliases from the bottom of the subnet, joining containers from the top.** The runtime assigns containers without a fixed address from the bottom.

## Consequences

- Reserved addresses are no longer only ones that stored resources record (ADR 0035): they include addresses a container holds without the runtime knowing. A Service that adds such addresses inside its containers reserves them the same way.
- While an Edge alias is reserved on the network, a join that fails at its chosen address does not retry with the runtime's own choice (`attach`), which could be a reserved address.
- The runtime asks for reserved addresses during joins the Edge relay makes while holding its lock, so the Edge aliases are read under a lock of their own.
