# gcpemu

A single Go binary that emulates Google Cloud services on a workstation or in CI, so the
same OpenTofu modules, client SDKs and application code that target GCP run unmodified
except for endpoint configuration. See [the SRS](GCP%20Local%20Emulator%20Software.md).

## Status

Milestones **M1** (core, IAM, Cloud Storage, Pub/Sub, Cloud DNS, Artifact Registry),
**M2** (compute networking, Cloud SQL, GKE, Cloud NAT) and **M3** (Application Load Balancer,
Cloud CDN, backend mTLS, Certificate Manager, host mode) are implemented. The full SRS 11.2
reference stack (steps 1–10) passes on linux/amd64 (`make e2e`).

| Service | Package | Highlights |
| --- | --- | --- |
| Core | `internal/…` | CLI, gateway (REST + gRPC on one port), admin API, store (memory/bbolt), seed files, fault injection, request log |
| IAM | `services/iam` | Service accounts and keys, roles, policies with conditions, audit/enforce, OAuth2 token endpoint, IAM Credentials, metadata server, STS/WIF |
| Cloud Storage | `services/gcs` | JSON + XML APIs, resumable uploads, generations/preconditions, checksums, V4 signed URLs, notifications, lifecycle, HMAC |
| Pub/Sub | `services/pubsub` | gRPC + REST, streaming pull, ordering, DLQ, filters, push with OIDC, exactly-once, snapshots/seek, schemas |
| Cloud DNS | `services/dns` | Zones, record sets, changes; authoritative UDP/TCP server with forwarding |
| Artifact Registry | `services/ar` | Repositories API with LROs; OCI Distribution v1.1 registry with referrers; pull-through cache, remote and virtual repositories |
| Compute networking | `services/compute` | Networks, subnetworks, firewalls, routes, addresses, routers, NEGs, operations; servicenetworking; subnetworks realised as container networks |
| Cloud NAT | `services/compute`, `services/nat` | Router NAT configs drive an egress gateway container; drop/reject, logging, offline sink |
| Cloud SQL | `services/sql` | sqladmin v1beta4/v1 over real PostgreSQL 14–17 containers; connector (3307), IAM DB auth, authorized networks, private IP, backups, import/export |
| GKE | `services/gke` | container v1 (gRPC + REST) over real k3s clusters; node pools, IAM-backed kube auth, Workload Identity, private nodes via NAT, NEG sync, real-hostname Google APIs in pods |
| Application Load Balancer | `services/lb` | Forwarding rules, proxies, URL maps, backend services/buckets, health checks, SSL certs/policies; in-process L7 proxy with frontend/backend mTLS, NEG and bucket backends, access logs |
| Cloud CDN | `services/cdn` | Cache modes, keys, TTLs, revalidation, negative caching, signed URLs/cookies, invalidation, LRU disk cache |
| Certificate Manager, Network Security | `services/certs` | Certificates (self-managed and managed via the local CA), maps, trust configs, DNS authorizations; backend authentication configs, server TLS policies |

## Quick start

```sh
make build
bin/gcpemu start --detach --services gcs,pubsub --seed seed.yaml
eval "$(bin/gcpemu env)"       # STORAGE_EMULATOR_HOST, PUBSUB_EMULATOR_HOST, GCE_METADATA_HOST, …
bin/gcpemu status
bin/gcpemu tofu-provider --project my-project   # provider "google" block with custom endpoints
bin/gcpemu stop
```

Other commands: `reset`, `logs [service] [--requests]`, `time advance 24h`, `hosts`,
`fault add|list|clear`, `doctor`, `version`, and `admin openapi` (the OpenAPI document of the
`/_emu/v1/` admin API, also served at `/_emu/v1/openapi.yaml`).

### Endpoints

