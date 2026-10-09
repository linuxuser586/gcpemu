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
| Secret Manager | `services/secrets` | gRPC + REST, global and regional secrets, versions and aliases, IAM, expiry and delayed destruction on the Emulator clock, Pub/Sub event notifications, managed rotation of Cloud SQL passwords |
| Cloud DNS | `services/dns` | Zones, record sets, changes; authoritative UDP/TCP server with forwarding |
| Artifact Registry | `services/ar` | Repositories API with LROs; OCI Distribution v1.1 registry with referrers; pull-through cache, remote and virtual repositories |
| Compute networking | `services/compute` | Networks, subnetworks, firewalls, routes, addresses, routers, zonal/regional/global NEGs, operations; servicenetworking; subnetworks realised as container networks; VPC peering (recorded) |
| Cloud NAT | `services/compute`, `services/nat` | Router NAT configs drive an egress gateway container; drop/reject, logging, offline sink |
| Cloud SQL | `services/sql` | sqladmin v1beta4/v1 over real PostgreSQL 14–18 containers; connector (3307), IAM DB auth, authorized networks, private IP, backups, import/export, `executeSql` (Data API) |
| GKE | `services/gke` | container v1 (gRPC + REST) over real k3s clusters; node pools, IAM-backed kube auth, Workload Identity, private nodes via NAT, NEG sync, real-hostname Google APIs in pods; Connect gateway (`connectgateway` v1) to each cluster's Kubernetes API; Gateway API CRDs, the Secret Manager add-on (`secrets-store-gke.csi.k8s.io`) and SecretSync from cluster settings |
| Application Load Balancer | `services/lb` | Forwarding rules, proxies, URL maps, backend services/buckets, health checks, SSL certs/policies; in-process L7 proxy with frontend/backend mTLS, NEG and bucket backends, access logs; Cloud Armor policies, TCP/SSL/gRPC proxies and legacy health checks (recorded) |
| Cloud CDN | `services/cdn` | Cache modes, keys, TTLs, revalidation, negative caching, signed URLs/cookies, invalidation, LRU disk cache |
| Certificate Manager, Network Security | `services/certs` | Certificates (self-managed and managed via the local CA), maps, trust configs, DNS authorizations; backend authentication configs, server TLS policies |

## Quick start

```sh
make build
bin/gcpemu start --detach --services gcs,pubsub --seed seed.yaml
eval "$(bin/gcpemu env)"       # STORAGE_EMULATOR_HOST, PUBSUB_EMULATOR_HOST, GCE_METADATA_HOST, …
bin/gcpemu status
bin/gcpemu tofu-provider --project my-project   # provider "google" block with custom endpoints
bin/gcpemu console              # the Web console in the default browser
bin/gcpemu stop
```

Other commands: `reset`, `logs [service] [--requests]`, `time advance 24h`, `hosts`,
`fault add|list|clear`, `doctor`, `version`, and `admin openapi` (the OpenAPI document of the
`/_emu/v1/` admin API, also served at `/_emu/v1/openapi.yaml`).

### Web console

The gateway serves a browser console at `http://127.0.0.1:4510/console/`; `gcpemu console`
opens it. It calls only the public APIs and the admin API, as the default principal, and
works offline. `--console=false` turns it off; under `CI=true` it is off unless `--console`
is passed, and it is not served when `--bind` is beyond loopback. `make build` builds it when
`pnpm` is on PATH (Node.js 24); without it the binary serves a "console not built" page.
It reaches GKE clusters' Kubernetes objects and pod logs through the Connect gateway
(`/connectgateway/v1/projects/P/locations/L/gkeMemberships/CLUSTER/...`, see
`docs/adr/0002-kubernetes-api-through-connect-gateway.md`), which `kubectl` can use too.
Its Cloud SQL query runner uses `instances.executeSql` (the Data API), so an instance needs
`settings.dataApiAccess` set to `ALLOW_DATA_API`; statements run read-only unless unticked.
Unlike Cloud SQL, the emulator lets `executeSql` name a built-in user without a password.

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
`LoadBalancer` Services are given node addresses on the cluster's VPC by k3s's built-in service
load balancer (servicelb), the emulator's in-cluster L4 allocator (FR-GKE-009).

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

