# The managed collector runs in the Instance, not as cluster pods

GKE's managed OpenTelemetry runs a collector Deployment in the `gke-managed-otel` namespace, and workloads send their logs, metrics and traces to `opentelemetry-collector.gke-managed-otel.svc.cluster.local` (FR-GKE-019). We decided that the emulated cluster serves that address with a Service leading to an OTLP receiver in the Instance rather than running collector pods. The receiver forwards to the Instance's OTLP endpoint, which FR-GKE-014's system metrics already use, so the Instance is the one place telemetry leaves.

## Considered options

- **A collector Deployment in `gke-managed-otel`**, as GKE runs it. The cluster looks like GKE's under `kubectl`, but each cluster pulls a collector image (so it fails offline without a cache), and its pods take capacity that would trigger an extra node on an Autopilot cluster (FR-GKE-015) where GKE's would not, because GKE does not count its own system pods against the user.
- **No collector; the workload points at the OTLP endpoint itself.** This is the status quo: workloads need a local override of the collector address, which is the difference from GKE this requirement removes.

## Consequences

- `kubectl get pods -n gke-managed-otel` shows nothing; the namespace holds only the Service. Anything that inspects the collector's pods, ConfigMaps or logs does not find them.
- Enriching telemetry with Kubernetes attributes uses the Instance's own view of the cluster, the same one FR-GKE-014 uses, instead of the OTel Collector's `k8sattributes` processor.
- The receiver goes away with the cluster's scope, not with pod restarts; there is no collector to crash, scale or restart.
