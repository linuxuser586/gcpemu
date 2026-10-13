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
| IAM | `services/iam` | Service accounts and keys, roles, policies with conditions, audit/enforce, OAuth2 token endpoint, IAM Credentials, metadata server, STS/WIF (OIDC exchange; AWS and SAML providers managed only), Service Usage (enabled APIs per project) |
| Cloud Storage | `services/gcs` | JSON + XML APIs, resumable uploads, generations/preconditions, checksums, V4 signed URLs, notifications, lifecycle, HMAC, legacy ACLs, managed folders with IAM |
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
                                                # (--beta adds a "google-beta" block)
bin/gcpemu console              # the Web console in the default browser
bin/gcpemu stop
```

Other commands: `reset`, `logs [service] [--requests]`, `time advance 24h`, `hosts`,
`fault add|list|clear`, `doctor`, `version`, `admin openapi` (the OpenAPI document of the
`/_emu/v1/` admin API, also served at `/_emu/v1/openapi.yaml`), and `admin token`.

### Binding beyond loopback

`--bind` (e.g. `0.0.0.0`) exposes every listener beyond this host, with a warning, and is
refused with IAM `off` unless `--i-understand-this-is-insecure` is passed. The admin API
(`/_emu/`) and the Web console then need an admin token (`docs/adr/0006-admin-token-guards-admin-api.md`):
a random one per start, or `GCPEMU_ADMIN_TOKEN` (at least 32 characters). `gcpemu start`
prints the console URL with the token on stderr, and opening it swaps the token for a cookie;
scripts send it in the `X-Gcpemu-Admin-Token` header. `gcpemu` commands on the same host
read it from the instance directory, `gcpemu admin token` prints it, and `gcpemu env`
exports it as `GCPEMU_ADMIN_TOKEN`. `GET /_emu/v1/health` and `/ready` stay open for probes,
and the public GCP APIs are governed by IAM as usual.

### Web console

The gateway serves a browser console at `http://127.0.0.1:4510/console/`; `gcpemu console`
opens it. It calls only the public APIs and the admin API, as the default principal, and
works offline. `--console=false` turns it off; under `CI=true` it is off unless `--console`
is passed. `make build` builds it when
`pnpm` is on PATH (Node.js 24); without it the binary serves a "console not built" page.
It reaches GKE clusters' Kubernetes objects and pod logs through the Connect gateway
(`/connectgateway/v1/projects/P/locations/L/gkeMemberships/CLUSTER/...`, see
`docs/adr/0002-kubernetes-api-through-connect-gateway.md`), which `kubectl` can use too.
Its Cloud SQL query runner uses `instances.executeSql` (the Data API), so an instance needs
`settings.dataApiAccess` set to `ALLOW_DATA_API`; statements run read-only unless unticked.
Unlike Cloud SQL, the emulator lets `executeSql` name a built-in user without a password.
Its Load Balancer view reads each forwarding rule's local listener from
`GET /_emu/v1/lb/listeners`, and "test a URL" asks `POST /_emu/v1/lb/route` which route and
backend of a URL map would serve a host, path and headers, without sending the request.
Its traffic tab (`/console/lb/traffic`, filtered by `?host=` and `?path=`) traces each request
live from the request log, whose `lb` entries carry the matched route, backend, endpoint, cache
status and backend latency; it shows the newest 500 that match.
Its Cloud CDN view treats a backend service or bucket with `enableCdn` as an origin: adding,
editing and removing one patches its `cdnPolicy` and `enableCdn`, and invalidation is
`urlMaps.invalidateCache`. Each origin's cached entries and hit ratio come from
`GET /_emu/v1/cdn` and `GET /_emu/v1/cdn/entries`, and "Purge all" is `POST /_emu/v1/cdn/purge`;
hit and miss totals survive a purge and are cleared by `gcpemu reset`.
Its Artifact Registry view lists repositories in every location by asking each one in turn, as
the API lists one location at a time. Deleting a digest deletes the tags on it too
(`versions.delete` with `force`). Pull commands name `LOCATION-docker.pkg.dev` when host mode is
ready and the registry port (`127.0.0.1:5000/LOCATION-docker.pkg.dev/...`) otherwise.
Its Cloud DNS view edits record sets only through `changes.create` (an edit deletes and re-adds
the record set in one change), and deleting a zone first deletes its record sets in one change.
Its query box asks `GET /_emu/v1/dns/query?name=N&type=T[&network=projects/P/global/networks/V]`,
which sends the query to the Instance's DNS server, from the host or as a client on that VPC
network, so private zones answer only on the networks they are bound to.
Its Cloud NAT view edits a NAT gateway by patching its router's `nats[]` with the whole list;
port usage is `routers.getNatMappingInfo`. Its translation log (`/console/nat/translations`,
filtered by `?src=`, `?dst=` and `?outcome=`) reads the request log's `nat` entries, which carry
each connection's source, destination, workload and NAT IP and port. The offline sink switch is
`GET`/`PUT /_emu/v1/nat/sink`; it lasts until the Instance stops.
Its IAM view edits the policy of a Project, service account, bucket, topic, subscription, AR
repository, secret, subnetwork or TLS policy through that resource's own `setIamPolicy`, carrying
the etag it read. Its policy simulator gets a token for the chosen Principal from the OAuth2
refresh token grant (the emulator takes `user:EMAIL` or `serviceAccount:EMAIL` as the refresh
token), calls the resource's `testIamPermissions` with it and revokes it. The IAM enforcement
level is switched with `PUT /_emu/v1/iam/enforcement` until the Instance stops. Its audit view
(`/console/iam/audit`, filtered by `?q=` and `?status=missing|granted`) lists
`GET /_emu/v1/iam/denials`: every call IAM found the caller lacking a permission for, once per
Principal, permission and resource, with the smallest predefined role that grants it and the
Project owning the resource. Grant binds that role on that Project through its `setIamPolicy`.
The log lives in memory until the Instance stops or is reset; `DELETE /_emu/v1/iam/denials`
clears it.

