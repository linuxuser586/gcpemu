# The CA injector fails closed

The CA injector was registered with `failurePolicy: Ignore`, and its handler answered `allowed: true` with no patch when it could not put the namespace's `gcpemu-ca-bundle` ConfigMap in place. Either way the pod started without the Instance CA. It looked healthy until its first Google API call, which failed with `x509: certificate signed by unknown authority`, far from the cause, and nothing retried it. During one release, with CI running in the same cluster, three admissions hit the webhook's 10 second timeout, and a Deployment's pod crash-looped until it was deleted by hand (#1053).

So the CA injector now fails closed. It is registered with `failurePolicy: Fail`. When the ConfigMap can't be ensured, the handler refuses the pod with 503 `ServiceUnavailable`. The message names the ConfigMap, the namespace and the cause, and says the pod's controller will retry. A ReplicaSet, Job or StatefulSet creates the pod again on its own backoff, and that pod gets the Instance CA. Dry-run admissions are still answered with the patch and no ConfigMap.

Most admissions now make no API server call at all. Each admission used to read the ConfigMap back from the cluster it was admitting into, and under load that call is what used up the 10 seconds. The handler now remembers, per cluster and namespace, that the ConfigMap held the current Instance CA, for 60 seconds of real time, and skips the read while that holds. A rotated Instance CA misses the cache. A refresh patches the ConfigMap only if it hasn't changed since it was read, by its `resourceVersion`, so an admission still holding the old Instance CA can't write it back over the new one while the cache says the new one is in place; the admission that loses refuses its pod. The 60 seconds bound how long the injector can be wrong after a namespace is deleted and recreated, or the ConfigMap is deleted by hand. In either case the pod's ConfigMap volume is `optional`, so the pod still starts, without the Instance CA, as before this change. The cache is per cluster's runtime state, so a cluster deleted and created again under the same name starts empty.

The scope is unchanged: `kube-system`, and `gcpemu.dev/inject=disabled` on a namespace or a pod, are still the only ways out.

## Considered options

- **Keep failing open and retry the read inside the handler.** The time a retry needs is the time the API server isn't giving, so it would still time out, and a timed-out webhook under `Ignore` still admits the pod unchanged.
- **Raise `timeoutSeconds`.** The admission waits longer on every pod during load, and a slow enough API server still runs out the timeout.
- **Watch ConfigMaps instead of caching for a fixed time.** It would be exact, but it needs a watch per cluster across every namespace, kept alive and resynced, for a check that is right nearly every time.
- **Exempt more namespaces.** Any pod outside the exemptions could still start without the Instance CA, and which namespaces were safe to exempt would be a guess.
- **Use the Emulator clock for the 60 seconds.** In a Deterministic Instance it moves only with requests, so a cache entry could outlive any real-time bound.

## Consequences

- While the Google front end is down, pods outside `kube-system` that haven't opted out can't be created. They are created once it is back.
- The NEG readiness gate and Instrumentation webhooks still fail open (ADR 0071). A pod admitted without them works. A pod without the Instance CA does not.
- A user who runs `kubectl run` or `kubectl apply` on a bare Pod during an outage gets the 503 and has to retry by hand.
