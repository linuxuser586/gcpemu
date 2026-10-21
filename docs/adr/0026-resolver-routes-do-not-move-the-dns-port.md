# Resolver routes don't move the dns Service endpoint, and on Linux they last until reboot

`gcpemu dns install` adds Resolver routes that send host queries for a suffix to the Instance's `dns` Service endpoint (#608). Windows' NRPT can't name a port, so on Windows `install` refuses unless the `dns` port is 53 and tells the user to start the Instance with `--port dns=53`. `start` still defaults `dns` to `127.0.0.1:5353` everywhere. On Linux, the routes are `resolvectl` settings on a dummy link that the command creates and owns. They last until the link goes away or the host reboots, and `gcpemu doctor` reports a route that is gone. The macOS files and Windows NRPT rules do persist.

A Service endpoint that moves by OS would break existing Windows users and tests that rely on 5353. It would also make `start` fail by default wherever ICS, Hyper-V or WSL holds port 53, even for users who never wanted a Resolver route. On Linux, an emulator change that silently outlives a reboot is surprising, and the persistent alternatives each reach past the emulator's own state.

## Considered options

- **Default `dns` to `127.0.0.1:53` on Windows.** Rejected for the reasons above.
- **A persistent `/etc/systemd/resolved.conf.d/` drop-in.** A global `DNS=` can become a default route, sending the host's unrelated lookups to the emulator.
- **Routes on `docker0` or a physical link.** `docker0` is missing with Podman, a VM runtime or no runtime, and NetworkManager overwrites a physical link's resolver settings when it reconnects.
- **A persistent dummy link through systemd-networkd or NetworkManager.** Too many moving parts for a development aid.