### Endpoints

All control-plane APIs are served by the gateway (default `127.0.0.1:4510`), under
`/<api>/` (for example `/pubsub/v1/…`, `/iam/v1/…`) and at their native root paths where
unambiguous (`/storage/v1/`, `/dns/v1/`, `/compute/v1/`, `/sql/v1beta4/`). Per-service ports:
GCS 4443, Pub/Sub 8085, registry 5000, DNS 5353 (UDP+TCP), metadata 8988, Cloud SQL host ports
54320+. `gcpemu env` also exports `KUBECONFIG` for GKE clusters. `--port name=0` picks a free port for one
listener and a bare `--port 0` for all of them (LB and Cloud SQL host ports included);
`--port-range 20000-20999` draws those free ports from a range. The choices are written to
`<data-dir>/endpoints.json` and printed by `gcpemu env`.

To reach a load balancer from another device (say, a phone on the LAN), pass
`--lb-bind <address>` (repeatable; `lbBind` / `GCPEMU_LB_BIND`). Every forwarding rule is then
also served on `<address>:<its port>`, while the admin API, the gateway and every other data
plane stay on `--bind`. Nothing else is exposed, so `--lb-bind` doesn't need
`--i-understand-this-is-insecure`. The emulator binds the port itself when it can. When the port is
privileged (`net.ipv4.ip_unprivileged_port_start` above it), a labelled `gcpemu-<id>-lb-lan`
container on the host network listens there and relays to the emulator. If two forwarding rules
share a port, only one gets the address. `gcpemu status` lists each rule's addresses as
`lb-lan:<name>`, and `gcpemu doctor` checks the addresses before start and lists them while running.

### Container runtime

Cloud SQL, GKE and Cloud NAT need Docker Engine (>= 24) or Podman (>= 4.9, Docker-compatible
socket; `DOCKER_HOST` is honoured). Everything the emulator creates is labelled
`gcpemu.instance=<id>`; containers are removed on `stop`, and with `--data-dir` the volumes
holding Postgres and Kubernetes state are kept so resources survive a restart. The emulator
copies its own (static) binary into containers as an in-container agent. GKE is Linux-only.

Cloud NAT knobs: `natReject` / `GCPEMU_NAT_REJECT=true` rejects uncovered egress immediately
instead of timing out; with `--offline` (or the offline sink switched on from the Web console),
NAT-allowed egress hits a sink that records the attempt and returns `natSinkResponse`
(`GCPEMU_NAT_SINK_RESPONSE`, default `503:gcpemu offline: egress blocked`).
`GCPEMU_GKE_MAX_NODES` caps nodes per instance (default 5).
`LoadBalancer` Services are given node addresses on the cluster's VPC by k3s's built-in service
load balancer (servicelb), the emulator's in-cluster L4 allocator (FR-GKE-009).

Pods reach an external load balancer by its public address, as on GCP: the emulated DNS answers
them with the forwarding rule's own IP (host clients still get the local listener), and the
`lb-edge` container serves those IPs on the external network. Nodes with external IPs route
them to it directly, and the backend sees the node's address; private nodes go through Cloud
NAT, so they need one, and the backend sees the NAT's address (ADR 0007).

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

