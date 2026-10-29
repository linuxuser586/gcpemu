# The Instance restarts an exited Cluster server

On a busy host, k3s can exit hours after its cluster became ready: kine's compaction misses its deadline and k3s treats the failed commit as fatal (#840). Until now the Instance only reported the exit, as a Disrupted container, and the cluster stayed down until the user stopped and started the whole Instance. We decided that the GKE Service watches each RUNNING cluster's Cluster server and starts it again when it exits, as #395 already does during bring-up. If it exits 3 times within 10 minutes, the Service stops restarting it and marks the cluster DEGRADED with the exits and the k3s log tail. This is a narrow exception to the out-of-scope rule on recovery (`.out-of-scope/recovery-from-runtime-restart.md`): a Service may restart a Managed container it watches when all of that container's state is on its volumes, and the Cluster server is the only such container so far.

## Considered options

- **Detection only** (the rule so far). It is correct, but stopping and starting the Instance to recover one k3s process restarts every Managed container and cuts off everything else in use.
- **Recovery for every Managed container**, as the out-of-scope note asks of any proposal. The other containers lose state a restart doesn't bring back (the VM relay's listeners, a node's pods, Cloud SQL's address), so each would need its own recovery, and none of them has been seen crashing on its own.
- **Telling a crash from a runtime restart** and restarting only after a crash. The runtime reports both as an exit, and a guess based on exit codes would be wrong sometimes. Restarting after a runtime restart does no harm: the other containers are still Disrupted, so the Instance stays not ready until it is stopped and started.
- **A runtime restart policy** (`on-failure`) on the container. The Instance would not see the exits, could not stop after a crash loop, and would report each restart as Disrupted.

## Consequences

- A Cluster server the Instance started again is not Disrupted. One started by something else still is.
- A cluster stays RUNNING while its Cluster server is being restarted. Once the Service gives up, the cluster is DEGRADED and `/_emu/v1/ready` fails, naming it. Stopping and starting the Instance is still the way back from there.
- While the Cluster server is down, the cluster's endpoint still accepts connections and closes them. Refusing them would mean closing the exposed port, which could then move. The Instance logs a warning naming the cluster instead.
- A proposal to restart another Managed container should show that all of its state is on its volumes, and add it here.