All control-plane APIs are served by the gateway (default `127.0.0.1:4510`), under
`/<api>/` (for example `/pubsub/v1/…`, `/iam/v1/…`) and at their native root paths where
unambiguous (`/storage/v1/`, `/dns/v1/`, `/compute/v1/`, `/sql/v1beta4/`). Per-service ports:
GCS 4443, Pub/Sub 8085, registry 5000, DNS 5353 (UDP+TCP), metadata 8988, Cloud SQL host ports
54320+. `gcpemu env` also exports `KUBECONFIG` for GKE clusters. `--port name=0` picks a free port for one
listener and a bare `--port 0` for all of them (LB and Cloud SQL host ports included);
`--port-range 20000-20999` draws those free ports from a range. The choices are written to
`<data-dir>/endpoints.json` and printed by `gcpemu env`.

### Container runtime

Cloud SQL, GKE and Cloud NAT need Docker Engine (>= 24) or Podman (>= 4.9, Docker-compatible
socket; `DOCKER_HOST` is honoured). Everything the emulator creates is labelled
`gcpemu.instance=<id>`; containers are removed on `stop`, and with `--data-dir` the volumes
holding Postgres and Kubernetes state are kept so resources survive a restart. The emulator
copies its own (static) binary into containers as an in-container agent. GKE is Linux-only.

Cloud NAT knobs: `natReject` / `GCPEMU_NAT_REJECT=true` rejects uncovered egress immediately
instead of timing out; with `--offline`, NAT-allowed egress hits a sink that records the attempt
and returns `natSinkResponse` (`GCPEMU_NAT_SINK_RESPONSE`, default `503:gcpemu offline: egress blocked`).
`GCPEMU_GKE_MAX_NODES` caps nodes per instance (default 5).

### Real hostnames: GKE pods and host mode

The emulator includes a "Google frontend" (endpoint `frontend`, a dynamic port unless you pass
`--port frontend=N`). It terminates TLS for the real hostnames the gateway serves
(`storage.googleapis.com`, `pubsub.googleapis.com`, `sqladmin.googleapis.com`,
`iamcredentials.googleapis.com`, `oauth2.googleapis.com`, …) and for every
`LOCATION-docker.pkg.dev`. Certificates come from the instance CA (`<data-dir>/ca.pem`). It then
passes HTTP/1.1, HTTP/2 and gRPC through to the emulator unchanged. APIs the emulator doesn't
serve (`logging.googleapis.com`, …) are never redirected.

**GKE pods** use real hostnames automatically. Each node answers DNS for the served names with
169.254.169.254 and relays port 443 there to the frontend. A mutating admission webhook mounts a
bundle of the host's system roots plus the emulator CA at `/etc/ssl/certs/ca-certificates.crt`
and `/etc/gcpemu/certs/`. It also sets `SSL_CERT_FILE`, `REQUESTS_CA_BUNDLE`,
`NODE_EXTRA_CA_CERTS` and `GCE_METADATA_HOST`. Unmodified Go, Python, Java and Node clients get
Workload Identity tokens from the metadata server and reach the emulated services without any
endpoint configuration. To opt a namespace or a pod out, label it `gcpemu.dev/inject=disabled`;
`kube-system` is always skipped. Nodes also trust the CA in their own system store.

**Host mode** (`gcpemu start --host-mode`, Linux) does the same for host processes without
root. It starts a labelled container (`gcpemu-<id>-hostmode`) that the host reaches at its
container IP. The container serves HTTPS on :443, plain HTTP on :80 and DNS on :53, answering the
served names with its own IP. It forwards other `googleapis.com`/`pkg.dev` names to your real
upstream resolvers and everything else to the emulated Cloud DNS. `gcpemu env` prints the IP as
`GCPEMU_HOST_MODE_IP` and writes setup steps to stderr. There are two ways to send the names
there:

```sh
# per-domain DNS routing with systemd-resolved (lasts until the bridge restarts)
sudo resolvectl dns <bridge-if> <ip> && sudo resolvectl domain <bridge-if> ~googleapis.com ~pkg.dev
# or a static /etc/hosts block (remove it when done)
gcpemu hosts | sudo tee -a /etc/hosts
```

