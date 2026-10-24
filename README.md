# gcpemu

A single Go binary that emulates a subset of Google Cloud's Products on a workstation or in CI, so the
same OpenTofu modules, client SDKs and application code that target GCP run unmodified
except for Endpoint overrides. See [the SRS](GCP%20Local%20Emulator%20Software.md).

## Status

Milestones **M1** (core, IAM, Cloud Storage, Pub/Sub, Cloud DNS, Artifact Registry),
**M2** (compute networking, Cloud SQL, GKE, Cloud NAT) and **M3** (Application Load Balancer,
Cloud CDN, backend mTLS, Certificate Manager, Host mode) are implemented. The full SRS 11.2
Reference stack (steps 1–10) passes on linux/amd64 (`task e2e`).

| Area | Package | Highlights |
| --- | --- | --- |
| Core | `internal/…` | CLI, API gateway (REST + gRPC on one port), admin API, store (memory/bbolt), seed files, fault injection, request log |
| IAM | `services/iam` | Service accounts and keys, roles, policies with conditions, IAM v2 deny policies on projects, audit/enforce, OAuth2 token endpoint, IAM Credentials, metadata server, STS/WIF (OIDC exchange; AWS and SAML providers managed only), Service Usage (enabled APIs per project) |
| Cloud Storage | `services/gcs` | JSON + XML APIs, resumable uploads, generations/preconditions, checksums, V4 signed URLs, notifications, lifecycle, HMAC, legacy ACLs, managed folders with IAM |
| Pub/Sub | `services/pubsub` | gRPC + REST, streaming pull, ordering, DLQ, filters, push with OIDC, exactly-once, snapshots/seek, schemas; Cloud Monitoring's subscription metrics (`num_undelivered_messages`, `oldest_unacked_message_age`, `dead_letter_message_count`) through `timeSeries.list` |
| Secret Manager | `services/secrets` | gRPC + REST, global and regional secrets, versions and aliases, IAM, expiry and delayed destruction on the Emulator clock, Pub/Sub event notifications, managed rotation of Cloud SQL passwords |
| Cloud DNS | `services/dns` | Zones, record sets, changes, zone IAM policies; server policies (recorded); response policies applied to bound VPC networks; authoritative UDP/TCP server with forwarding |
| Artifact Registry | `services/ar` | Repositories API with Operations; OCI Distribution v1.1 registry with referrers; pull-through cache, remote and virtual repositories |
| Compute networking | `services/compute` | Networks, subnetworks, firewalls, routes, addresses, routers, zonal/regional/global NEGs, operations; servicenetworking; subnetworks realised as container networks; VPC peering (recorded) |
| Cloud NAT | `services/compute`, `services/nat` | Router NAT configs drive the NAT egress proxy container; drop/reject, logging, offline sink |
| Cloud SQL | `services/sql` | sqladmin v1beta4/v1 over real PostgreSQL 14–18 containers; connector (3307), IAM DB auth, authorized networks, private IP, backups (incl. the backups API and final backups on delete, and backups retained after it), point-in-time clone from archived WAL kept to `transactionLogRetentionDays` on the database clock (`gcpemu time advance` does not age the WAL archive or move the Recovery window), import/export, `executeSql` (Data API) |
| GKE | `services/gke` | container v1 (gRPC + REST) over real k3s clusters; node pools, IAM-backed kube auth, Workload Identity, private nodes via NAT, NEG sync, Ingress (`gce`) and Gateway API (`gke-l7-global-external-managed`, `gke-l7-gxlb`) controllers building load balancers, real-hostname Google APIs in pods; Connect gateway (`connectgateway` v1) to each cluster's Kubernetes API; Gateway API CRDs, the Secret Manager add-on (`secrets-store-gke.csi.k8s.io`) and SecretSync from cluster settings; system metrics over OTLP |
| Application Load Balancer | `services/lb` | Forwarding rules, proxies, URL maps, backend services/buckets, health checks, SSL certs/policies; in-process L7 proxy with frontend/backend mTLS, NEG and bucket backends, access logs; Cloud Armor policies, TCP/SSL/gRPC proxies and legacy health checks (recorded) |
| Cloud CDN | `services/cdn` | Cache modes, keys, TTLs, revalidation, negative caching, signed URLs/cookies, invalidation, LRU disk cache |
| Cloud Build | `services/build` | Builds API v1 (gRPC + REST, global and regional) with Operations; steps run in order or by `waitFor` as containers sharing `/workspace`, with `script`, `allowFailure`/`allowExitCodes` and volumes; `timeout` and `queueTtl` on the Emulator clock; `build.concurrency` (default 4) Builds at once; sources from Cloud Storage (`storageSource`, as `gcloud builds submit` uploads it) or a public Git URL (`gitSource`); step logs written as they run to `log-BUILD_ID.txt` in the Build's `logsBucket`; steps run as the Build's `serviceAccount` (the Compute Engine default without one; naming one needs `iam.serviceAccounts.actAs`) and reach the Instance by real hostname through the Build agent, with the metadata server, the Instance CA trusted and an Artifact Registry proxy; substitutions (`substitutionOption`, `dynamicSubstitutions`) and `availableSecrets.secretManager`; Build triggers (global and regional) run by `triggers.run`, Pub/Sub messages (`pubsubConfig`) and webhooks (`webhookConfig`), with SCM events Recorded; worker pools Recorded |
| Certificate Manager, Network Security | `services/certs` | Certificates (self-managed and managed via the local CA), maps, trust configs, DNS authorizations; backend authentication configs, server TLS policies |

## Development