Flags > `GCPEMU_*` env vars > `gcpemu.yaml` > defaults. `CI=true` implies `--ephemeral`,
JSON logs and no Web console. IAM runs in `audit` mode by default (`--iam-mode off|audit|enforce`).

### Seed files

Top-level keys are `projects`, `faults` and service names (`iam`, `compute`, `gcs`, `pubsub`,
`dns`, `ar`, `sql`, `gke`). Each service documents its section in its `seed.go`.

### Go tests

```go
inst := emutest.Start(t, []string{"gcs", "pubsub"})
inst.Setenv(t) // client env vars for this test
```

### GitHub Actions

```yaml
- uses: linuxuser586/gcpemu/setup-gcpemu@v0
  with:
    services: gcs,pubsub,sql
    seed: test/emu-seed.yaml
```

This installs the release binary after checking it against `SHA256SUMS`, and starts the
instance once the seed has been applied. It exports `gcpemu env` to later steps. When the job
fails, it uploads the logs, request log, resource dump and kubeconfig as an artifact. See
[setup-gcpemu/README.md](setup-gcpemu/README.md). Releases are published from `vX.Y.Z` tags, and each one moves
its major tag. Until v1.0.0 that tag is `@v0`; pin `@v0.1.0` for an exact release.

### Container image

Each release is also a multi-arch (amd64, arm64) distroless image, running as a non-root
user, at `docker.io/linuxuser586/gcpemu` and `ghcr.io/linuxuser586/gcpemu`. It is tagged
`X.Y.Z`, `X.Y`, `X` and `latest`; a prerelease gets only its exact version.

```sh
docker run -d --name gcpemu -p 4510:4510 -p 4443:4443 -p 8085:8085 \
  -v gcpemu:/data linuxuser586/gcpemu:0
docker exec gcpemu /gcpemu status
```

The image binds `0.0.0.0` with IAM in audit mode, keeps state in `/data` (mount a volume to
keep it across containers, or set `GCPEMU_EPHEMERAL=true`), and reports healthy once every
Service is ready (`gcpemu status --ready`). The Web console is not served, because the emulator
is not bound to loopback. Load balancer forwarding rules listen on their own ports, so publish
those with extra `-p` flags.

Cloud SQL, GKE and Cloud NAT start containers of their own, so they need the host's Docker
socket (Docker-outside-of-Docker). Without the socket the image starts every other Service and
logs which it skipped. With it, the emulator stays unprivileged and non-root; it needs only the
socket's group:

```sh
docker run -d --name gcpemu -p 4510:4510 -p 4443:4443 -p 8085:8085 \
  -v /var/run/docker.sock:/var/run/docker.sock \
  --group-add "$(stat -c %g /var/run/docker.sock)" \
  linuxuser586/gcpemu:0 start --ephemeral
```

