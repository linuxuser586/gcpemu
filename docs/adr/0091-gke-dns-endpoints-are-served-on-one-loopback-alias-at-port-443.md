# GKE DNS endpoints are served on one Loopback alias at port 443

A GKE cluster with `dnsEndpointConfig` has a DNS endpoint, `gke-<hash>-<project number>.<location>.gke.goog`, and gcloud writes a kubeconfig for it as `https://<name>` with no CA, both for `get-credentials --dns-endpoint` and whenever IP endpoints are off (#1229). So a host client dials port 443 of whatever the name resolves to and verifies the certificate against Host trust. We decided that the emulated DNS answers each cluster's DNS endpoint with one Loopback alias, taken from the shared pool on the first query, where the emulator listens on port 443 and terminates TLS with an Instance CA certificate for the name the client sent. SNI and the Host header pick the cluster. A request needs a Google access token whose Principal has `container.clusters.connect` on the cluster, and a DNS endpoint that allows external traffic. It then runs on the API server as that Principal, as through the Connect gateway (ADR 0002).

The hash is derived from the cluster's ID, so the name never moves across restarts or endpoint updates.

## Considered options

- **Through Host mode.** Resolving the name to the host-mode container and serving it in the Google front end needs no privileged port, but it works only with `--host-mode`, and the front end serves only exact hostnames.
- **Host mode, falling back to a Loopback alias.** The most reach, but two paths to test for one endpoint.
- **A port in the reported name** (`<name>:<port>`). gcloud would write a working kubeconfig with no privileged port, but the name would not be in GKE's form, and consumers that build URLs from it would break.
- **One alias per cluster**, as Kubernetes Services from the host have. With SNI one listener serves every cluster, and a second port-443 binding per cluster gains nothing.

## Consequences

- `gke.goog` is not among `gcpemu dns install`'s default suffixes: once it is routed, every `*.gke.goog` name resolves only for the emulator's clusters, so a real GKE cluster's DNS endpoint stops resolving on that host. Users add it with `--suffix gke.goog`.
- On Linux the emulator binds port 443 only where `net.ipv4.ip_unprivileged_port_start` allows it. On macOS it binds the wildcard address and serves loopback connections only. A Containerized Instance, a host without aliases or a refused port leaves the names SERVFAIL; the reason is in the Admin API, and `gcpemu doctor` prints it.
- Turning IP endpoints off changes only what the cluster reports. The emulator keeps serving the API server on its IP and LAN addresses, which its own kubeconfig uses.
- Requests from VPC workloads, which GKE admits without `allowExternalTraffic`, are not served: pods reach the API server in-cluster.