Builds and tests run through [Task](https://taskfile.dev/installation) (3.54.0 or later), on
macOS, Linux and Windows:

```sh
task          # lists the tasks
task doctor   # checks the tools they need (Go, git, Node.js and pnpm, a container runtime, ...)
```

`task doctor` fails when a required tool (Task, Go, git) is missing or too old, and warns about
optional ones, naming the tasks each one blocks and how to install it. It checks the tools for
building and testing gcpemu; `gcpemu doctor` checks a host for running it.

## Quick start

```sh
task build
bin/gcpemu start --detach --services gcs,pubsub --seed seed.yaml
eval "$(bin/gcpemu env)"       # STORAGE_EMULATOR_HOST, PUBSUB_EMULATOR_HOST, GCE_METADATA_HOST, …
bin/gcpemu status
bin/gcpemu tofu-provider --project my-project   # provider "google" block with Endpoint overrides
                                                # (--beta adds a "google-beta" block)
bin/gcpemu console              # the Web console in the default browser
bin/gcpemu stop
```

Other commands: `wait --for KIND=TARGET` (see [Waiting for resources](#waiting-for-resources)),
`reset`, `logs [service] [--requests]`, `time advance 24h`, `hosts`,
`fault add|list|enable|disable|clear`, `snapshot save|restore|list|delete`, `export` (the
resources as a seed file), `doctor`, `prune`, `version`, `admin openapi` (the OpenAPI document of the
`/_emu/v1/` admin API, also served at `/_emu/v1/openapi.yaml`), and `admin token`.

`gcpemu status --ready` exits non-zero, and says why, until every Service is ready. It also fails, naming
them, while Managed containers that something other than the Instance stopped or restarted (a restart
of the container runtime, `docker stop` or `docker restart`) are down or have lost their setup: stop and
start the Instance to bring them back.

### Running one command

`gcpemu run` does the whole `start --detach`, `eval "$(gcpemu env)"`, command, `stop` sequence in one
step, and tears down even when the command fails or is interrupted:

```sh
bin/gcpemu run --services gcs,pubsub --seed seed.yaml -- go test ./...
```

`run` hosts the Instance in its own process, as `gcpemu start` without `--detach` does, so if `run` is
killed the Instance goes with it. It takes every `start` flag, with two other defaults: the Instance is
Ephemeral (`--ephemeral=false` keeps a Persistent Instance's state), and it logs at `warn`
(`--log-level=info` for the usual level), to stderr. It refuses to start while an Instance with the same
name is running.

The command starts once the Instance is ready, Seeding included. Its environment is the caller's plus the
variables `gcpemu env` prints, which win; `--trust` and `--no-universe` mean what they mean for `env`.
Ctrl+C and SIGTERM go to the command, and the Instance, with its Managed containers, is torn down once
the command has exited. `run` exits with the command's code, or 128+*n* on Unix when signal *n* killed it
(a teardown that fails is only a warning). It exits 1 without running the command when there is no
command after `--`, the command can't be found, the Instance fails to start, or it isn't ready within
`--wait-timeout`. To wait for resources too, the command can call `gcpemu wait`.

### Waiting for resources

`gcpemu wait` blocks until every Wait condition given with `--for` holds, so a script does not have to
poll `gcloud` in a loop:

```sh
bin/gcpemu wait --for gke/cluster=my-project/us-central1-a/c1 \
                --for lb/health=my-project/global/api --timeout 5m
```

A Wait condition is `KIND=PROJECT/LOCATION/NAME`:

| Kind | LOCATION | Met when | Fails when |
|---|---|---|---|
| `gke/cluster` | the cluster's zone or region | its status is `RUNNING` | its status is `ERROR` or `STOPPING` |
| `lb/health` | `global` or the backend service's region | every backend group has at least one `HEALTHY` endpoint, as `backendServices.getHealth` reports it | — |

A resource that does not exist yet keeps `wait` waiting, since another process may still be creating it,
and a backend service with no backends has not met `lb/health`. `wait` exits 0 once all of them are
met. It exits 1 straight away when one fails or no Instance is running, and once `--timeout` (default
`120s`) passes. When it fails, it names each Wait condition not met, and why, on stderr. A malformed
`--for`, or an unknown kind, is a usage error. `wait` asks the Admin API (`GET
/_emu/v1/conditions?for=...`), which is not subject to IAM.

### State snapshots

`gcpemu snapshot save NAME` saves all of a running Instance's state, and `snapshot restore NAME`
puts it back, undoing everything done since. A snapshot holds the control-plane store, GCS and
Artifact Registry blobs, and a `pg_dumpall` of every Cloud SQL instance with its backup files.
`snapshot list` and `snapshot delete NAME` manage them. Snapshots live in the instance dir under
`snapshots/`, beside the data directory, so an Ephemeral Instance's snapshots outlive it and a
later Instance of the same name restores them. That makes a snapshot a fast test fixture:

```sh
gcpemu start --detach --seed fixtures.yaml && gcpemu snapshot save clean
# ... run a test ...
gcpemu snapshot restore clean   # instead of re-seeding
```

Saving needs a quiet Instance: it fails while a Service is still starting, an Operation is
running, a Cloud SQL instance is stopped, or clients keep changing state during the save. Image
pulls through the GKE nodes' registry mirror only fill a cache, so they don't stop a save.
Restoring keeps configuration, Fault rules and the Emulator clock, which never goes back.
A RUNNING GKE cluster keeps its Kubernetes objects and its cluster CA: its k3s datastore is
saved, and its nodes rejoin and pull their images again. PersistentVolume contents are not saved,
so volumes come back empty, and a cluster that is not RUNNING at save time comes back with a fresh
data plane.

### Windows

Download `gcpemu-windows-amd64.exe` or `gcpemu-windows-arm64.exe` from a
[release](https://github.com/linuxuser586/gcpemu/releases), check it against `SHA256SUMS`
(`certutil -hashfile gcpemu-windows-amd64.exe SHA256`), and rename it to `gcpemu.exe` somewhere
on `PATH`. No installer, zip or administrator rights are needed.

`gcpemu.exe` runs every Service natively, Cloud SQL, GKE, Cloud NAT and Cloud Build included,
with Docker Desktop or Podman machine in Linux-containers mode as its container runtime
(ADR 0015). As on macOS, that runtime is a VM runtime: the emulator reaches its containers
through the VM relay, and you never install or enter a WSL2 distribution yourself. Without a
runtime, or with one serving Windows containers, the default Service list skips the Runtime
Services and logs why. Host mode is Linux-only and fails at start.

Windows has no privileged ports, so a forwarding rule binds its own port directly. A port in one
of Windows' excluded port ranges, which Hyper-V, WSL and others reserve, can't be bound at all: a
forwarding rule on one fails, pointing at `netsh int ipv4 show excludedportrange protocol=tcp`,
and `gcpemu doctor` lists the ports that fall in a range.

Running the Linux binary inside a WSL2 distribution also works: to the emulator, that is Linux.

State lives under `%LOCALAPPDATA%\gcpemu\<instance>` unless `XDG_STATE_HOME` or `--data-dir`
says otherwise. Windows doesn't enforce file modes such as 0600, so the per-user ACL on that
directory protects the CA key and the admin token.

`gcpemu env` prints bash by default, so from Git Bash run `eval "$(gcpemu env)"`. From
PowerShell, ask for PowerShell syntax (`pwsh` works too):

```powershell
gcpemu env --shell powershell | Out-String | Invoke-Expression
```

Windows PowerShell 5.1 reads a program's output in the console's OEM code page, so if a path
such as your profile folder has non-ASCII characters, run
`[Console]::OutputEncoding = [Text.Encoding]::UTF8` first. PowerShell 7 already reads UTF-8.

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

The API gateway serves the Web console at `http://127.0.0.1:4510/console/`; `gcpemu console`
opens it. It calls only the public APIs and the admin API, and works offline. It acts as
the default principal until you pick another user or service account in the header's
principal switcher, so the views show what IAM allows that principal (try it with
`--iam-mode enforce`). The choice is kept per browser and is not part of a shared link. `--console=false` turns it off; under `CI=true` it is off unless `--console`
is passed. `task build` builds it when
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
live from the request log, whose `lb` entries carry the matched route, backend, network endpoint, cache
status and backend latency; it shows the newest 500 that match.
Its Cloud CDN view treats a backend service or bucket with `enableCdn` as an origin: adding,
editing and removing one patches its `cdnPolicy` and `enableCdn`, and invalidation is
`urlMaps.invalidateCache`. Each origin's cached entries and hit ratio come from
`GET /_emu/v1/cdn` and `GET /_emu/v1/cdn/entries`, and "Purge all" is `POST /_emu/v1/cdn/purge`;
hit and miss totals survive a purge and are cleared by `gcpemu reset`.
Its Artifact Registry view lists repositories in every location by asking each one in turn, as
the API lists one location at a time. Deleting a digest deletes the tags on it too
(`versions.delete` with `force`). Pull commands name `LOCATION-docker.pkg.dev` when Host mode is
ready and the registry port (`127.0.0.1:4561/LOCATION-docker.pkg.dev/...`) otherwise.
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
A call an IAM v2 deny policy refused is listed separately, naming the policy (`denyPolicy`) and
no role, since no grant overrides a deny.
The log lives in memory until the Instance stops or is reset; `DELETE /_emu/v1/iam/denials`
clears it.
Its Fault rules view (`/console/faults`, filtered by `?service=` and `?q=`) lists
`GET /_emu/v1/faults` with each rule's hit count, adds rules with `POST /_emu/v1/faults`, and
enables or disables one with `PATCH /_emu/v1/faults/ID` and `{"disabled": true|false}`, as
`gcpemu fault enable|disable ID` do. A disabled rule keeps its hit count and does not fire; a
seed file's rule may start with `disabled: true`.
Its Request log view (`/console/requests`, filtered by `?service=`, `?method=`, `?principal=`,
`?status=` and `?resource=`) tails `GET /_emu/v1/requests` live, newest first, and keeps up to
100,000 rows in the browser. A status filter is a number (`404`), a class (`5xx`) or a code name
(`NOT_FOUND`). Expanding an entry reads `GET /_emu/v1/requests/ID`, which adds the request and
response bodies. The Instance keeps the first 16 KiB of each API call's bodies, and a unary gRPC
message as protojson, for as long as the entry stays among the last 2,000 it logs.
Its State view (`/console/state`) saves, restores and deletes State snapshots, resets the
Instance, applies a seed file chosen or pasted into it (`POST /_emu/v1/seed` with the YAML as
the body, whose relative paths resolve against the instance dir), and downloads the Instance's
resources as a seed file (`GET /_emu/v1/seed`, as `gcpemu export` prints). An exported seed file
holds Projects, each Service's resources and the Fault rules, with GCS object content inline;
server-assigned fields and data plane contents (Postgres rows, Kubernetes objects, Pub/Sub
messages) are left out.

### Service endpoints

All Control plane APIs are served by the API gateway (default `127.0.0.1:4510`), under
`/<api>/` (for example `/pubsub/v1/…`, `/iam/v1/…`) and at their native root paths where
unambiguous (`/storage/v1/`, `/dns/v1/`, `/compute/v1/`, `/sql/v1beta4/`). Per-Service ports:
GCS 4443, Pub/Sub 8085, registry 4561, DNS 5353 (UDP+TCP), metadata 8988, Cloud SQL host ports
54320+. With `--bind 127.0.0.1` or `localhost` (the default), each of these TCP listeners, the API
gateway's and the Google front end's also listen on `[::1]` at the same port, so a client that
resolves `localhost` to `::1` reaches the Instance and not another process holding the port on all
interfaces (macOS's AirPlay Receiver holds 5000 and 7000). `gcpemu env` also exports `KUBECONFIG`
for GKE clusters. `--port name=0` picks a free port for one listener and a bare `--port 0` for
all of them (LB and Cloud SQL host ports included); `--port-range 20000-20999` draws those free
ports from a range. The choices are written to
`<data-dir>/endpoints.json` and printed by `gcpemu env`.

Each load balancer forwarding rule's host listener is printed as
`GCPEMU_LB_<PROJECT>_<REGION|GLOBAL>_<NAME>` (for example `GCPEMU_LB_MY_PROJ_GLOBAL_WEB`), and
also as `GCPEMU_LB_<NAME>` while the name is unique across all projects and scopes (ADR 0013).
When a second rule takes the name, say a regional `web` beside a global one, `GCPEMU_LB_WEB` is
not printed rather than pointing at either rule; it comes back once only one rule has the name.
Two rules whose qualified variables come out the same (`-` and `/` both become `_`) print neither.
The emulator logs a warning when a variable is withheld, and `gcpemu doctor` lists it.

To reach a load balancer from another device (say, a phone on the LAN), pass
`--lb-bind <address>` (repeatable; `lbBind` / `GCPEMU_LB_BIND`). Every forwarding rule is then
also served on `<address>:<its port>`, while the admin API, the API gateway and every other Data
plane stay on `--bind`. Nothing else is exposed, so `--lb-bind` doesn't need
`--i-understand-this-is-insecure`. The emulator binds the port itself when it can. When the port is
privileged (`net.ipv4.ip_unprivileged_port_start` above it), a labelled `gcpemu-<id>-lb-lan`
container on the host network listens there and relays to the emulator. That fails on a VM
runtime, whose host network is the VM's. On macOS no container is used: a forwarding rule on a
port below 1024 listens on the wildcard address, which serves only loopback and `--lb-bind`
addresses. If two forwarding rules share a port, only one gets the address. `gcpemu status` lists each rule's addresses as
`lb-lan:<name>`, and `gcpemu doctor` checks the addresses before start and lists them while running.

To manage GKE clusters and push images from another machine (say, Argo CD and BuildKit in
another cluster on the LAN), pass `--gke-bind <address>` and `--ar-bind <address>` (both
repeatable; `gkeBind` / `GCPEMU_GKE_BIND`, `arBind` / `GCPEMU_AR_BIND`). As with `--lb-bind`,
the admin API and every other listener stay on `--bind`, and neither flag needs
`--i-understand-this-is-insecure` (`docs/adr/0011-lan-binding-requires-credentials.md`).

- `--gke-bind`: each cluster's Kubernetes API is also served on `<address>:<a port of its own>`,
  kept across restarts, and the API server's certificate covers the address.
  `<data-dir>/kubeconfig-lan` points at the first address with the cluster's admin client
  certificate, and `GET /container/_emu/kubeconfig?cluster=…&lan=true` (or `lan=<address>`)
  returns the same for one cluster. Service account tokens and emulator IAM access tokens work
  too. Anonymous requests are refused, and an unknown bearer token no longer maps to the
  default principal at any IAM enforcement level, on loopback as well.
- `--ar-bind`: the registry is also served on `<address>:<the registry's port>`, falling back
  to a free port when that one is taken. It serves TLS with a certificate from the instance CA,
  so clients trust `<data-dir>/ca.pem` (for docker, as
  `/etc/docker/certs.d/<address>:<port>/ca.crt`). Every request needs credentials, whatever the
  IAM enforcement level: `docker login -u oauth2accesstoken -p <access token>`, or `-u _json_key`
  with a service account key for a long-lived one such as BuildKit's. IAM permissions are then
  checked as on loopback. Clients are challenged for Basic credentials, because crane and ko
  refuse a token realm on a private IP address.

`gcpemu status` lists the addresses as `gke-lan:<context>` and `ar-lan`.

To reach Cloud SQL from another machine, pass `--sql-bind <address>` (repeatable; `sqlBind` /
`GCPEMU_SQL_BIND`). Every instance with a public IP is then also served there, and, as with
`--gke-bind`, nothing else is exposed and no `--i-understand-this-is-insecure` is needed:

- Postgres is served on `<address>:<a port of its own>`: the instance's loopback host port when
  that is free on the address, kept across restarts. `gcpemu status` lists it as
  `sql-lan:<connection name>`, so `psql "host=<address> port=<port> user=postgres"`. Postgres's
  own password authentication applies to built-in users, and IAM database users need an access
  token the Instance issued, whatever the IAM enforcement level. The connection reaches the
  instance as a host client's does, so authorized networks must admit `127.0.0.1/32` for it, as
  for psql on the emulator's machine (#639).
- Port 3307 on the address serves the Cloud SQL Auth Proxy and the connectors for every
  instance. They all dial `<address>:3307`, so it routes each connection by the TLS server name
  they send, the instance's `dnsName` (`<id>.<region>.sql.goog.`, which its server certificate
  names; `docs/adr/0028-connectors-reach-every-instance-on-a-lan-address-by-its-dnsname.md`). A connector that sends none reaches the address's only instance, if it has just one.
- A caller that arrives through the LAN, through `--gfe-bind` or at a `--bind` or `--sql-bind`
  address beyond loopback, is told `<address>` as the instance's only `ipAddresses` entry
  (`PRIMARY`), and no address for an instance with no public IP. Callers on the emulator's
  machine still see the instance's own addresses. With `--gfe-bind` too, the Auth Proxy on another
  machine works with an access token the Instance issued, for example
  `cloud-sql-proxy --token <token> <connection name>` with `sqladmin.googleapis.com` pointed at
  the `--gfe-bind` address.

For clients on another machine that call Google by its real hostnames and take no Endpoint override, such
as Argo Workflows' GCS artifact driver, pass `--gfe-bind <address>` (repeatable; `gfeBind` /
`GCPEMU_GFE_BIND`). The Google front end is then also served on `<address>:443`
(`--port gfe-lan=N` for another port), with certificates from the instance CA, for every name it
serves: `storage.googleapis.com`, `oauth2.googleapis.com`, `iam.googleapis.com`,
`LOCATION-docker.pkg.dev` and the rest. Point that machine's DNS for those names at the address
and have it trust `<data-dir>/ca.pem`. Port 443 is privileged on Linux: when an unprivileged
emulator can't bind it, a labelled `gcpemu-<id>-gfe-lan` container on the host network listens
there instead and relays each connection, still encrypted, to the Google front end. That needs a
container runtime that isn't a VM runtime, an emulator that isn't a Containerized Instance (a
container on the host's network qualifies) and no `--port gfe-lan`; otherwise run `sudo sysctl net.ipv4.ip_unprivileged_port_start=443`.
`gcpemu doctor` checks which applies. Nothing else is exposed (`docs/adr/0021-google-front-end-on-the-lan.md`):

- Every request needs credentials, whatever the IAM enforcement level: a bearer token the
  Instance issued, or a signed URL or V4 signature for the Cloud Storage XML API. The token
  endpoint (`https://oauth2.googleapis.com/token`) takes only a service account key's JWT-bearer
  grant, in the request body. The registry behaves as with `--ar-bind`, so containerd pulls with `_json_key`.
- A service account key created through a real hostname (through the Google front end, from the LAN,
  a pod or Host mode) has `token_uri` `https://oauth2.googleapis.com/token` and
  `universe_domain` `googleapis.com`, so it works unmodified on the other machine. Keys created at
  the API gateway's address keep the gateway's `token_uri` and the Invalid universe. To make one
  on the emulator's machine, call `iam.googleapis.com` through the Google front end in Host mode.
  With OpenTofu, set `iam_custom_endpoint = "https://iam.googleapis.com/v1/"` in the provider
  that creates the `google_service_account_key`.
- A read of a bucket the Instance doesn't have that carries none of the Instance's credentials,
  such as the Go module proxy's downloads from `proxy-golang-org-prod`, is a Read-through: the
  Instance reads it from real Cloud Storage. This is the one case in which the emulator calls Google, and it never does with
  `--offline`.
- The admin API and the Web console are not served there.

`gcpemu status` lists the addresses as `gfe-lan`.

### Linked Instances

A team can share one long-lived Instance on an office machine while each member keeps their own on a
laptop. Two Instances first pair, after which each accepts the other over a link
(`docs/adr/0027-instances-delegate-whole-projects-to-paired-instances.md`). Pairing is approved on
both sides, because it gives the other machine the authority to assert Principals.

The Instance that accepts Pairing requests, the office one here, passes `--link-bind <address>`
(repeatable; `linkBind` / `GCPEMU_LINK_BIND`). It then serves the link protocol on
`<address>:4511` (`--port link=N` for another port) over mutual TLS: each side presents a
certificate from its own Instance CA. Nothing else is served there, and every request but a
Pairing request needs a Pairing, so `--link-bind` needs no `--i-understand-this-is-insecure`.
`gcpemu status` lists the addresses as `link`.

```sh
# on the laptop: shows the office Instance's name, Instance ID and CA fingerprint, and asks to go on
gcpemu link pair 192.168.1.20
# on the office machine: shows the laptop's request, with its own fingerprint, and asks to approve it
gcpemu link requests
gcpemu link approve            # or: gcpemu link approve <request> / gcpemu link reject <request>
```

`link pair` waits up to 5 minutes for the approval; an Instance keeps at most 16 Pairing requests
waiting, and refuses more until one is decided. A newer request from the same Instance replaces its
older one, which is refused as superseded. Compare the fingerprints with the other
machine's owner before approving; `--yes` skips the questions, for scripts. Once approved, each side
pins the other's Instance CA, and names the link after the other Instance (`--name` for another
name, which is needed when two links would share one). The Admin API serves the same steps under
`/_emu/v1/links`, for the Web console.

- `gcpemu link list` lists the links. `gcpemu link unpair <name>` removes one; that side stops
  accepting the other at once. Unpair it on the other side too.
- An Instance that is not paired, or whose Instance CA no longer matches the one pinned, is refused,
  and the error says which. `gcpemu ca rotate` therefore breaks an Instance's links, and both sides
  report it, naming the link. A side calls a link broken only on what it saw itself: its own CA was
  rotated, or dialling the other Instance found another CA, which `link list` keeps showing, across
  restarts, until a dial finds the pinned CA again. A refused connection that only claims to be the
  other Instance, with another CA, is a warning on the link instead, since anyone could claim it; on
  a link the other side only connects in on, that warning is all this side can see of a rotation.
  Pairing again replaces the old link; the request then warns which link it replaces, so that a
  stranger claiming that Instance's ID is not approved by mistake.
- If `link pair` loses its connection just as the other side approves, only the other side keeps
  the link. Run `gcpemu link pair` again: the other side answers at once, since it is already
  paired.
- Instances negotiate a link protocol version. One with no version in common is refused, at Pairing
  and on connect, with an error naming both versions.
- Links are configuration, kept in `<data-dir>/links.json`: Reset and State snapshots leave them
  alone, and a Persistent Instance keeps them across restarts. An Ephemeral Instance keeps them in
  memory.
- `gcpemu doctor` reports each link: its state, whether the other Instance answers, and at which
  protocol version. A link whose other side has no `--link-bind` is inbound only.

#### Shared and Delegated Projects

Once paired, the office Instance shares Projects and the laptop delegates them. From then on the
laptop forwards every request naming a Delegated Project over the link, REST and gRPC, whatever
the Service, so `gcloud`, OpenTofu and SDK clients configured against the laptop work on the
office's Projects too:

```sh
# on the office machine: share with every paired Instance, including later ones, or with one: --with laptop
gcpemu link share team-shared
# on the laptop: the link must share it, and the laptop must not have a Project of that name
gcpemu link delegate team-shared --to office
```

- The laptop authenticates the caller, at its own IAM enforcement level, and asserts their
  Principal over the link; a caller with no credentials is the laptop's Default principal. The
  office checks IAM at its own level, as that Principal. A PERMISSION_DENIED from the office names
  the Principal the laptop asserted.
- A request names a Project by `projects/P` (an ID or number) in its REST path or in a gRPC request
  message's resource names (gzip-compressed or not), or, for the bucket collection
  (`gcloud storage buckets list`), by its `project` parameter, or for an XML API bucket by its
  `x-goog-project-id`. A request naming Projects that are not all delegated to one link, such as
  a local subscription to a delegated topic, fails with FAILED_PRECONDITION, since no one Instance
  holds both.
- Requests that name no Project are routed by what owns them:
  - **Buckets.** The office tells the laptop which buckets its Shared Projects own, and keeps that
    list current. A request on a bucket the laptop doesn't have goes to the office if the bucket is
    in a Delegated Project. That covers the JSON API, uploads (resumable ones included), downloads,
    the XML API and signed URLs, but not batches, which the laptop always serves. The laptop learns
    the list when it connects to the office, and `gcpemu link delegate` fetches it before it
    returns. Bucket names are global across the link: creating a bucket whose name the other side
    already holds fails with 409. Only the laptop checks this, and only against buckets in Projects
    shared with it, so the office can still create a name the laptop holds, and the laptop's own
    bucket then wins for requests through the laptop.
  - **Service accounts.** A service account under `projects/-` (`generateAccessToken`, `signBlob`,
    key creation) and the token endpoint's JWT-bearer grant are routed by the Project in the
    account's email. When a caller presents an access token the office issued, for a service
    account of a Project it shares with the laptop, the laptop asks the office whose it is and
    asserts that service account.
  - **Merged lists.** Resource Manager's Project lists (`gcloud projects list`) and REST lists under
    `projects/-` go to the laptop and to every link it delegates to. Each office answers with the
    items of the Projects it shares, the laptop keeps those it delegates, and it pages through the
    answers as one list, one Instance after another. If any office is unreachable, the whole list
    fails with UNAVAILABLE. gRPC lists, and Resource Manager's v3 `projects.list` by parent, which
    the emulator does not serve, are not merged.
- The office serves a forwarded request only if every Project it names is shared with that link,
  and never serves its Admin API or Web console over a link.
- The laptop holds no state for a Delegated Project: a reference to it never creates it there,
  and under `--strict-projects` it counts as existing, since its requests are forwarded. Anything
  that would still create state for it on the laptop, such as a request naming it only in its
  body, or a seed, fails with FAILED_PRECONDITION naming the link. A Project the laptop already
  has, or shares, cannot be delegated, so local state is never shadowed or merged.
- While the office is unreachable, requests for its Delegated Projects fail with UNAVAILABLE,
  naming the link; the laptop stays ready.
- The laptop's Reset and State snapshots act on its local Projects only, and the Delegated
  Projects' resources stay on the office. Shares and Delegations are configuration in `links.json`,
  like the links: they survive Reset. Unpairing a link drops the Shares with it and the Delegations
  to it.
- The laptop's Request log records each forwarded request, marked with the link it went over
  (`delegated` in the Admin API), and its fault rules still match it. Forwarded requests do not
  move the laptop's deterministic clock.
- `gcpemu link unshare <project>` (or `--with <link>`) and `gcpemu link undelegate <project>` undo
  them, and `gcpemu link list` lists the Shared and Delegated Projects. A Project unshared while
  still delegated fails with PERMISSION_DENIED until the laptop undelegates it. An Instance never
  delegates a Project it shares, nor shares one it delegates. The Admin API serves them under
  `/_emu/v1/links/shares` and `/_emu/v1/links/delegations`.

### External emulators

Firestore, Datastore, Spanner and Bigtable are not emulated, but Google ships emulators for them.
`--external <api>=<host:port>` (repeatable; `external` / `GCPEMU_EXTERNAL`, e.g.
`firestore=localhost:8080,spanner=localhost:9010`) makes the API gateway forward that API to such
an External emulator, or to any server speaking the API's wire format, so clients use one address
and Host mode covers the API too (`docs/adr/0027-instances-delegate-whole-projects-to-paired-instances.md`).
The key is the API name, the first label of its `googleapis.com` host. An API that a running
Service serves can't be delegated: `--external storage=…` fails at start because `gcs` serves it,
while `--external pubsub=…` works with `--services` leaving out `pubsub`.

```sh
gcloud emulators firestore start --host-port=localhost:8080 &
docker run -d -p 9010:9010 gcr.io/cloud-spanner-emulator/emulator
gcpemu start --external firestore=localhost:8080 --external spanner=localhost:9010
eval "$(gcpemu env)"
```

- gRPC, streaming included, goes through as it comes, for the API's `google.<api>.*` services
  and for `google.longrunning` calls naming one of its Operations (a Spanner database's, say).
  REST goes through under `/<api>/` on the API gateway, with the prefix taken off, and Firestore's
  and Datastore's REST paths also at the gateway's root, where their clients send them to
  `FIRESTORE_EMULATOR_HOST` and `DATASTORE_EMULATOR_HOST`. The External emulator must serve REST
  on the same address for REST clients: Spanner's emulator serves REST on a port of its own, so
  delegate it for gRPC clients.
- `gcpemu env` points `FIRESTORE_EMULATOR_HOST`, `DATASTORE_EMULATOR_HOST`,
  `SPANNER_EMULATOR_HOST`, `BIGTABLE_EMULATOR_HOST` or `PUBSUB_EMULATOR_HOST` at the API gateway
  for the APIs delegated. In Go tests, `emutest.WithExternal("firestore", addr)` does the same as
  the flag.
- In Host mode, and on `--gfe-bind`, the Google front end also serves the API's real hostnames
  (`firestore.googleapis.com`; for Bigtable also `bigtableadmin.googleapis.com`), so clients
  need no Endpoint override. On `--gfe-bind` a request still needs credentials the Instance
  issued before it is forwarded.
- No IAM check is made for a Delegated API and the emulator claims no Fidelity for it: whatever
  the External emulator does is what clients get. Reset, State snapshots, Fault injection and
  the Request log leave Delegated APIs out.
- An External emulator that can't be reached answers UNAVAILABLE, naming the API and its
  address. The Instance stays ready without it. `gcpemu doctor` reports each External emulator,
  and so do the Web console's dashboard and `GET /_emu/v1/external`.

### Container runtime

Cloud SQL, GKE, Cloud NAT and Cloud Build need Docker Engine (>= 24) or Podman (>= 4.9,
Docker-compatible socket). On macOS, Docker Desktop, OrbStack, Colima and Podman machine work too,
and on Windows Docker Desktop and Podman machine, in Linux-containers mode. The emulator uses
`DOCKER_HOST` first, then the Docker context (`DOCKER_CONTEXT`, or the current context in
`~/.docker/config.json`), then the first well-known endpoint that responds and serves Linux
containers: a socket, or on Windows the named pipes `//./pipe/dockerDesktopLinuxEngine`,
`//./pipe/docker_engine` and `//./pipe/podman-machine-default`, in that order. If `DOCKER_HOST` or a
context other than `default` names a runtime that doesn't respond, start fails rather than using
another one, so `gcpemu` and `docker` always agree. `gcpemu doctor` shows the runtime it picked
and why. Everything the emulator creates is labelled
`gcpemu.instance=<id>` and `gcpemu.dir=<data dir>`; containers are removed on `stop`, and with `--data-dir` the volumes
holding Postgres and Kubernetes state are kept so resources survive a restart. When a data dir
is deleted, or a run crashes and never starts again, nothing reclaims its objects: `gcpemu doctor`
warns about such abandoned Instance IDs, and `gcpemu prune` lists their containers, networks and
volumes and removes them once you confirm (`--yes` skips the question). An ID with a running
container is never touched.

On a VM runtime, host clients reach Cloud SQL instances, forwarding rules and GKE `LoadBalancer`
Services on loopback addresses: a `127.0.0.x` of their own each. A GKE cluster's endpoint is
`127.0.0.1` and a port the emulator serves, so `gcloud container clusters get-credentials` writes
a kubeconfig that works from the host. Linux and Windows route all of `127.0.0.0/8` to loopback,
so they need no loopback aliases, but macOS has only `127.0.0.1`, so there only one Cloud SQL
instance is reachable and forwarding rules and `LoadBalancer` Services share `127.0.0.1`: each on
its own port, or a free one when an earlier one holds it, as `gcpemu env` shows.
That needs no container runtime. `sudo gcpemu loopback install` adds `127.0.0.2`–`127.0.0.254` to `lo0` and installs
a LaunchDaemon that adds them again at boot; `sudo gcpemu loopback uninstall` removes both.
`gcpemu doctor` reports whether they are present. A Cloud SQL instance needs port 3307 free on its
address; 5432 is served there too while it is free, so a local PostgreSQL on `127.0.0.1:5432`
doesn't keep connectors out.

When a subnetwork's range overlaps another network on the host, its container network runs on a
free range instead and keeps the subnetwork's addresses on a VLAN of each container's interface
(a Translated network, ADR 0004). That needs VLAN (8021q) support in the runtime's kernel, which on
macOS is the VM's kernel. Without it, only the Translated network fails, with an error naming the
runtime; give the subnetwork a range that doesn't overlap, and everything else keeps working.

| Runtime | VLAN (8021q) support |
| --- | --- |
| Docker Desktop | supported (4.96.2) |
| Colima | supported (0.10.3) |
| OrbStack | supported (2.2.4) |
| Podman machine | supported (6.1.4) |

The emulator copies a static linux gcpemu binary into containers as a Sidecar agent, for the
runtime's architecture. On a linux host of that architecture it's its own executable. Otherwise
it uses `GCPEMU_AGENT_BINARY`, then `gcpemu-linux-<arch>` next to the executable, then the copy
cached in the instance directory under `agent/<version>/linux-<arch>/`. A release build (`vX.Y.Z`)
with none of these copies it out of the published image of the same version (`ghcr.io`, then
Docker Hub) and caches it. A dev build needs `task agent` instead. A binary for the wrong
architecture fails the start with an error naming both.

Cloud NAT knobs: `natReject` / `GCPEMU_NAT_REJECT=true` rejects uncovered egress immediately
instead of timing out; with `--offline` (or the offline sink switched on from the Web console),
NAT-allowed egress hits a sink that records the attempt and returns `natSinkResponse`
(`GCPEMU_NAT_SINK_RESPONSE`, default `503:gcpemu offline: egress blocked`).
Each workload a NAT serves gets a block of `minPortsPerVm` ports (default 64, 32 with dynamic
port allocation) on ports 1024-65535 of the NAT's IPs, in address order; AUTO_ONLY NATs gain IPs
as workloads join, and on MANUAL_ONLY a workload that doesn't fit gets no ports, so its egress is
dropped and `getRouterStatus` reports `minExtraNatIpsNeeded`. NAT IPs are emulated: the NAT egress
proxy translates onto the workload's port block on its own external address, reusing a port per
destination; endpoint-independent mapping caps a workload's concurrent connections at its block,
and dynamic port allocation lets it grow to `maxPortsPerVm`. Mapping info, status and NAT logs
name the NAT IP the block stands for.
`GCPEMU_GKE_MAX_NODES` caps nodes per instance (default 5).
A cluster whose `monitoring_config` enables `SYSTEM_COMPONENTS` exports its node, pod and
container CPU and memory usage, requests, limits, allocatable and restarts (with the last
termination reason, e.g. `OOMKilled`) over OTLP when the Instance's environment sets
`OTEL_EXPORTER_OTLP_ENDPOINT` (or `OTEL_EXPORTER_OTLP_METRICS_ENDPOINT`), using the standard
`OTEL_EXPORTER_OTLP_*` variables and `OTEL_METRIC_EXPORT_INTERVAL` (default 60000 ms). Names follow
the OTel Collector's `kubeletstats` and `k8s_cluster` receivers (FR-GKE-014).
`LoadBalancer` Services are given node addresses on the cluster's VPC by k3s's built-in service
load balancer (servicelb), the emulator's in-cluster L4 allocator (FR-GKE-009). On a VM runtime,
or when the cluster's subnetwork is a Translated network, the host can't reach node addresses,
so the emulator serves them instead: on a loopback address, reported as the Service's ingress
and by `gcpemu env` as `GCPEMU_GKE_<CLUSTER>_<NAMESPACE>_<SERVICE>_<PORT>`, and relayed to the
Service's node port. Only TCP ports are served. When the emulator itself runs in a container, see
"Container image".

Cloud Build knobs: `build.concurrency` in `gcpemu.yaml` (`GCPEMU_BUILD_CONCURRENCY`,
`--build-concurrency`, default 4) caps how many Builds are `WORKING` at once; the rest wait
`QUEUED` until their `queueTtl` passes. A Reset cancels every Build and removes its containers
and volumes, and keeps the Build daemon's cache; a Build that was `QUEUED` or `WORKING` when a State snapshot was saved restores as
`INTERNAL_ERROR`.

Build steps get the environment of a GKE pod from the Build agent, one Sidecar agent per Instance
on the Build network: `gcloud` and client-library steps need no Endpoint override. The agent is
the steps' DNS server and answers `metadata.google.internal`, the Google front end's hostnames
and every `LOCATION-docker.pkg.dev` with its own address, where it relays `:443` to the front end,
serves the metadata server as the step's Build's service account on `:80` and proxies Artifact
Registry over plain HTTP on `:5000` with that account's token.

Every step gets `/var/run/docker.sock` of the Build daemon (ADR 0016), one privileged
Docker-in-Docker container per Instance (`docker:29.9.0-dind`) started with the first Build, so
`gcr.io/cloud-builders/docker` and `docker` steps build, run and push images unmodified, and
`docker run --network=cloudbuild` reaches containers started earlier by name. Unlike on GCP, the
steps themselves are not on `cloudbuild`, so a step reaches such a container only from another
container on it, not by name from the step. The host runtime
only ever runs the daemon and the step containers. Its image cache is a volume that survives
Builds, Reset and restarts of a Persistent Instance, and is removed with the Instance's data;
unlike on GCP, Builds share it. `GET /_emu/v1/build/daemon` reports its size, and
`POST /_emu/v1/build/daemon/prune` empties it. The containers steps start in the daemon are
removed once no Build runs. The daemon reaches every `LOCATION-docker.pkg.dev` through the
Build agent's registry proxy, with the credentials the docker CLI sends it: every Build's
`/builder/home/.docker/config.json` holds its service account's for every Artifact Registry
location, as on Cloud Build. Once the steps succeed, `images` (and `artifacts.images`) are pushed
and listed with their digests in `results.images`, and the files `artifacts.objects.paths` match
in `/workspace` are uploaded under `artifacts.objects.location`, with the manifest
`artifacts-BUILD_ID.json`, `results.artifactManifest` and `results.numArtifacts`; Maven, Python,
npm and Go artifacts answer `UNIMPLEMENTED`. A runtime that refuses privileged containers, such
as Docker with userns-remap, fails every Build saying so, and `gcpemu doctor` flags it. Every step gets the Instance CA
bundle at `/etc/gcpemu/certs/ca-certificates.crt` and over `/etc/ssl/certs/ca-certificates.crt`,
`SSL_CERT_FILE`, `REQUESTS_CA_BUNDLE` and `NODE_EXTRA_CA_CERTS` set to it, and `GCE_METADATA_HOST`
and `GCE_METADATA_IP` set to the agent's address; unlike on GCP, the metadata server is not at
169.254.169.254, which steps cannot route to. The legacy `PROJECT_NUMBER@cloudbuild.gserviceaccount.com`
exists in every Project with Cloud Build Service Account (`roles/cloudbuild.builds.builder`) on it.

A Build's source is a `storageSource` tarball (gzipped, plain or zip), extracted into `/workspace`
at the `generation` given (a generation that is gone fails the Build `FETCH_SOURCE_FAILED`), or a
`gitSource` cloned at its `revision` (branch, tag or commit) with the `alpine/git` image. Other
sources answer `UNIMPLEMENTED`. Every step's output goes to `log-BUILD_ID.txt` in the Build's
`logsBucket` while it runs, under its path prefix if it has one; without one the Build gets
`gs://PROJECT_NUMBER.cloudbuild-logs.googleusercontent.com`, which the emulator creates.
`options.logging` is recorded, and the log always goes to Cloud Storage. `logUrl` opens the Build in
the Web console.

Substitutions expand when the Build is created, in each step's `name`, `args`, `env`, `dir` and
`entrypoint`, in `images`, `tags`, `logsBucket`, `artifacts` and each `availableSecrets` `versionName`,
and the Build shows the expanded values. The built-ins are `$PROJECT_ID`, `$PROJECT_NUMBER`, `$BUILD_ID`,
`$LOCATION`, `$SERVICE_ACCOUNT` and `$SERVICE_ACCOUNT_EMAIL`. The source and Build trigger ones, such as
`$COMMIT_SHA` and `$BRANCH_NAME`, are empty unless something sets them. User `$_KEY` and `${_KEY}`
substitutions expand too, and `$$` is a literal `$`. As on GCP, a `script` isn't substituted. Under
`options.substitutionOption: MUST_MATCH` (the default), an unused user substitution or a `$_KEY`
with no value fails `builds.create` with `INVALID_ARGUMENT`. `ALLOW_LOOSE` allows both. A `$KEY` that
isn't a built-in always fails. `options.dynamicSubstitutions` enables bash parameter expansion
(`${_SHA:0:7}`, `${_REF##*/}`, `${_X/a/b}`, `${_X,,}`, ...) and lets user values refer to other
substitutions. `availableSecrets.secretManager` versions are read as the Build's service account
when it starts, so under `enforce` the account needs `secretmanager.versions.access`. A version
that can't be read fails the Build. Each payload is set as its `env` in the steps that list it in
`secretEnv` (or in `options.secretEnv`), and shows as `[REDACTED]` in the log, along with each line of
a multi-line payload that is 8 characters or longer. Unlike on GCP as far as is known, this masking
is deliberate emulator behaviour. Cloud KMS `secrets` and `availableSecrets.inline` answer
`UNIMPLEMENTED`.

Build triggers are created, read (by ID or name), listed, patched and deleted at
`projects/*/triggers` and `projects/*/locations/*/triggers`. A Build trigger's Build is its inline
`build`, or the Build config file its `filename` (with `sourceToBuild`) or `gitFileSource` names,
read from the public Git repository with the `alpine/git` image. Its source is `sourceToBuild`, or
else the `build`'s own `gitSource` or `storageSource`; `triggers.run` builds at the branch, tag or
commit its source names, and adds that source's `substitutions`. A `pubsubConfig` Build trigger makes
the push subscription `gcb-TRIGGER` (`gcb-LOCATION-TRIGGER` for a regional one) to its topic,
reported in `pubsubConfig.subscription` and deleted with the Build trigger, and starts one Build per
message, a Cloud Storage notification to the topic included. `triggers.webhook` starts a Build of a
`webhookConfig` Build trigger when its `secret` query parameter is the payload of the Build trigger's
Secret Manager secret version, read as `service-PROJECT_NUMBER@gcp-sa-cloudbuild.iam.gserviceaccount.com`;
a wrong or missing secret is refused with `PERMISSION_DENIED`. The Build trigger's `substitutions` bind
to the push or webhook body, as on GCP: `$(body.message.data)` (the decoded data as sent, with JSON
data walked further, as in `$(body.message.data.tag)`), `$(body.message.attributes.KEY)`, `$(body.ref)`. Its
Builds get `$TRIGGER_NAME`, `$TRIGGER_BUILD_CONFIG_PATH` and, from the Git revision they build,
`$BRANCH_NAME`, `$TAG_NAME`, `$REF_NAME`, `$COMMIT_SHA`, `$REVISION_ID`, `$REPO_NAME` and
`$REPO_FULL_NAME`, `buildTriggerId` and the tag `trigger-ID`. `github`, `triggerTemplate` and
`repositoryEventConfig` are Recorded: no SCM event fires them, and running one without
`sourceToBuild` answers `UNIMPLEMENTED`, as do `autodetect` and Cloud Source Repositories. `filter`,
`includedFiles` and `ignoredFiles` are Recorded. Worker pools are Recorded: they are created,
patched and deleted through Operations, and a Build naming one in `options.pool.name` runs on the
Build daemon like any other. A Seed file's `build` section declares Build triggers and worker pools,
which `gcpemu export` writes back.

gcloud can't stream the log outside Host mode (SRS 11.3): it reads it from
`storage.UNIVERSE_DOMAIN`, ignoring endpoint overrides, so `gcloud builds submit` fails even when
the Build succeeds, and `--suppress-logs` doesn't help, since gcloud still reads the log. Pass
`--async`, or set `options.logging: NONE` in the build config, which makes gcloud wait without
reading the log and exit non-zero for a failed Build. Then read the log with
`gcloud storage cat gs://BUCKET/log-BUILD_ID.txt`, in the Web console or at `logUrl`. In Host mode,
`eval "$(gcpemu env --no-universe)"` points gcloud at `storage.googleapis.com`, and the log streams.

Pods reach an external load balancer by its public address, as on GCP: the emulated DNS answers
them with the forwarding rule's own IP (host clients still get the local listener), and the
`lb-edge` container serves those IPs on the external network. Nodes with external IPs route
them to it directly, and the backend sees the node's address; private nodes go through Cloud
NAT, so they need one, and the backend sees the NAT's address (ADR 0007).

### Kubernetes Services from the host

A process on the host can call a GKE cluster's Kubernetes Services by name, as its pods do (ADR
0014), on Linux, macOS and native Windows (ADR 0015). The emulator's DNS (Service endpoint `dns`,
`127.0.0.1:5353` by default) answers host queries for

- `<service>.<namespace>.svc.<cluster>.<location>.<project>.gke.internal`, always, and
- `<service>.<namespace>.svc.cluster.local`, while exactly one cluster has that Service. When a
  second cluster adds a Service of the same name, the short name stops resolving instead of
  picking one; the emulator logs a warning and `gcpemu doctor` lists the collision. It resolves
  again once only one cluster has it.

The answer is a Loopback alias (`127.0.0.x`) the Service gets on the first query and keeps until
it's deleted, also across `gcpemu stop` and `start` of an Instance with a data dir. The emulator
serves each of the Service's TCP ports there, so `curl http://web.default.svc.cluster.local:8080`
reaches a pod, on Linux and on a VM runtime (as on macOS and Windows) alike. Headless Services
aren't answered; ExternalName Services are answered with their CNAME. Queries from the VPC (pods,
through their node's DNS relay) are unaffected.

Send the two domains to the emulator's DNS with `gcpemu dns install`, which adds a Resolver route
for each one. Add a `--suffix` for each emulated Cloud DNS zone the host should resolve too:

```sh
gcpemu dns install                                # svc.cluster.local and gke.internal
gcpemu dns install --suffix example.dev.test      # adds a public zone's DNS name
gcpemu dns uninstall --suffix example.dev.test    # removes that one route
gcpemu dns uninstall                              # removes every route this Instance added
```

Like `gcpemu loopback`, it prints the commands, asks before running them (`--yes` skips that,
`--dry-run` only prints them) and runs only that step as root: through sudo on Linux and macOS,
and through UAC on Windows, whose dialog shows even with `--yes`. It doesn't need the Instance to
be running, and it reads the Instance's `dns` port. Running it again adds to the routes the
Instance has and moves them all to that port. It records what it changed in the user config
directory, so `uninstall` removes only that. It refuses a suffix another Instance routes, or one
that configuration it didn't write already covers, and then changes nothing.

- **Linux** (systemd-resolved): the routes are on a dummy link that the command creates,
  `gcpemu-dns0` (`gcpemu-dns1` and on for other Instances) (ADR 0026). They last until reboot; `gcpemu doctor` reports routes that are gone. Without
  systemd-resolved it refuses and prints the address and suffixes to set up by hand.
- **macOS**: one `/etc/resolver/<suffix>` file per suffix.
- **Windows**: one NRPT rule per suffix. NRPT can't name a port, so start the Instance with
  `--port dns=53` first; `install` refuses otherwise. Then `Resolve-DnsName web.default.svc.cluster.local`
  answers from the emulator.

`gcpemu status` lists the Instance's Resolver routes, and `gcpemu doctor` reports any that are gone
or point at another port.

Aliases come from the `127.0.0.2`–`127.0.0.254` pool that Cloud SQL instances and forwarding rules
also use. On macOS run `sudo gcpemu loopback install` first; Linux and Windows route all of
`127.0.0.0/8` already and need no install. When no alias is free, or on a Mac without them, the DNS
answers SERVFAIL and the emulator logs why; `gcpemu doctor` flags both. `gcpemu status` lists the
Services that hold aliases. The host must allow binding a Service's port: Linux refuses ports below
1024 to non-root users unless `net.ipv4.ip_unprivileged_port_start` allows them, and those ports
aren't served. Windows has no privileged ports, but a Service port in one of Windows' excluded port
ranges isn't served on its alias (`netsh int ipv4 show excludedportrange protocol=tcp` lists them).
A Containerized Instance has no host loopback, so it doesn't answer these names.

### VPC workloads reaching the host

A process on the host can stand in for a pod (ADR 0014), on Linux, macOS and native Windows (ADR
0015): offer its port, and workloads on a subnetwork reach it at the subnetwork's Host address,
which forwards to `127.0.0.1` on the host. The process only needs to listen on loopback.

```sh
gcpemu offer 8080 53/udp --subnetwork projects/my-project/regions/us-central1/subnetworks/default
# prints the Host address, e.g. 10.128.0.1: point an EndpointSlice at 10.128.0.1:8080
gcpemu host-address --subnetwork projects/my-project/regions/us-central1/subnetworks/default
gcpemu withdraw 8080 --subnetwork projects/my-project/regions/us-central1/subnetworks/default
```

Where the host routes to the network (Linux), the Host address is the network's gateway and the
emulator binds the offered port there, so offering a port that a process holds on `0.0.0.0` or
`[::]` fails: have it listen on `127.0.0.1` instead. On a VM runtime (as on macOS and Windows) the
VM relay holds the address. Ports that aren't offered aren't reachable. The Host address and the
offers survive `gcpemu stop` and `start` of an Instance with a data dir, and `gcpemu status` lists
them.

### Real hostnames: GKE pods and host mode

The emulator includes a Google front end (Service endpoint `frontend`, a dynamic port unless you
pass `--port frontend=N`). It terminates TLS for the real hostnames the API gateway serves
(`storage.googleapis.com`, `pubsub.googleapis.com`, `sqladmin.googleapis.com`,
`iamcredentials.googleapis.com`, `oauth2.googleapis.com`, …) and for every
`LOCATION-docker.pkg.dev`. Certificates come from the instance CA (`<data-dir>/ca.pem`). It then
passes HTTP/1.1, HTTP/2 and gRPC through to the emulator unchanged. APIs the emulator doesn't
serve (`logging.googleapis.com`, …) are never redirected.

**GKE pods** use real hostnames automatically. Each node answers DNS for the served names with
169.254.169.254 and relays port 443 there to the Google front end. A mutating admission webhook mounts a
bundle of the host's system roots plus the emulator CA at `/etc/ssl/certs/ca-certificates.crt`
and `/etc/gcpemu/certs/`. It also sets `SSL_CERT_FILE`, `REQUESTS_CA_BUNDLE`,
`NODE_EXTRA_CA_CERTS` and `GCE_METADATA_HOST`. Unmodified Go, Python, Java and Node clients get
Workload Identity tokens from the metadata server and reach the emulated Services without any
Endpoint override. To opt a namespace or a pod out, label it `gcpemu.dev/inject=disabled`;
`kube-system` is always skipped. Nodes also trust the CA in their own system store.

Containers that a pod starts itself, such as the ones BuildKit runs for a Dockerfile's `RUN`
lines, get the cluster's DNS but not the CA, so `storage.googleapis.com` fails TLS in them.
`go mod download` fails this way, because proxy.golang.org redirects every download to a bucket
there. Label the pod that starts them `gcpemu.dev/googleapis=real`. The webhook then points the
pod's DNS, which its containers inherit, at a resolver on the node that resolves
`*.googleapis.com` through Cloud DNS to real Google, as on GCP. Every other name, including
`LOCATION-docker.pkg.dev`, still resolves through the cluster. The pod's calls to every
`googleapis.com` API are then deliberately Escaped requests, so the label suits public data and
anonymous calls only; on private nodes they need Cloud NAT. A pod that sets `dnsPolicy: None`
keeps its own DNS, and a `dnsConfig` of its own is merged in. The label does nothing in
`kube-system` or with `gcpemu.dev/inject=disabled` (ADR 0019).

```yaml
metadata:
  labels:
    gcpemu.dev/googleapis: real   # e.g. on the BuildKit pod
```

**Host mode** (`gcpemu start --host-mode`, Linux hosts only: `start` refuses it on macOS and Windows,
in a Containerized Instance on a VM runtime, and on a Linux host whose runtime is a VM runtime, such as Docker Desktop,
where the VM relay is on) does the
same for host processes without root. It starts a labelled container (`gcpemu-<id>-hostmode`) that the host reaches at its
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

Then trust the CA with `gcpemu ca install`. On Linux and macOS it goes in the system trust store;
on Linux it also goes in Docker's and containerd's per-registry trust for every
`LOCATION-docker.pkg.dev`, and, with `certutil` (`libnss3-tools`), in the browser NSS databases:
Chrome's `~/.pki/nssdb` and every native, snap and flatpak Firefox profile. containerd's trust is a
`/etc/containerd/certs.d/HOST/hosts.toml` file. A kubelet on the host pulls through containerd's
CRI, which reads it only when CRI's `config_path` includes that folder, which by default is so only
from containerd 2.2. Otherwise `ca install` and `gcpemu doctor` print the `config.toml` change to
make, followed by `sudo systemctl restart containerd`. Emulated GKE clusters are not affected:
their nodes pull with their own registry settings. Colima and Podman
machine keep their own registry trust inside their VM, so for them `ca install` prints the
command to run there; Docker Desktop and OrbStack use the macOS keychain. Docker Desktop reads
it only when it starts, so `ca install` and `ca uninstall` tell you to restart it. On Windows it
adds the CA to the current user's ROOT certificate store with `certutil -user -addstore Root`,
which needs no administrator rights. Windows shows a security dialog to confirm the change, and
another for `gcpemu ca uninstall`; choose Yes. `gcpemu ca rotate` replaces the CA in the browser NSS
databases, and `gcpemu doctor` reports where it is trusted: the OS store, the browser NSS databases,
and Docker's and containerd's registry trust; for a VM runtime it notes that it cannot check and
repeats `ca install`'s advice. `ca install --cert FILE` and
`ca uninstall --cert FILE` (`-` for stdin) do the same for another CA certificate, such as a
Containerized Instance's (see "Trusting the CA on the host"). The Web console's Certificate Manager
view rotates a running Instance's CA too (Rotate CA), but changes no Host trust: run `gcpemu ca install`
afterwards. Or trust it per shell with
`eval "$(gcpemu env --trust)"`. That sets `SSL_CERT_FILE` to `<data-dir>/ca-bundle.pem`, which
holds the system roots (on Windows, exported from the ROOT certificate store) plus the emulator
CA. `gcpemu env` always exports `GCPEMU_CA_FILE`. To test without changing the host, use `curl
--resolve storage.googleapis.com:443:<ip> --cacert "$GCPEMU_CA_FILE" …`, or give Go clients a
`net.Resolver` that dials `<ip>:53`.

### Configuration

Flags > `GCPEMU_*` env vars > `gcpemu.yaml` > defaults. `CI=true` implies `--ephemeral`,
JSON logs and no Web console. The IAM enforcement level is `audit` by default (`--iam-mode off|audit|enforce`);
`PUT /_emu/v1/iam/enforcement` with `{"level":"enforce"}` switches it while the Instance runs.
IAM v2 deny policies (`google_iam_deny_policy`, `/iam/v2/policies/…/denypolicies`) are checked
before any allow policy and bind every Principal they name, the Default principal included;
internal calls with no Principal are exempt. Under `audit` a denied call is logged and allowed.
Denial conditions on `request.time` and `resource.*` are evaluated; conditions on resource tags
are refused, since the emulator has no tags.

### Deterministic Instances

`--deterministic` (`emutest.WithDeterministic()`) makes generated IDs and timestamps the same run
to run, for golden-file tests. The clock starts at 2026-01-01T00:00:00Z and moves 1 ms each time
an API request arrives; every timestamp within one request is the same, and real time passing
never moves it. `gcpemu time advance` still moves it on purpose. The guarantee holds for a client
that sends one API request at a time. Console and admin API traffic (`/_emu/`) does not move the
clock, so it does not affect the output. Operations with no `--lro-latency` come back `DONE` from
the call that started them. Operations backed by runtime containers (GKE clusters, Cloud SQL
instances, backups, imports and exports) and any operation once latency is configured are outside
the guarantee, and so are Delegated Projects: the guarantee covers local Projects only.

### Seed files

Top-level keys are `projects`, `faults` and Service IDs (`iam`, `compute`, `gcs`, `pubsub`,
`dns`, `ar`, `sql`, `gke`). Each Service documents its section in its `seed.go`.

### Go tests

`github.com/linuxuser586/gcpemu/emutest` runs an in-process Instance with the Services you name
(plus their dependencies), on dynamic ports, Ephemeral by default. `emutest.Start(t, …)` runs one
for a test and stops it at the test's cleanup:

```go
inst := emutest.Start(t, []string{"gcs", "pubsub", "secrets", "dns"})

sc := inst.Storage(t)              // *storage.Client, closed at cleanup
ps := inst.PubSub(t, "my-project") // *pubsub.Client (cloud.google.com/go/pubsub/v2)

// Endpoint overrides for any gRPC client library: the API gateway, no authentication, insecure transport.
sm, err := secretmanager.NewClient(ctx, inst.GRPCOptions()...)

// Endpoint overrides for any REST client library, by API name: "storage", "compute", "dns", "sqladmin", …
opts, err := inst.HTTPOptions("dns") // fails for an API the Instance doesn't serve
svc, err := dns.NewService(ctx, opts...)

inst.Setenv(t) // or the client env vars (STORAGE_EMULATOR_HOST, …) for this test
```

To share one Instance across a package's tests, start it in `TestMain` with `emutest.New` and
stop it with `Close`, which also removes its temporary data dir:

```go
var inst *emutest.Instance

func TestMain(m *testing.M) {
	ctx := context.Background()
	var err error
	if inst, err = emutest.New(ctx, []string{"gcs"}); err != nil {
		log.Fatal(err)
	}
	code := m.Run()
	_ = inst.Close(ctx)
	os.Exit(code)
}
```

Options, for `Start` and `New`:

| Option | Effect |
| --- | --- |
| `WithSeed(path)` | Applies a Seed file before the Instance is ready |
| `WithDataDir(dir)` | A Persistent Instance in `dir` instead of an Ephemeral one |
| `WithOperationLatency(serviceID, d)` | The Service's Operations complete after `d` (`--lro-latency`) |
| `WithStrictProjects()` | Refuses undeclared projects (`--strict-projects`) |
| `WithOffline()` | Never reaches the internet (`--offline`) |
| `WithIAMMode(mode)` | `off`, `audit` or `enforce` |
| `WithDeterministic()` | A Deterministic Instance (see above) |
| `WithConsole()` | Serves the Web console |
| `WithSQLHostClients()` | Cloud SQL host clients on loopback (below) |

Set `GCPEMU_TEST_LOG=1` to see the emulator's logs. `RequireRuntime(t)` skips a test without a
container runtime; GKE tests call `HoldClusterSlot(t)` to limit how many clusters run at once
(`GCPEMU_TEST_CLUSTERS`, default 2).

On a VM runtime, Cloud SQL instances started by `emutest` serve no host clients on a loopback
address, so a test reaches each one on its published port (`GCPEMU_SQL_<INSTANCE>`).
`emutest.WithSQLHostClients()` serves them as `gcpemu` does, for tests that dial the address an
instance reports. On a host without loopback aliases, such a test waits until no other `emutest`
test of the same user is serving on `127.0.0.1`.

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

The image runs on Linux, macOS and Windows hosts. In PowerShell, continue lines with a backtick
instead of `\`:

```powershell
docker run -d --name gcpemu -p 4510:4510 -p 4443:4443 -p 8085:8085 `
  -v gcpemu:/data linuxuser586/gcpemu:0
docker exec gcpemu /gcpemu status
```

The image is Linux-only, as are k3s, PostgreSQL and the Sidecar agent it starts (ADR 0015), so
Docker Desktop in Windows-containers mode can't run it: `docker run` fails with "no matching
manifest". Choose "Switch to Linux containers" in Docker Desktop's menu first.

The image binds `0.0.0.0` at IAM enforcement level audit, keeps state in `/data` (mount a volume to
keep it across containers, or set `GCPEMU_EPHEMERAL=true`), and reports healthy once every
Service is ready (`gcpemu status --ready`). Because it is not bound to loopback, the admin API
and the Web console need the admin token (see "Binding beyond loopback"): `docker logs gcpemu`
shows the console URL with it, `docker exec gcpemu /gcpemu admin token` prints it, and
`-e GCPEMU_ADMIN_TOKEN=...` sets it.

`docker exec gcpemu /gcpemu doctor` checks the container, not the host: CA trust and the
host-only checks (loopback aliases, Windows' excluded port ranges) are notes for the host, and
every fix names a `docker run` flag or a host command.

`--lb-bind`, `--gke-bind`, `--ar-bind`, `--gfe-bind`, `--sql-bind` and `--link-bind` serve the LAN
only from the host's network. On Linux, run the image with `--network host` and they work as documented. On a bridge
network they bind only the container's own addresses, which only container networks reach, and
the emulator warns at start. Publishing a port on a LAN address is no substitute: the published
listeners are `--bind`'s, which don't require credentials, and the certificates don't name the
host's address. On a VM runtime (Docker Desktop, Podman machine), use the native binary instead
(`docs/adr/0011-lan-binding-requires-credentials.md`).

#### Trusting the CA on the host

The container creates its own CA in `/data`, which the host doesn't trust, so HTTPS from the host
to load balancers and the Google front end fails verification until you trust it. Print the CA
certificate and install it with the native binary on the host:

```sh
docker exec gcpemu /gcpemu ca print > gcpemu-ca.pem
gcpemu --instance container ca install --cert gcpemu-ca.pem
```

`podman exec` works the same, as does `docker cp gcpemu:/data/ca.pem gcpemu-ca.pem`; `--cert -`
reads the certificate from stdin, and then needs `--yes`, since stdin can't also confirm.
`--cert` needs `--instance`, which names the trust entries, so the container's CA doesn't replace
a native instance's; `ca install --cert` trusts it in the same stores as `gcpemu ca install`
and never touches an instance data directory. Keep `gcpemu-ca.pem`: remove the trust with
`gcpemu --instance container ca uninstall --cert gcpemu-ca.pem`, or run that before removing the
container or its volume. After `ca rotate` in the container, uninstall with the old file, then
print the new certificate and install it again. A persistent `/data` volume keeps the same CA
across containers, while a container without one has a new CA each time. To trust it per client
without installing it, use `curl --cacert gcpemu-ca.pem` or `SSL_CERT_FILE=gcpemu-ca.pem`.

A load balancer forwarding rule listens on a loopback address inside the container, which `-p`
never forwards to, so each rule is also served on a port the container publishes (ADR 0012), and
`gcpemu env` prints the host side of that port as the rule's `GCPEMU_LB_*` variables (see "Service
endpoints"). Without the Docker socket,
publish each rule's port at the same host port (`-p 127.0.0.1:8080:8080`); the first rule on a
port is served there. With the socket, the emulator reads its own published ports: a rule takes
its own port when the container publishes it (`-p 127.0.0.1:28080:8080` gives
`GCPEMU_LB_<NAME>=127.0.0.1:28080`, while the name is unique), and otherwise a free port of
`--port-range` when that range is published (see below). A rule with no free published port has
no `GCPEMU_LB_*` variable; the
emulator logs what to publish, `gcpemu doctor` lists it, and it is served once a port frees.

A container on the host's network (`--network host`) isn't a Containerized Instance, and runs as
on the host. When the runtime can't tell (no Docker socket, or it can't find the emulator's
container), set `--containerized=false` to say so (see below).

Cloud SQL, GKE and Cloud NAT start containers of their own, so they need the host's Docker
socket (Docker-outside-of-Docker). Without the socket the image starts every other Service and
logs which it skipped. With it, the emulator stays unprivileged and non-root; it needs only the
socket's group:

```sh
docker run -d --name gcpemu -p 4510:4510 -p 4443:4443 -p 8085:8085 \
  -v /var/run/docker.sock:/var/run/docker.sock \
  --group-add "$(docker run --rm -v /var/run/docker.sock:/s busybox:1.37 stat -c %g /s)" \
  linuxuser586/gcpemu:0 start --ephemeral
```

```powershell
docker run -d --name gcpemu -p 4510:4510 -p 4443:4443 -p 8085:8085 `
  -v /var/run/docker.sock:/var/run/docker.sock `
  --group-add "$(docker run --rm -v /var/run/docker.sock:/s busybox:1.37 stat -c %g /s)" `
  linuxuser586/gcpemu:0 start --ephemeral
```

The `--group-add` line reads the socket's group the way the runtime mounts it, so the same line
works from bash, zsh and PowerShell on every host: on a VM runtime the socket inside the VM
belongs to `root`, and the line gives `0`. In Git Bash, MSYS path conversion rewrites every
container path in these examples, so run `export MSYS_NO_PATHCONV=1` first, or double each
leading slash (`//var/run/docker.sock`, `//s`, `//gcpemu`).

Podman serves the same API on a socket of its own (best-effort): put its path in place of the
host side of both `-v` mounts above, keeping `/var/run/docker.sock` and `/s` in the container.
Rootless Podman on Linux serves `$XDG_RUNTIME_DIR/podman/podman.sock`; a rootful Podman machine
on macOS and Windows (`podman machine set --rootful`) serves `/run/podman/podman.sock`. Where
SELinux enforces, as in a Podman machine, add `--security-opt label=disable` to both `docker run`
lines, or SELinux denies the container the socket (`stat: can't stat '/s': Permission denied`).

The emulator finds its own container and joins the networks it creates (taking each
subnetwork's reserved second-to-last address), so it works on a normal bridge or compose
network as well as with `--network host`. Privileged GKE nodes get their privilege from the
Docker daemon, not from the emulator's container. From the host:

- Cloud SQL instances are published on `127.0.0.1` (`GCPEMU_SQL_*` ports, as on the host).
  The Cloud SQL Auth Proxy and connectors dial an instance's `PRIMARY` address on port 3307:
  off a VM runtime its IP on the emulator's bridge networks, which a Linux host reaches
  directly. On a VM runtime, see below.
- `GET /container/_emu/kubeconfig?cluster=projects/P/locations/L/clusters/C` returns a
  kubeconfig whose server is the cluster's API published on `127.0.0.1`; the Connect gateway
  (`/connectgateway/v1/...`) works through the API gateway port.
- A GKE `LoadBalancer` Service's ingress is a node address that a Linux host reaches directly:
  servicelb's on an ordinary network, and on a Translated network every Ready node's
  Fallback-range address, served by the cluster itself (`gcpemu env` prints the first). On a VM
  runtime, see below.
- Use `--ephemeral` (or a volume on `/data`): stopping an ephemeral instance removes all of its
  containers, networks and volumes, while a persistent one keeps them for its next start.

On a VM runtime (Docker Desktop, OrbStack, Colima, Podman machine; every runtime on a macOS or
Windows host) the host can't route to container addresses inside the VM, so the emulator serves
each GKE `LoadBalancer` Service on a port its own container publishes (ADR 0012), as it does
forwarding rules on any runtime. Start it with `--port-range` and publish that range on the same
container and host ports:

```sh
docker run -d --name gcpemu -p 127.0.0.1:4510:4510 -p 127.0.0.1:20000-20019:20000-20019 \
  -v /var/run/docker.sock:/var/run/docker.sock \
  --group-add "$(docker run --rm -v /var/run/docker.sock:/s busybox:1.37 stat -c %g /s)" \
  linuxuser586/gcpemu:0 start --ephemeral --port-range 20000-20019
```

```powershell
docker run -d --name gcpemu -p 127.0.0.1:4510:4510 -p 127.0.0.1:20000-20019:20000-20019 `
  -v /var/run/docker.sock:/var/run/docker.sock `
  --group-add "$(docker run --rm -v /var/run/docker.sock:/s busybox:1.37 stat -c %g /s)" `
  linuxuser586/gcpemu:0 start --ephemeral --port-range 20000-20019
```

A Service takes its own port when the container publishes it at the same host port
(`-p 127.0.0.1:8080:8080`), and otherwise a free port of the range. Its ingress is the host IP
of that binding (`127.0.0.1`), and the port appears only in `gcpemu env`
(`GCPEMU_GKE_<CLUSTER>_<NAMESPACE>_<SERVICE>_<PORT>`) and the Web console. Only TCP ports are
served. When no published port is free, the Service has no ingress and the emulator logs a
warning saying what to publish. Forwarding rules without their own published port take ports
from the same range, and whoever binds a port first gets it. In any Containerized Instance
with a `--port-range`, the Google front end's dynamic port stays out of the range, so the range is
left for Services and forwarding rules. A container on the host's network takes it from the range,
as on the host.

The emulator guesses from the runtime's info that it is on a VM runtime: Docker Desktop (on any
host) and OrbStack are recognized. Set `GCPEMU_VM_RUNTIME=true` (or `--vm-runtime true`) on
Colima and Podman machine, or `false` to keep node addresses; the default is `auto`. Podman
machine on Windows can't be recognized either: its WSL2 kernel looks the same as Docker Engine
installed inside a WSL distribution, which isn't a VM runtime from that distribution.
`docker exec gcpemu /gcpemu doctor` prints the decision, warns when no `--port-range` port is
published, lists forwarding rules with no published port, and flags clusters last started under
the other decision; restarting the emulator recreates their servers with the current one.

On a VM runtime each Cloud SQL instance with a public IP publishes port 3307, and 5432 when it
is free, on a `127.0.0.x` of its own, and reports that address as its `PRIMARY` IP, so the Auth
Proxy, the connectors and `psql` work from the host as with the native binary (ADR 0012). Every
connector dials port 3307, so instances can't share an address, and the emulator can't see from
inside its container whether the host has the loopback aliases `127.0.0.2`–`127.0.0.254`.
`GCPEMU_LOOPBACK_ALIASES` (`--loopback-aliases`, `loopbackAliases`) says whether to use them:

- `auto` (the default) uses them on Docker Desktop, and falls back to `127.0.0.1` when Docker
  Desktop refuses a loopback alias the host lacks. Elsewhere it uses only `127.0.0.1`: OrbStack and
  Colima accept a loopback alias binding and never forward it, so the address would time out.
- `true` uses them on any runtime, with the same fallback.
- `false` uses only `127.0.0.1`.

With only `127.0.0.1`, one public instance at a time serves host clients there, and creating or
starting a second fails with `FAILED_PRECONDITION` naming the first. Linux and Windows hosts
already have the loopback aliases: all of 127.0.0.0/8 is loopback there, so on a Windows host
`true` is safe on any runtime. A macOS host has none until `sudo gcpemu loopback install` is run
from the native binary. Once the host has them, set `GCPEMU_LOOPBACK_ALIASES=true` on VM
runtimes other than Docker Desktop. A local PostgreSQL on `127.0.0.1:5432` doesn't get in the
way: the instance is then served on 3307 only.
`gcpemu doctor` prints the decision next to the VM-runtime one, each public instance's host
address, and whether the runtime has refused a loopback alias. Instance containers are recreated on
every start, so a changed setting takes effect when the emulator restarts.

A container on the host's network runs as on the host. It isn't a Containerized Instance, so
forwarding rules aren't served on published ports, the Google front end's dynamic port comes
from `--port-range`, the gfe-lan relay serves `--gfe-bind`'s port 443, and `gcpemu doctor` checks
the `--*-bind` addresses and the unprivileged port start as the host's. With the Docker socket,
the emulator asks the runtime to find its own container and whether that container is on the
host's network. When the runtime finds it, the runtime's answer always wins. Without the socket, or when the runtime can't find the container,
`GCPEMU_CONTAINERIZED` (`--containerized`, `containerized`) decides:

- `auto` (the default) treats any container as a Containerized Instance, and logs a warning
  saying so. `gcpemu doctor` warns about it too. Forwarding rules are then served as without the
  socket.
- `true` treats the emulator as a Containerized Instance.
- `false` declares a container on the host's network, which runs as on the host.

When `true` or `false` disagrees with the runtime, the runtime wins and the emulator logs a
warning naming both.

As a GitHub Actions service container (the job waits for the health check):

```yaml
services:
  gcpemu:
    image: linuxuser586/gcpemu:0
    ports: ["4510:4510", "4443:4443", "8085:8085"]
env:
  STORAGE_EMULATOR_HOST: http://localhost:4443
  PUBSUB_EMULATOR_HOST: localhost:8085
```

### Verifying a release

Each release is a signed release: its release workflow signs it with keyless cosign. `SHA256SUMS.sigstore.json`
signs `SHA256SUMS`, which covers every binary. Each binary also has an SPDX SBOM
(`gcpemu-<os>-<arch>.spdx.json`) and SLSA provenance, recorded as GitHub attestations. With
[cosign](https://docs.sigstore.dev/cosign/system_config/installation/) and the
[GitHub CLI](https://cli.github.com/), in the directory you downloaded `SHA256SUMS`,
`SHA256SUMS.sigstore.json` and the binary to:

```sh
cosign verify-blob --bundle SHA256SUMS.sigstore.json \
  --certificate-identity-regexp '^https://github.com/linuxuser586/gcpemu/.github/workflows/release.yml@refs/tags/v' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com SHA256SUMS
sha256sum -c --ignore-missing SHA256SUMS
gh attestation verify gcpemu-linux-amd64 --repo linuxuser586/gcpemu
```

The image is signed by digest in both registries (replace `X.Y.Z` with the version):

```sh
cosign verify \
  --certificate-identity-regexp '^https://github.com/linuxuser586/gcpemu/.github/workflows/release.yml@refs/tags/v' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com docker.io/linuxuser586/gcpemu:X.Y.Z
cosign verify \
  --certificate-identity-regexp '^https://github.com/linuxuser586/gcpemu/.github/workflows/release.yml@refs/tags/v' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com ghcr.io/linuxuser586/gcpemu:X.Y.Z
```

A release whose signing fails is never published. If signing the image fails, its tags are
already pushed but unsigned, and `cosign verify` fails for them until the maintainer re-runs the
image job. See "Signing" in [docs/release.md](docs/release.md).

### Reference stack (SRS 11.2)

`e2e/` holds the Reference stack, the final acceptance test of the whole emulator: one OpenTofu root module
(`e2e/stack/`), a Go API (`e2e/app/`) and `TestReferenceStack`, which drives them against an
in-process emulator at IAM enforcement level `enforce`:

1. `tofu apply` creates a VPC and subnet (pod/service ranges), Cloud Router + NAT, a private
   GKE cluster with Workload Identity and a node pool, an Artifact Registry repo, buckets
   (static assets, uploads with a Pub/Sub notification), a topic with an OIDC push
   subscription, Cloud SQL Postgres 16 on a private IP with an IAM service account user,
   the `example.test.` zone and a global external ALB with Cloud CDN (backend bucket, NEG
   backend service with backend mTLS, managed certificate, HTTP→HTTPS redirect); a plan
   right after must be empty;
2. the app is built for amd64 and arm64 FROM scratch and pushed to AR, Istio is installed from
   its Helm charts (its ingress gateway requires the LB's client certificate), the app is deployed
   and a second apply (`api_neg_name`) adds the ingress gateway's GKE NEG to the load balancer;
3. `https://app.example.test/` (emulated DNS, emulator CA; Go client and curl) serves the
   bucket's page and the second request reports `X-Cache-Status: hit`;
4. `/api/health` goes LB → Istio → app; a client without the LB certificate is refused;
5. `/api/work` writes to GCS, inserts into Cloud SQL (Go connector, IAM auth) and publishes,
   using Workload Identity and no Endpoint override;
6. the bucket notification and the message arrive by push with a verified OIDC token; the
   pod then calls `https://app.example.test/api/health` itself, through Cloud NAT;
7. the pod reaches the internet through Cloud NAT, and neither the internet nor the load
   balancer after `nat_enabled=false`;
8. `app_storage_access=false` makes step 5 fail with `PERMISSION_DENIED`;
9. `urlMaps.invalidateCache` on `/*` makes the next request a `miss`;
10. `tofu destroy` leaves no containers (`gcpemu status` lists the instance's containers).

```sh
task e2e                          # = GCPEMU_NET_TESTS=1 go test -tags e2e ./e2e -timeout 30m -v
GCPEMU_NET_TESTS=0 task e2e       # offline-ish: steps 1, 3, 9 and 10 only
```

`task e2e` runs the whole `e2e` package: the Reference stack and the Containerized Instance tests,
which build the OCI image and drive a Containerized Instance from the host. Those need Docker and
`GCPEMU_NET_TESTS=1`, and skip themselves otherwise. The 10-minute budget is the Reference stack's alone.

It needs a container runtime and `tofu` on `PATH`; `tofu init` downloads the `google` and `tls`
providers. Steps 2 and 4–8 download Helm and the Istio release for its charts (both pinned and
checksum-verified) and the Istio images and reach `https://example.com/`, so they only run with `GCPEMU_NET_TESTS=1`.
Downloads are cached in `$GCPEMU_E2E_CACHE` (default `<user cache dir>/gcpemu-e2e`). The run
must finish within 10 minutes (`GCPEMU_E2E_BUDGET` overrides); it prints per-step timings.
With `emulator_gateway` empty the module targets real GCP (set `project` and `domain`).

### Client compatibility (SRS 7.1)

`task compat` runs the real clients against a detached instance with only Endpoint
overrides: `gcloud` (storage, pubsub, secrets, iam, dns, compute load balancing and NAT, artifacts,
sql, container), `kubectl` through `gke-gcloud-auth-plugin`, Helm, Docker, `crane`, `ko`, `psql`,
the Cloud SQL Auth Proxy v2 (including automatic IAM authentication) and `dig`. gcloud (with the
`gke-gcloud-auth-plugin` component), kubectl, dig and Docker come from `PATH`; Helm, crane, ko
and the Auth Proxy are downloaded at pinned, checksum-verified versions.

gcloud runs behind the same Escape guard as the OpenTofu acceptance (below), with `NO_PROXY`
covering loopback, because gcloud, unlike Go, proxies loopback too. A gcloud request with no
`CLOUDSDK_API_ENDPOINT_OVERRIDES_*` covering it fails the step and names the missing variable.
gcloud's universe descriptor fetch from `storage.gcpemu.invalid` is not counted as an escape.
The `gcloud/escape-guard` subtest lists Pub/Sub topics without the pubsub override, and fails
unless the guard refuses `pubsub.gcpemu.invalid`. The other clients run without the guard: they
either stay on loopback or reach the internet on purpose.

### OpenTofu acceptance (SRS 11.1, IF-001)

`task tofu` applies each module in `compat/tofu/` (one per service area: IAM, Storage, Pub/Sub,
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
`provider = google-beta`. It has the same Endpoint overrides, except that container's is
`/container/v1beta1/`. It is opt-in because a `google-beta` block makes OpenTofu require the
`hashicorp/google-beta` provider.

Both blocks set `universe_domain = "gcpemu.invalid"`. Without it, the provider looks up the
caller's email at `openidconnect.googleapis.com` every time it is configured. No Endpoint
override covers that URL, so each run calls Google, or stalls for about 10 minutes when there is no DNS.
An API that the block gives no Endpoint override is addressed as `<api>.gcpemu.invalid`, which never
resolves, so the call fails instead of reaching Google. Providers older than 7.2 still make the
lookup. Upgrade them, or set `HTTPS_PROXY` to an address that refuses connections.

Pointing gcloud at an instance needs nothing beyond `eval "$(gcpemu env)"` plus a token: for
example `echo owner > /tmp/tok; export CLOUDSDK_AUTH_ACCESS_TOKEN_FILE=/tmp/tok`. Registry
clients log in to `$GCPEMU_REGISTRY` as `oauth2accesstoken` with `gcloud auth print-access-token`.

`gcpemu env` also sets `CLOUDSDK_CORE_UNIVERSE_DOMAIN=gcpemu.invalid`, in Host mode too. A gcloud
API with no `CLOUDSDK_API_ENDPOINT_OVERRIDES_<API>` then goes to `<api>.gcpemu.invalid`, which
never resolves, so the command fails instead of reaching Google. gcloud also refuses credentials
stored for Google (`gcloud auth login`, or a key from real Google) with a universe mismatch
error, so they are never sent to the instance; use the token file above, or a key the emulator
issued. Outside Google's universe gcloud behaves differently in a few places:

- `gcloud compute images list` and `gcloud compute instances create` without `--image*` flags fail
  looking up the universe descriptor at `storage.gcpemu.invalid`. Pass `--no-standard-images`, or
  `--image-family` with `--image-project`.
- `gcloud storage buckets create` requires `--location`.

`gcpemu env --no-universe` leaves that variable out, for a shell that also points gcloud at Google
with stored credentials.

For the client libraries, `gcpemu env` sets `GOOGLE_CLOUD_UNIVERSE_DOMAIN=gcpemu.invalid` (which
`--no-universe` keeps). A Go client, or a generated gRPC client such as Pub/Sub in Python or Node,
then sends an API with no endpoint override to `<api>.gcpemu.invalid` instead of Google. The Python
and Node Cloud Storage clients ignore the variable, so they still need `STORAGE_EMULATOR_HOST`.

`gcpemu env` also sets `GOOGLE_API_USE_MTLS_ENDPOINT=never`, which `--no-universe` keeps. In
`always` mode, or when a client certificate is found, a Go client dials a hard-coded
`<api>.mtls.googleapis.com`, which the universe does not change. If you set
`GOOGLE_API_USE_MTLS_ENDPOINT=always` after `eval "$(gcpemu env)"`, a Go client for an API with no
endpoint override reaches Google. The Python client libraries refuse mTLS outside Google's
universe, so they fail instead. The Node client libraries build the mTLS host from the universe,
so in `always` mode they stay on the unresolvable `<api>.gcpemu.invalid`, and with
`GOOGLE_API_USE_CLIENT_CERTIFICATE=true` they refuse mTLS outside Google's universe.

Service-account keys the emulator issues say `"universe_domain": "gcpemu.invalid"`. A Go client
that loads one needs `GOOGLE_CLOUD_UNIVERSE_DOMAIN=gcpemu.invalid`, even outside a `gcpemu env`
shell; without it the first request fails with a universe mismatch. With it, Go clients sign their
own JWTs from the key instead of calling its `token_uri`, which the instance also accepts. Some
clients compare the key with Google's universe whatever the variable says:

- Python Cloud Storage with credentials passed explicitly: pass
  `client_options={"universe_domain": "gcpemu.invalid"}`.
- Node Pub/Sub, including with `PUBSUB_EMULATOR_HOST`: pass `universeDomain: 'gcpemu.invalid'`
  with `emulatorMode: true`, since any `universeDomain` otherwise turns emulator mode off.
- An OpenTofu `provider "google"` block that loads a key: set `universe_domain = "gcpemu.invalid"`,
  as the `gcpemu tofu-provider` blocks do.

The metadata server that `gcpemu env` points host processes at is in the Invalid universe too.
Python google-auth otherwise looks up each account's Regional Access Boundary at
`iamcredentials.googleapis.com`, which no Endpoint override covers (ADR 0009). ADC through it
needs nothing else, except that Python Cloud Storage with those credentials passed explicitly
needs the same `client_options` as an emulator key. In GKE pods the metadata server stays in
Google's universe, and the instance answers the lookup. On a machine where someone ran
`gcloud auth application-default login`, ADC uses a key instead, as below.

Credentials the instance did not issue, such as a key from real Google or gcloud's
application-default login, send their token requests to `oauth2.googleapis.com`. Go HTTP clients,
Python Cloud Storage with credentials passed explicitly and the Node REST fallback fetch a token
before they check the universe, so that request reaches Google even when every API has an
endpoint override. ADC finds gcloud's `application_default_credentials.json` before the metadata
server, so when it exists and `GOOGLE_APPLICATION_CREDENTIALS` is unset, `gcpemu env` exports
`GOOGLE_APPLICATION_CREDENTIALS` pointing at an emulator key for the metadata server's service
account (ADR 0010). It never overrides a `GOOGLE_APPLICATION_CREDENTIALS` you set, but it warns,
as `gcpemu doctor` does, when that file is not in the Invalid universe. In host mode those token
requests reach the instance instead, which refuses a refresh token Google issued (one starting
`1//`, or sent with gcloud's application-default client ID) with 400 `invalid_grant` rather than
taking it as the default Principal.

The Cloud SQL Auth Proxy v2 puts a static `--token` in Google's universe, so in a `gcpemu env`
shell it refuses the token with a universe mismatch. Its `--universe-domain` cannot go with
`--sqladmin-api-endpoint`, so run it with `GOOGLE_CLOUD_UNIVERSE_DOMAIN=` or with
`--credentials-file` and an emulator key.

## Development

```sh
task agent         # the Sidecar agent, bin/gcpemu-linux-<arch> (needed off linux)
task vet race      # gofmt, vet, tests with -race
task release       # static binaries for linux/darwin/windows × amd64/arm64, with the Web console
task cross         # the same binaries without building the console (no Node.js)
```

The tasks in `Taskfile.yml` need Task 3.54 or later, from any shell (brew, winget, or
`go install github.com/go-task/task/v3/cmd/task@v3.54.0`), and only the tools each task calls
(go, git, pnpm, docker, npm); `task doctor` checks them. `task build` writes `bin/gcpemu.exe` on
Windows. `task race` needs a C compiler for cgo.

The Web console (`console/`) is Vite + React + TypeScript; `console/embed.go` embeds its
`dist/`. With Node.js 24 and pnpm:

```sh
bin/gcpemu start                       # in one terminal
cd console && pnpm install && pnpm dev # /console/ with hot reload; other paths go to the API gateway
                                       # (GCPEMU_GATEWAY, default 127.0.0.1:4510)
pnpm lint && pnpm typecheck && pnpm test
pnpm gen:api                           # after editing internal/admin/openapi.yaml
task console-e2e                       # Playwright on Chromium, Firefox and WebKit
```

Each Service is an isolated package implementing `emu.Service`
(`internal/emu/emu.go`); cross-service contracts live in `internal/emu/xservice.go`.

API route tables and field-behaviour validation (REQUIRED, OUTPUT_ONLY, IMMUTABLE) are
generated into `internal/apidef` from the googleapis commit pinned in
`internal/apidef/apis.yaml` and from the discovery documents of the `google.golang.org/api`
version in `go.mod`. gRPC services get REST through `internal/transcode`, and the API gateway
checks every gRPC request against the generated tables. After a version bump, regenerate:

```sh
task generate                     # after go get google.golang.org/api@... or editing apis.yaml
task generate GOOGLEAPIS=latest   # move the googleapis pin to master, then regenerate
go test ./internal/apidef         # TestDrift: the pin and the Go modules must agree
```

Annotations a real API doesn't enforce go in the `relax` lists of `apis.yaml`, each with its reason.

Licensed under Apache-2.0.
