# Managed Prometheus scrapes from the Instance, not from collector pods

GKE's Managed Prometheus runs GMP collector pods (a DaemonSet in `gke-gmp-system`) that scrape the Scrape targets of PodMonitorings and ClusterPodMonitorings and write to Cloud Monitoring (FR-GKE-020). We decided that the Instance scrapes them itself, through each cluster's API server pod proxy, and exports to the OTLP endpoint, following ADR 0034's reasoning for the Managed collector. The cluster gets only GMP's CRDs.

## Considered options

- **GMP's collector and operator pods**, as GKE runs them. `kubectl` shows what GKE shows, but each cluster pulls GMP images (so it fails offline without a cache), the DaemonSet takes capacity that would trigger extra nodes on an Autopilot cluster (FR-GKE-015) where GKE's system pods would not, and the collector writes to Cloud Monitoring, which the emulator does not serve (10.3), so it would still need rewiring to OTLP.
- **The Instance dials pod IPs directly** instead of using the API server proxy. It avoids the extra hop, but pod addresses are not reachable from the Instance on every runtime (VM runtimes, Translated networks), while the API server is.

## Consequences

- `kubectl get pods -n gke-gmp-system` shows nothing; there is no collector to inspect, restart or scale, and no `OperatorConfig`.
- Every scrape goes through the cluster's API server, so scraping many targets at short intervals loads it in a way GKE's node-local collectors do not.
- Kubernetes attributes come from the Instance's own view of the cluster, the one FR-GKE-014 uses.
- Endpoint settings that only make sense for a collector's own HTTP client (`proxyUrl`, and `tls`, `authorization`, `basicAuth` and `oauth2`, which the proxy does not pass through as configured) are not applied; the PodMonitoring's or ClusterPodMonitoring's condition names them.
