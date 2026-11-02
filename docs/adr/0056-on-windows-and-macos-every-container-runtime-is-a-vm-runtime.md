# On Windows and macOS every container runtime is a VM runtime

Amends ADR 0005.

ADR 0005 chose the VM relay with a probe: an Instance on the host is on a VM runtime when it cannot bind its services network's gateway, and the probe found Docker Desktop for Linux as well. A successful bind is meant to show that the gateway is on a bridge the runtime made on the host. On Windows and macOS, though, no container runtime makes its bridges on the host, so a bind there succeeds only because some other host adapter holds the same address. On a Windows host with WSL, the `vEthernet (WSL)` adapter held `192.168.16.1/20`. Once Docker Desktop had run out of free 172.x ranges, it gave a services network `192.168.16.0/20`, and gcpemu decided the runtime was not a VM runtime. Host attachment then bound Offered ports on the host, where the Host address does not exist, and the Registry relay turned itself off. Doctor still said the relay was on, since it probes the default bridge's gateway, not the services network's (#913). We decided that on Windows and macOS every container runtime is a VM runtime, with no probe, and that Linux keeps the probe. `GCPEMU_VM_RELAY` still forces the relay either way on every OS.

## Considered options

- **Trusting a successful bind only on an interface the runtime made**, such as `docker0`, `br-*` or `podman*`. That would keep one rule everywhere, but the interface checks are different on each OS, and on Windows and macOS they would never succeed anyway.
- **gcpemu choosing subnets that avoid every address the host holds.** That would also help on Linux, for example with a VPN, but gcpemu would allocate subnets itself instead of leaving it to the runtime, and a host adapter that appears after the network exists would still collide with it.
- **Recognising the runtime from what it reports about itself**, such as Docker's `OperatingSystem: Docker Desktop`. The list of names would only grow, and an unknown runtime on macOS or Windows would still be a VM runtime.

## Consequences

- The VM relay's reason on these hosts names the OS, as in `on (windows runs container runtimes in a VM)`, not a gateway. Doctor no longer reports the relay as undetermined there when the runtime has no default bridge network.
- Pre-release check 7 requires the relay to be on and not forced, not on because of the probe.
- On Linux, a host adapter holding a services network's gateway can still mislead the probe. #915 tracks that.
