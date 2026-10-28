# VM-side clients reach the registry through a Registry relay

On a VM runtime, `docker push` and `podman push` are done by the runtime's daemon inside its VM, so `localhost:4561` there is the VM's loopback, not the host's, and pushes to `GCPEMU_REGISTRY` fail (#831). The VM relay can't help: it sits on the Instance's bridge networks, and the daemon is not on them. We decided that an Instance on a VM runtime also starts a Registry relay: a Managed container in the VM's host network namespace that listens on `127.0.0.1:<the registry's port>` and forwards to the registry's netplane address on the services network, the upstream GKE nodes pull from. VM-side clients then push and pull with the same image references as Host clients, under the loopback listener's auth rules, and Docker needs no `insecure-registries` entry because it treats loopback registries as insecure.

## Considered options

- **A daemon-facing address in `gcpemu env`**, such as `host.docker.internal:4561`. The name differs per runtime (`host.lima.internal` on Colima, `host.containers.internal` on Podman), every user would have to add it to `insecure-registries` and restart the daemon, and images would need a second reference that Host clients don't use.
- **Listening on `[::1]` too.** The Docker Desktop and Colima daemons never connect to `[::1]` for `localhost`, and OrbStack and Colima forward an in-VM listener to the host, where it could take the address the Instance binds.
- **Moving to a free port when the registry's port is taken in the VM.** That would break the single reference, so the Instance starts without the Registry relay and warns instead.
- **Documenting a push from the host** (`docker save` and `crane push`) as the only way. It's the workaround, but `docker push` is the first thing users try.

## Consequences

- An Instance on the host that runs Artifact Registry no longer waits for a Service to need containers before it connects to the runtime: once its Services have started it connects in the background and, on a VM runtime, starts the VM relay and the Registry relay. It decides with the VM relay's own decision, so the two never disagree. Reset removes the Registry relay with the VM relay and starts both again. Start and readiness don't wait for them. emutest's Instances don't, so tests that push from the host start no containers.
- OrbStack and Colima forward a host-network container's port to the host, and whichever side binds first holds it. The Instance binds its host registry listeners before starting the Registry relay, and removes a leftover Registry relay before binding.
- Podman doesn't treat `localhost` as insecure, so Podman machine users need `podman push --tls-verify=false` or a `registries.conf` entry inside the machine. The Instance prints the step but never edits the VM.
- CI has no VM runtime, so the Pre-release host checks push with each Required runtime's own CLI.
- A Containerized Instance needs no Registry relay (#835). The runtime binds its Published registry port inside the VM, where VM-side clients reach it at the same `localhost` address as Host clients, and a relay there would only compete with that binding. The Pre-release host checks push to a Containerized Instance's registry on each Required runtime, and a runtime where that fails gets its own issue.
