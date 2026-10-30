# A VM relay whose port forward stalls is recreated once

On Podman machine (libkrun, macOS), the host's port forward to the VM relay's published control port sometimes accepts every connection and passes none of them on, for longer than the 60 s that starting the relay waits (#807). The relay's agent answers all the while: a probe on the services network reaches it inside the VM at once. We reproduced it while other Instances were creating and removing containers on the same machine; a hundred relays started one at a time never stalled. In 20 passes of the Host attachment tests, 8 of 72 relay starts stalled this way, and each relay started in their place answered within 0.1 s on its new port. We decided that when the start's wait fails and that probe gets the agent's answer, the Instance removes the relay and starts a new one, which the runtime publishes on a new port, once. When the probe gets no answer, the relay itself is at fault and the start fails at once, so the retry never hides a bug of ours; a second stall also fails, with both attempts' diagnostics, since a port forward that stalls twice is broken rather than flaky.

## Considered options

- **Fail and leave it to Podman.** A start that fails now and then is what #807 reported: the Pre-release host checks failed, and so would a user's `gcpemu start`. A Podman report is filed as well, but its fix would only reach Podman machines that upgrade.
- **Cut the wait short and recreate blindly**, e.g. after 20 s. Starts that took about 50 s and then passed were seen, so a short cut-off would tear down relays about to answer, and without the probe a recreate would also hide a relay that does not serve.
- **Retry until it works.** A forward that stalls twice in a row is not one we can count on, and the user should see that rather than wait without bound.

## Consequences

- A start that stalls and then recovers takes the 60 s wait, up to 30 s of probe, and the second start: about two minutes instead of failing.
- Only the start is covered. A forward that stalls after the relay has answered is left to the reconnect loop, which already finds the control port again; no such stall has been seen.