Then trust the CA system-wide with `gcpemu ca install`. On Linux it also adds the CA to Docker's
and containerd's per-registry trust for every `LOCATION-docker.pkg.dev`, and, with `certutil`
(`libnss3-tools`), to the browser NSS databases: Chrome's `~/.pki/nssdb` and every native, snap
and flatpak Firefox profile. `gcpemu ca rotate` replaces it in those databases, and `gcpemu
doctor` reports where it is trusted. Or trust it per shell with
`eval "$(gcpemu env --trust)"`. That sets `SSL_CERT_FILE` to `<data-dir>/ca-bundle.pem`, which
holds the system roots plus the emulator CA. `gcpemu env` always exports `GCPEMU_CA_FILE`. To
test without changing the host, use `curl --resolve storage.googleapis.com:443:<ip> --cacert
"$GCPEMU_CA_FILE" …`, or give Go clients a `net.Resolver` that dials `<ip>:53`.

### Configuration

Flags > `GCPEMU_*` env vars > `gcpemu.yaml` > defaults. `CI=true` implies `--ephemeral`,
JSON logs and no Web console. IAM runs in `audit` mode by default (`--iam-mode off|audit|enforce`);
`PUT /_emu/v1/iam/enforcement` with `{"level":"enforce"}` switches it while the Instance runs.

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
Service is ready (`gcpemu status --ready`). Because it is not bound to loopback, the admin API
and the Web console need the admin token (see "Binding beyond loopback"): `docker logs gcpemu`
shows the console URL with it, `docker exec gcpemu /gcpemu admin token` prints it, and
`-e GCPEMU_ADMIN_TOKEN=...` sets it. Load balancer forwarding rules listen on their own ports, so publish
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
6. the bucket notification and the message arrive by push with a verified OIDC token; the
   pod then calls `https://app.example.test/api/health` itself, through Cloud NAT;
7. the pod reaches the internet through Cloud NAT, and neither the internet nor the load
   balancer after `nat_enabled=false`;
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

gcloud runs behind the same Escape guard as the OpenTofu acceptance (below), with `NO_PROXY`
covering loopback, because gcloud, unlike Go, proxies loopback too. A gcloud request with no
`CLOUDSDK_API_ENDPOINT_OVERRIDES_*` covering it fails the step and names the missing variable.
The `gcloud/escape-guard` subtest lists Pub/Sub topics without the pubsub override, and fails
unless the guard refuses `pubsub.googleapis.com`. The other clients run without the guard: they
either stay on loopback or reach the internet on purpose.

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

`tofu plan`, `apply` and `destroy` run behind an Escape guard: `HTTPS_PROXY` and `HTTP_PROXY`
point at an in-test proxy that refuses every request that would leave the machine. Each refused
host fails the module, and the error names it. The instance listens on loopback, which Go
never proxies and `NO_PROXY` exempts. `tofu init` runs without the guard so it can still download providers. An
`escape-guard` subtest per provider plans a data source whose `dns_custom_endpoint` is removed
from the provider block, and fails unless the guard refuses `dns.gcpemu.invalid`. The Reference
stack's OpenTofu runs use the same guard.

`gcpemu tofu-provider --beta` also prints a `provider "google-beta"` block for modules that use
`provider = google-beta`. It has the same endpoints, except that container's is
`/container/v1beta1/`. It is opt-in because a `google-beta` block makes OpenTofu require the
`hashicorp/google-beta` provider.

Both blocks set `universe_domain = "gcpemu.invalid"`. Without it, the provider looks up the
caller's email at `openidconnect.googleapis.com` every time it is configured. No endpoint
setting covers that URL, so each run calls Google, or stalls for about 10 minutes when there is no DNS.
An API that the block gives no endpoint is addressed as `<api>.gcpemu.invalid`, which never
resolves, so the call fails instead of reaching Google. Providers older than 7.2 still make the
lookup. Upgrade them, or set `HTTPS_PROXY` to an address that refuses connections.

Pointing gcloud at an instance needs nothing beyond `eval "$(gcpemu env)"` plus a token: for
example `echo owner > /tmp/tok; export CLOUDSDK_AUTH_ACCESS_TOKEN_FILE=/tmp/tok`. Registry
clients log in to `$GCPEMU_REGISTRY` as `oauth2accesstoken` with `gcloud auth print-access-token`.

## Development

```sh
make vet race      # gofmt, vet, tests with -race
make release       # static binaries for linux/darwin/windows × amd64/arm64, with the Web console
make cross         # the same binaries without building the console (no Node.js)
```

The Makefile needs GNU make (3.81 or later) and a POSIX shell. macOS ships both. On Windows,
run it from Git Bash with GNU make (`winget install ezwinports.make`); `make build` writes
`bin/gcpemu.exe` there, and `make race` needs a C compiler for cgo.

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