Then trust the CA system-wide with `gcpemu ca install` (on Linux it also adds the CA to Docker's
and containerd's per-registry trust for every `LOCATION-docker.pkg.dev`), or per shell with
`eval "$(gcpemu env --trust)"`. That sets `SSL_CERT_FILE` to `<data-dir>/ca-bundle.pem`, which
holds the system roots plus the emulator CA. `gcpemu env` always exports `GCPEMU_CA_FILE`. To
test without changing the host, use `curl --resolve storage.googleapis.com:443:<ip> --cacert
"$GCPEMU_CA_FILE" …`, or give Go clients a `net.Resolver` that dials `<ip>:53`.

### Configuration

Flags > `GCPEMU_*` env vars > `gcpemu.yaml` > defaults. `CI=true` implies `--ephemeral`
and JSON logs. IAM runs in `audit` mode by default (`--iam-mode off|audit|enforce`).

### Seed files

Top-level keys are `projects`, `faults` and service names (`iam`, `compute`, `gcs`, `pubsub`,
`dns`, `ar`, `sql`, `gke`). Each service documents its section in its `seed.go`.

### Go tests

```go
inst := emutest.Start(t, []string{"gcs", "pubsub"})
inst.Setenv(t) // client env vars for this test
```

### Reference stack (SRS 11.2)

`e2e/` is the acceptance test of the whole emulator: one OpenTofu root module
(`e2e/stack/`), a Go API (`e2e/app/`) and `TestReferenceStack`, which drives them against an
in-process emulator with IAM in `enforce` mode:

1. `tofu apply` creates a VPC and subnet (pod/service ranges), Cloud Router + NAT, a private
   GKE cluster with Workload Identity and a node pool, an Artifact Registry repo, buckets
   (static assets, uploads with a Pub/Sub notification), a topic with an OIDC push
   subscription, Cloud SQL Postgres 16 on a private IP with an IAM service account user,
   the `example.test.` zone and a global external ALB with Cloud CDN (backend bucket, NEG
   backend service with backend mTLS, managed certificate, HTTP→HTTPS redirect); a plan
   right after must be empty;
2. the app is built for amd64 and arm64 FROM scratch and pushed to AR, Istio is installed with
   `istioctl` (its ingress gateway requires the LB's client certificate), the app is deployed
   and a second apply (`api_neg_name`) adds the gateway's GKE NEG to the load balancer;
3. `https://app.example.test/` (emulated DNS, emulator CA; Go client and curl) serves the
   bucket's page and the second request reports `X-Cache-Status: hit`;
4. `/api/health` goes LB → Istio → app; a client without the LB certificate is refused;
5. `/api/work` writes to GCS, inserts into Cloud SQL (Go connector, IAM auth) and publishes,
   using Workload Identity and no endpoint configuration;
6. the bucket notification and the message arrive by push with a verified OIDC token;
7. the pod reaches the internet through Cloud NAT, and not after `nat_enabled=false`;
8. `app_storage_access=false` makes step 5 fail with `PERMISSION_DENIED`;
9. `urlMaps.invalidateCache` on `/*` makes the next request a `miss`;
10. `tofu destroy` leaves no containers (`gcpemu status` lists the instance's containers).

```sh
make e2e                          # = GCPEMU_NET_TESTS=1 go test -tags e2e ./e2e -run TestReferenceStack -timeout 30m -v
GCPEMU_NET_TESTS=0 make e2e       # offline-ish: steps 1, 3, 9 and 10 only
```

It needs a container runtime and `tofu` on `PATH`; `tofu init` downloads the `google` and `tls`
providers. Steps 2 and 4–8 download `istioctl` (pinned, checksum-verified) and the Istio
images and reach `https://example.com/`, so they only run with `GCPEMU_NET_TESTS=1`.
Downloads are cached in `$GCPEMU_E2E_CACHE` (default `<user cache dir>/gcpemu-e2e`). The run
must finish within 10 minutes (`GCPEMU_E2E_BUDGET` overrides); it prints per-step timings.
With `emulator_gateway` empty the module targets real GCP (set `project` and `domain`).

## Development

```sh
make vet race      # gofmt, vet, tests with -race
make release       # static binaries for linux/darwin × amd64/arm64
```

Each service is an isolated package implementing `emu.Service`
(`internal/emu/emu.go`); cross-service contracts live in `internal/emu/xservice.go`.

Licensed under Apache-2.0.
