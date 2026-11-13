# A failed cluster delete leaves the cluster in ERROR, and resumes on start

When a GKE cluster delete failed to remove the cluster's Managed containers, the cluster record stayed in STOPPING, its operation ended DONE with the error, and nothing moved it out of STOPPING again until the Instance next started. terraform-provider-google waits for a cluster to leave STOPPING before it deletes, so a `tofu destroy` run again after the failure polled until its own timeout (#1145, main CI run 43457474120). On GKE, a failed delete leaves the cluster in ERROR, and deleting an ERROR cluster again is the supported recovery; a cluster is never in STOPPING with no operation running.

We decided that a failed cluster delete moves the cluster to ERROR, with the failure in `statusMessage`, and the cluster remembers that its delete failed. Deleting it again works as for any ERROR cluster. When the Instance starts, or a State snapshot is restored, a cluster whose delete failed has its delete run again, as a STOPPING cluster's already is, instead of being brought back up as other ERROR clusters are. A cluster in ERROR does not make the Instance not ready; only DEGRADED does (ADR 0045).

## Considered options

- **Keep STOPPING** and let the Instance's next start finish the delete. Real GKE never shows STOPPING without a running operation, and clients that wait for a resting state, such as the google provider, hang until their own timeout.
- **ERROR, and restore it on start** like every other ERROR cluster, which is what the Instance did with an ERROR cluster until now. The cluster the user deleted comes back up with whatever the failed delete left of it.
- **ERROR, and leave it in ERROR on start.** Reclaim has removed its containers by then, so it has no data plane, and the user must send a delete the emulator already accepted once.
- **Drop the record anyway** and leave what was not removed for the next Reclaim. The delete would report success while the cluster's containers may still run and hold its addresses and ports.

## Consequences

- An ERROR cluster is brought back up on start or deleted on start depending on why it is in ERROR, so the cluster record keeps whether its delete failed.
- A State snapshot saved while a cluster's delete has failed holds that too: restoring it, including into another Instance, runs the delete there, and the cluster is gone once it succeeds.
- `/_emu/v1/ready` keeps passing with a cluster in ERROR after a failed delete; the GKE API reports it and a second delete clears it.
- A node pool whose delete fails is not covered (#1142's out-of-scope note); it would take a decision of its own.