The emulator finds its own container and joins the networks it creates (taking each
subnetwork's reserved second-to-last address), so it works on a normal bridge or compose
network as well as with `--network host`. Privileged GKE nodes get their privilege from the
Docker daemon, not from the emulator's container. From the host:

- Cloud SQL instances are published on `127.0.0.1` (`GCPEMU_SQL_*` ports, as on the host).
  The Cloud SQL Auth Proxy and connectors dial the instance IPs on the emulator's bridge
  networks, which a Linux host reaches directly (Docker Desktop does not).
- `GET /container/_emu/kubeconfig?cluster=projects/P/locations/L/clusters/C` returns a
  kubeconfig whose server is the cluster's API published on `127.0.0.1`; the Connect gateway
  (`/connectgateway/v1/...`) works through the API gateway port.
- Use `--ephemeral` (or a volume on `/data`): stopping an ephemeral instance removes all of its
  containers, networks and volumes, while a persistent one keeps them for its next start. As a GitHub Actions service container (the job waits for the health check):

```yaml
services:
  gcpemu:
    image: linuxuser586/gcpemu:0
    ports: ["4510:4510", "4443:4443", "8085:8085"]
env:
  STORAGE_EMULATOR_HOST: http://localhost:4443
  PUBSUB_EMULATOR_HOST: localhost:8085
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
2. the app is built for amd64 and arm64 FROM scratch and pushed to AR, Istio is installed from
   its Helm charts (its ingress gateway requires the LB's client certificate), the app is deployed
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
providers. Steps 2 and 4–8 download Helm and the Istio release for its charts (both pinned and
checksum-verified) and the Istio images and reach `https://example.com/`, so they only run with `GCPEMU_NET_TESTS=1`.
Downloads are cached in `$GCPEMU_E2E_CACHE` (default `<user cache dir>/gcpemu-e2e`). The run
must finish within 10 minutes (`GCPEMU_E2E_BUDGET` overrides); it prints per-step timings.
With `emulator_gateway` empty the module targets real GCP (set `project` and `domain`).

### Client compatibility (SRS 7.1)

`make compat` runs the real clients against a detached instance with only endpoint
configuration: `gcloud` (storage, pubsub, secrets, iam, dns, compute load balancing and NAT, artifacts,
sql, container), `kubectl` through `gke-gcloud-auth-plugin`, Helm, Docker, `crane`, `ko`, `psql`,
the Cloud SQL Auth Proxy v2 (including automatic IAM authentication) and `dig`. gcloud (with the
`gke-gcloud-auth-plugin` component), kubectl, dig and Docker come from `PATH`; Helm, crane, ko
and the Auth Proxy are downloaded at pinned, checksum-verified versions.

### OpenTofu acceptance (SRS 11.1, IF-001)

`make tofu` applies each module in `compat/tofu/` (one per service area: IAM, Storage, Pub/Sub,
DNS, VPC networking, load balancing and CDN, Certificate Manager, Cloud SQL, GKE, Artifact
Registry), plans again expecting no changes, destroys it and checks the state is empty. It does
this with the `google` and `google-beta` providers, each at the latest release of the current
and the previous minor (looked up in the OpenTofu registry), against a detached instance whose
provider block is `gcpemu tofu-provider` output. `GCPEMU_TOFU_PROVIDERS` narrows the matrix
(`google-beta`, `google@8.6.0`, `google@8.6.0,google-beta@8.5.0`) and `GCPEMU_TOFU_MODULES`
the modules (`lb,dns`). The resource types each module must declare are listed in
`compat/tofu_test.go`; that list is the supported surface. It needs `tofu` on `PATH` and, for
Cloud SQL and GKE, a container runtime.

The `gcpemu tofu-provider` block works for `google-beta` as well: rename it to
`provider "google-beta"` and keep the `/v1/` endpoints, since the emulator also serves the beta
API versions the provider uses.

Pointing gcloud at an instance needs nothing beyond `eval "$(gcpemu env)"` plus a token: for
example `echo owner > /tmp/tok; export CLOUDSDK_AUTH_ACCESS_TOKEN_FILE=/tmp/tok`. Registry
clients log in to `$GCPEMU_REGISTRY` as `oauth2accesstoken` with `gcloud auth print-access-token`.

## Development

```sh
make vet race      # gofmt, vet, tests with -race
make release       # static binaries for linux/darwin × amd64/arm64, with the Web console
make cross         # the same binaries without building the console (no Node.js)
```

The Web console (`console/`) is Vite + React + TypeScript; `console/embed.go` embeds its
`dist/`. With Node.js 24 and pnpm:

```sh
bin/gcpemu start                       # in one terminal
cd console && pnpm install && pnpm dev # /console/ with hot reload; other paths go to the gateway
                                       # (GCPEMU_GATEWAY, default 127.0.0.1:4510)
pnpm lint && pnpm typecheck && pnpm test
pnpm gen:api                           # after editing internal/admin/openapi.yaml
make console-e2e                       # Playwright on Chromium, Firefox and WebKit
```

Each service is an isolated package implementing `emu.Service`
(`internal/emu/emu.go`); cross-service contracts live in `internal/emu/xservice.go`.

API route tables and field-behaviour validation (REQUIRED, OUTPUT_ONLY, IMMUTABLE) are
generated into `internal/apidef` from the googleapis commit pinned in
`internal/apidef/apis.yaml` and from the discovery documents of the `google.golang.org/api`
version in `go.mod`. gRPC services get REST through `internal/transcode`, and the gateway
checks every gRPC request against the generated tables. After a version bump, regenerate:

```sh
make generate                     # after go get google.golang.org/api@... or editing apis.yaml
make generate GOOGLEAPIS=latest   # move the googleapis pin to master, then regenerate
go test ./internal/apidef         # TestDrift: the pin and the Go modules must agree
```

Annotations a real API doesn't enforce go in the `relax` lists of `apis.yaml`, each with its reason.

Licensed under Apache-2.0.
