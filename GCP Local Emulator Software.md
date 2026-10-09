# GCP Local Emulator — Software Requirements Specification

Oct 7, 2026 · @Barry

## 1. Introduction

### 1.1 Purpose

This SRS defines the requirements for `gcpemu`, a single Go binary that emulates eleven Google Cloud services well enough that the same OpenTofu modules, Kubernetes manifests, application code and client SDKs that target real GCP can be run, unmodified except for endpoint configuration, on a developer workstation and in CI. It is written for the people who will build, test and accept the emulator.

### 1.2 Scope

In scope are the control-plane APIs and a working data plane for these services:

| Service | GCP API | Emulated data plane |
| --- | --- | --- |
| IAM | `iam.googleapis.com`, `iamcredentials.googleapis.com`, `sts.googleapis.com`, resource `getIamPolicy`/`setIamPolicy` | Token issuance, policy evaluation, metadata server |
| Cloud DNS | `dns.googleapis.com` v1 | Authoritative DNS server |
| Artifact Registry | `artifactregistry.googleapis.com` v1 | OCI Distribution (Docker) registry |
| Cloud Storage | JSON API v1 and XML API | Object store on local disk |
| Pub/Sub | `pubsub.googleapis.com` v1 (gRPC and REST) | Message broker with pull, streaming pull and push |
| Cloud SQL for PostgreSQL | `sqladmin.googleapis.com` v1 | Real PostgreSQL servers |
| GKE | `container.googleapis.com` v1 | Real Kubernetes clusters running containers |
| Application Load Balancer | `compute.googleapis.com` v1 (forwarding rules, proxies, URL maps, backend services/buckets, health checks, NEGs, SSL certs) | In-process L7 proxy |
| Cloud CDN | Cache settings on backend services and buckets; `urlMaps.invalidateCache` | HTTP cache in the L7 proxy |
| Cloud NAT | `compute.googleapis.com` v1 routers and NAT configs | Egress gateway for cluster workloads |
| Secret Manager | `secretmanager.googleapis.com` v1 (gRPC and REST), global and regional endpoints | Secret payload store with Pub/Sub event notifications and Cloud SQL credential rotation |

Supporting resources those services depend on (projects, regions/zones, VPC networks, subnetworks, firewall rules, long-running operations) are emulated to the degree the eleven services need them. A React web console embedded in the binary lets users inspect and operate all eleven services from a browser.

Out of scope: production use, performance or SLA parity with GCP, billing, real global anycast, and every service not listed. Section 10 gives the full exclusions list.

### 1.3 Definitions

| Term | Meaning |
| --- | --- |
| Control plane | The resource-management APIs (create a bucket, a cluster, a URL map) |
| Data plane | The traffic the resources carry (object bytes, HTTP requests through the LB, Postgres connections, DNS queries) |
| Fidelity | How closely a behaviour matches real GCP; levels defined in Section 10 |
| Endpoint override | Pointing a client at the emulator through an env var, provider setting or SDK option instead of `*.googleapis.com` |
| Host mode | Optional mode where the emulator answers on real Google hostnames via local DNS and a locally trusted CA |
| Instance | One running emulator process and its state directory |

### 1.4 Requirement conventions

Each requirement has an ID (`FR-<AREA>-nnn`, `NFR-<AREA>-nnn`), a statement, and a MoSCoW priority: **M** must (v1.0 blocker), **S** should (v1.0 target), **C** could (later). "The emulator" means the `gcpemu` binary and anything it starts.

### 1.5 References

- googleapis/googleapis on GitHub (current protos, pinned per release) and the discovery documents for each API listed in 1.2 (the authoritative source of resource shapes and error codes)
- OCI Distribution Specification v1.1
- Kubernetes API conformance for the minor versions in FR-GKE-002
- OpenTofu `google` provider documentation (custom endpoint settings)

## 2. Overall description

### 2.1 Product perspective

Today a GCP stack of GKE, a global external Application Load Balancer with Cloud CDN and backend mTLS to Istio, Cloud DNS, Cloud SQL and GCS can only be exercised end to end in a real project. Google's own emulators cover Pub/Sub, Datastore/Firestore, Spanner and Bigtable only; there is none for GKE, the load balancer, CDN, NAT, DNS, Artifact Registry, Cloud SQL or IAM. Teams patch the gap with MinIO, kind, a hand-run Postgres and mocks, none of which accept the GCP APIs or OpenTofu resources. `gcpemu` replaces that patchwork with one process that speaks the GCP APIs and runs the real traffic.

### 2.2 User classes

| User | Goal | Typical use |
| --- | --- | --- |
| Application developer | Run the whole app locally against "GCP" | `gcpemu start`, then `go run`, `kubectl apply`, browser through the local LB |
| Infrastructure engineer | Test OpenTofu modules without a cloud bill or wait | `tofu apply` with custom endpoints, inspect resources, `tofu destroy` |
| CI pipeline | Hermetic integration and end-to-end tests on every PR | Start detached, seed, run tests, collect logs, exit |
| Test author | Exercise failure paths | Fault injection, LRO latency, IAM deny |

### 2.3 Operating environment

| Item | Requirement |
| --- | --- |
| Host OS | Linux (amd64, arm64) primary; macOS (arm64, amd64) secondary, without GKE in v1.0; Windows via WSL2 only |
| CI | GitHub Actions `ubuntu-24.04` and `ubuntu-24.04-arm` hosted runners; any Linux CI with Docker |
| Container runtime | Required only for GKE and Cloud SQL: Docker Engine ≥ 24, Podman ≥ 4.9, or containerd ≥ 1.7. All other services run with no runtime |
| Privileges | Unprivileged for all services except GKE, which needs a runtime that can run privileged containers |
| Network | Works fully offline once images are cached; internet needed only for first image pulls and for NAT egress tests |

### 2.4 Design and implementation constraints

1. Distributed as one statically linked Go binary per OS/arch; no installer, no runtime other than the container runtime above. The React web console is compiled into the binary, so Node.js is needed only to build it.
2. Implemented in Go; third-party dependencies limited to permissive, foundation- or community-governed licenses (Apache-2.0, MIT, BSD, MPL-2.0).
3. Container images the binary launches (Kubernetes node, PostgreSQL) are multi-arch (amd64 + arm64) and pinned by digest in the release.
4. API resource shapes, field names, error codes and LRO semantics follow the public GCP API definitions; no invented fields in public APIs.
5. No phone-home telemetry.

### 2.5 Assumptions and dependencies

- Users configure clients with endpoint overrides; host mode is optional convenience, not a prerequisite.
- Users accept that GKE and Cloud SQL run real Kubernetes and PostgreSQL in containers, so those two services cost seconds to start and hundreds of MB of RAM.
- GCP API discovery documents and protobufs remain publicly available for code generation.

## 3. System architecture

`gcpemu` is one process: a shared API gateway, one module per service, the data planes those modules configure, and one state store. It reaches outside itself only to run Kubernetes nodes and PostgreSQL in the host's container runtime.

&#91;embedded content: gcpemu architecture · one process, one external dependency\]

Service modules create and manage the GKE and Cloud SQL containers; the data planes carry live traffic to and from them (LB to NEG endpoints, registry pulls, metadata tokens, Postgres connections).

### 3.1 Components

| Component | Responsibility |
| --- | --- |
| Control-plane gateway | Terminates REST and gRPC, authenticates the caller, routes to a service module, records the request log |
| Service modules | One Go package per API: validation, resource state machine, LROs, IAM permission checks; generated types from discovery docs/protos |
| Policy engine | Shared IAM evaluation used by every module and data plane |
| Data planes | L7 proxy with CDN cache (one listener per forwarding rule), DNS server, OCI registry, Pub/Sub broker, metadata server, NAT egress gateway, Cloud SQL connector endpoint |
| Runtime driver | Talks to Docker, Podman or containerd to create, label, start, stop and reclaim node and Postgres containers |
| State store | Embedded pure-Go KV for resources; files under the data dir for objects, blobs, caches and volumes |
| Web console | React SPA embedded in the binary, served by the gateway at /console; uses the public APIs and the admin API's event stream |

### 3.2 Default ports

| Port | Use |
| --- | --- |
| 4510 | Control-plane gateway (all APIs, REST and gRPC) |
| 4443 | Cloud Storage (for `STORAGE_EMULATOR_HOST`) |
| 8085 | Pub/Sub gRPC (for `PUBSUB_EMULATOR_HOST`) |
| 5000 | Artifact Registry (OCI) |
| 5353 | DNS (UDP and TCP) |
| 8988 | Metadata server (host side) |
| 54320+ | Cloud SQL instance public IPs, one per instance |
| 127.0.0.x:80/443 or 18080+ | LB forwarding rules |

Every port is configurable and can be set to 0 (FR-CORE-042). The web console has no port of its own; it lives at /console on the gateway port.

### 3.3 Networking model

Each instance creates one container network (`gcpemu-<instance>`). VPC subnetworks map to address ranges on it, so GKE node, pod and Cloud SQL private IPs are real, routable addresses. The binary is reachable from that network at a gateway address; in-cluster DNS resolves `*.googleapis.com`, `*.pkg.dev` and `metadata.google.internal` to it, and 169.254.169.254 is redirected to the metadata server. Private-node egress is routed through the NAT gateway data plane, which decides per Cloud NAT config whether to forward or drop.

## 4. Functional requirements: core platform

### 4.1 CLI and lifecycle

| ID | Requirement | Pri |
| --- | --- | --- |
| FR-CORE-001 | `gcpemu start` runs the emulator in the foreground; `--detach` daemonizes it and returns once every enabled service reports ready, or exits non-zero after `--wait-timeout` (default 120 s). | M |
| FR-CORE-002 | `gcpemu stop`, `status`, `reset` (wipe state, keep config), `logs [service]` and `version` operate on a running instance identified by `--data-dir` or `--instance`. | M |
| FR-CORE-003 | `--services` selects which services run (e.g. `gcs,pubsub,iam`). Services not selected start no listeners, goroutines or containers. Dependencies are added automatically (GKE pulls in IAM, compute networking and Artifact Registry). | M |
| FR-CORE-004 | `gcpemu env --shell bash` (also `fish`, `github`) prints the env vars clients need (`STORAGE_EMULATOR_HOST`, `PUBSUB_EMULATOR_HOST`, `GCE_METADATA_HOST`, `CLOUDSDK_API_ENDPOINT_OVERRIDES_*`, `KUBECONFIG`, `SSL_CERT_FILE` addition); `github` writes `$GITHUB_ENV` format. | M |
| FR-CORE-005 | `gcpemu tofu-provider` prints an OpenTofu `provider "google"` block with every relevant `*_custom_endpoint` set to the instance. | S |
| FR-CORE-006 | Graceful shutdown on SIGINT/SIGTERM stops data planes, flushes state and removes containers it created, within 30 s. A crashed instance's orphaned containers are reclaimed on next start (labelled by instance ID). | M |
| FR-CORE-007 | `gcpemu doctor` checks container runtime, ports, CA trust, disk and arch, and prints fixes. | S |

### 4.2 Configuration and seeding

| ID | Requirement | Pri |
| --- | --- | --- |
| FR-CORE-010 | Config precedence: flags > env (`GCPEMU_*`) > `gcpemu.yaml` > defaults. | M |
| FR-CORE-011 | A seed file (YAML) declares projects and resources (buckets + objects from local paths, topics, subscriptions, DNS zones and records, service accounts, IAM bindings, AR repositories, SQL instances with init SQL). `start --seed file` applies it idempotently before reporting ready. | M |
| FR-CORE-012 | `gcpemu snapshot save NAME` / `restore NAME` capture and restore all service state, including GCS objects and Postgres data, for fast test fixtures. | S |
| FR-CORE-013 | `gcpemu export` writes current resources as seed YAML. | C |

### 4.3 Projects, locations and resource model

| ID | Requirement | Pri |
| --- | --- | --- |
| FR-CORE-020 | Any project ID is accepted and auto-created on first use; `--strict-projects` requires projects to be declared. Project number is derived deterministically from the ID. | M |
| FR-CORE-021 | Regions and zones are validated against an embedded list of real GCP locations; invalid locations return the same error GCP returns. | M |
| FR-CORE-022 | Resource names, self-links, IDs, etags, fingerprints, `creationTimestamp` and `selfLink` formats match GCP; IDs are deterministic when `--deterministic` is set (for golden-file tests). | M |
| FR-CORE-023 | Mutating calls that are long-running in GCP return `Operation` resources (compute, container, sqladmin, artifactregistry, dns changes) with correct polling and `wait` semantics. LRO duration is `instant` by default and configurable per service (`--lro-latency container=20s`). | M |
| FR-CORE-024 | Field validation, required fields, immutable fields, `updateMask`/`fieldMask` handling and pagination (`pageToken`, `maxResults`/`pageSize`) follow each API's spec. | M |
| FR-CORE-025 | Errors use GCP's status codes and the `google.rpc.Status` / JSON error envelope with `ErrorInfo` reason and domain. | M |
| FR-CORE-026 | Resource-in-use rules are enforced (e.g. cannot delete a backend service referenced by a URL map; cannot delete a non-empty bucket). | M |

### 4.4 State

| ID | Requirement | Pri |
| --- | --- | --- |
| FR-CORE-030 | `--ephemeral` (default in CI when `CI=true`) keeps control-plane state in memory and data under a temp dir removed on exit. | M |
| FR-CORE-031 | `--data-dir` persists all state across restarts; resources, object data, Postgres data and Kubernetes cluster state survive `stop`/`start`. | M |
| FR-CORE-032 | Control-plane state is stored in an embedded pure-Go store (no cgo). Writes are crash-safe. | M |
| FR-CORE-033 | Multiple instances can run side by side on one host with distinct data dirs and ports. | M |

### 4.5 API surface and endpoints

| ID | Requirement | Pri |
| --- | --- | --- |
| FR-CORE-040 | One gateway port (default 4510) serves every control-plane API over HTTP/1.1 REST and gRPC (HTTP/2, h2c and TLS), routing by path and `:authority`/`Host`. | M |
| FR-CORE-041 | Per-service ports are also available for clients that cannot set a path prefix (e.g. `PUBSUB_EMULATOR_HOST=localhost:8085`, GCS on 4443). | M |
| FR-CORE-042 | `--port 0` and `--port-range` allocate free ports; chosen ports are written to `<data-dir>/endpoints.json` and printed by `gcpemu env`. | M |
| FR-CORE-043 | Host mode (`--host-mode`) serves real hostnames (`storage.googleapis.com`, `REGION-docker.pkg.dev`, …) via the emulator's DNS and a local CA, so unmodified clients work once the CA is trusted. | S |
| FR-CORE-044 | Listeners bind to 127.0.0.1 by default; `--bind` widens it with a warning. | M |
| FR-CORE-045 | An admin API (`/_emu/…`) exposes health, readiness per service, resource dumps, request log, fault injection, reset, and a server-sent events stream of resource, LRO and request-log changes for the web console. | M |

### 4.6 Authentication of API callers

| ID | Requirement | Pri |
| --- | --- | --- |
| FR-CORE-050 | Requests with no credentials are accepted and attributed to a configurable default principal (`user:dev@example.com`). | M |
| FR-CORE-051 | Bearer tokens issued by the emulator's IAM (Section 5.1) are validated and identify the caller; unknown tokens are rejected only when IAM enforcement is on. | M |
| FR-CORE-052 | Service-account JSON keys created by the emulator, ADC from `gcloud auth application-default login` pointed at the emulator, and the emulated metadata server all produce usable tokens. | M |

### 4.7 Fault injection and observability

| ID | Requirement | Pri |
| --- | --- | --- |
| FR-CORE-060 | Rules can inject an error code, latency or dropped connection by service, method, resource name pattern and probability or count, via admin API, CLI, web console and seed file. | S |
| FR-CORE-061 | Every API call and data-plane request is logged as structured JSON (method, principal, resource, status, latency); `--log-format text` for humans. | M |
| FR-CORE-062 | Prometheus metrics endpoint for request counts and latencies per service. | C |
| FR-CORE-063 | A React web console embedded in the binary inspects and operates every service; requirements in 4.8. | M |

### 4.8 Web console

The web console is a React single-page app compiled into the binary and served at `http://localhost:4510/console`. It talks only to the same public GCP APIs and `/_emu/v1` admin API that every other client uses, so anything done in the console can also be done, and tested, from code.

#### 4.8.1 Platform and shell

| ID | Requirement | Pri |
| --- | --- | --- |
| FR-UI-001 | Built with React and TypeScript (Vite build), embedded with `go:embed`; Node.js is needed only at build time. All assets, fonts and icons are bundled, so the console works fully offline. | M |
| FR-UI-002 | Served on the gateway port under `/console`; `gcpemu console` opens it in the default browser; `--console=false` disables it. | M |
| FR-UI-003 | Calls only public GCP API endpoints and the admin API through the gateway; no console-private backend endpoints. | M |
| FR-UI-004 | Acts as the default principal; a principal switcher lets the user act as any user or service account to see what IAM allows. | M |
| FR-UI-005 | Global project switcher, location filter and search across resource names; every view has a shareable deep link. | M |
| FR-UI-006 | Live updates without manual refresh, via a server-sent events stream on the admin API (`/_emu/v1/events`) carrying resource changes, LRO progress and request-log entries. | M |
| FR-UI-007 | Light and dark themes following the OS; keyboard navigation; WCAG 2.1 AA contrast and labels. | S |
| FR-UI-008 | Supports the latest two versions of Chrome, Firefox, Safari and Edge. | M |
| FR-UI-009 | When `--bind` exposes the gateway beyond loopback, the console requires a session token printed at startup. | M |

#### 4.8.2 Cross-service views

| ID | Requirement | Pri |
| --- | --- | --- |
| FR-UI-010 | Dashboard: per-service readiness with failure reason, endpoint and port map, container runtime status, resource counts, and a copy button for `gcpemu env` output. | M |
| FR-UI-011 | Every resource type at **M** priority in Section 5 can be listed, viewed, created, edited and deleted. Forms mirror API fields with the API's validation messages; a raw JSON editor covers any field the form omits. | M |
| FR-UI-012 | Operations view: all LROs with live status, duration and the resulting resource or error. | M |
| FR-UI-013 | Request log: live tail with filters for service, method, principal, status and resource; expandable request and response bodies. | M |
| FR-UI-014 | IAM audit view: every call audit mode would have denied, with the missing permission, the smallest predefined role that grants it, and a one-click grant. | M |
| FR-UI-015 | Any resource can be copied as a `gcloud` command, `curl` request or OpenTofu resource block. | S |
| FR-UI-016 | Fault injection rules: create, enable, disable and see hit counts. | S |
| FR-UI-017 | Snapshots, reset and seed import/export. | S |

#### 4.8.3 Service views

| Service | Shows | Actions | Pri |
| --- | --- | --- | --- |
| IAM | Service accounts, keys, custom and predefined roles, policies on any resource, enforcement mode | Create SA, create and download key, edit bindings, switch mode, policy simulator (`testIamPermissions` as any principal) | M |
| Cloud DNS | Zones, record sets, network bindings | CRUD records through `changes`; built-in query box that resolves a name against the emulator's DNS server | M |
| Artifact Registry | Repositories, images, tags, digests, per-arch manifests, sizes | Delete tag or digest; copy pull command | M |
| Cloud Storage | Bucket browser by prefix, object metadata, generations, notifications | Drag-and-drop upload, download, delete, restore a generation, generate a V4 signed URL | M |
| Pub/Sub | Topics, subscriptions, backlog, oldest unacked age, dead-letter counts | Publish with attributes and ordering key, pull and ack, peek without ack, seek, purge | M |
| Cloud SQL | Instances, databases, users, flags, connection names and strings | Start, stop, restart; SQL query runner with read-only default; backups and restore | M |
| GKE | Clusters, node pools, nodes, namespaces, workloads and pod status, NEGs | Download kubeconfig, resize node pool, view pod logs | M |
| Load Balancer | Forwarding rules with local listener addresses, proxies, URL maps rendered as a route tree, backend health per endpoint, cert status | Edit URL map; "test a URL" shows which route and backend would serve a given host, path and headers | M |
| Cloud CDN | Cache settings per backend, hit ratio, cached entries and sizes | Invalidate a path, purge all | M |
| Cloud NAT | Routers, NAT configs, covered subnets, port usage, translation log | Toggle offline sink | S |
| Secret Manager | Global and regional secrets, labels, expiry, rotation and topics; versions with state and aliases | CRUD secrets; add a version; reveal a payload (masked by default); enable, disable and destroy versions | M |
| Load Balancer traffic | Live per-request trace: forwarding rule, matched route, backend, cache status, latency | Filter by host or path | S |

#### 4.8.4 Console quality

| ID | Requirement | Target | Pri |
| --- | --- | --- | --- |
| FR-UI-020 | Compressed bundle size added to the binary | ≤ 3 MB | S |
| FR-UI-021 | First load to interactive on localhost | ≤ 1 s | S |
| FR-UI-022 | Large lists (objects, records, log lines) stay responsive by virtualizing rows | 100,000 rows | S |
| FR-UI-023 | Playwright end-to-end tests cover every **M** console requirement on Chromium, Firefox and WebKit | 100% of M | M |

## 5. Functional requirements: services

### 5.1 IAM

IAM is the identity backbone every other service calls; enforcement defaults to audit mode, so missing roles surface in the logs without blocking a first run.

| ID | Requirement | Pri |
| --- | --- | --- |
| FR-IAM-001 | Service accounts: create, get, list, patch, disable/enable, delete, undelete; keys: create (JSON, RSA-2048), list, delete, upload. | M |
| FR-IAM-002 | Roles: an embedded catalogue of predefined roles and their permissions for the eleven services, plus project-level custom roles (create, patch, delete, undelete). | M |
| FR-IAM-003 | `getIamPolicy`, `setIamPolicy`, `testIamPermissions` on projects and on every resource type GCP supports them on (buckets, topics, subscriptions, AR repositories, service accounts, clusters via project). Etag concurrency enforced. | M |
| FR-IAM-004 | Enforcement modes: `off` (allow all), `audit` (allow, log every call that real IAM would deny with the missing permission), `enforce` (deny with `PERMISSION_DENIED`). Default mode is audit. Each API method maps to the permission GCP checks. | M |
| FR-IAM-005 | IAM Conditions on bindings, evaluating `request.time`, `resource.name` and `resource.type` CEL expressions. | S |
| FR-IAM-006 | OAuth2 token endpoint for SA keys (JWT bearer grant) and user ADC (refresh-token grant); tokens are opaque, map to a principal, expire after 1 h. | M |
| FR-IAM-007 | IAM Credentials API: `generateAccessToken`, `generateIdToken` (signed JWTs verifiable against the emulator's JWKS), `signBlob`, `signJwt`, with impersonation chains checked against `roles/iam.serviceAccountTokenCreator`. | M |
| FR-IAM-008 | Metadata server compatible with GCE/GKE (`/computeMetadata/v1/…`, `Metadata-Flavor: Google`): project ID, numeric ID, zone, default SA token and identity token. Reachable from the host via `GCE_METADATA_HOST` and from GKE pods at 169.254.169.254. | M |
| FR-IAM-009 | Workload Identity Federation: workload identity pools and OIDC providers; STS token exchange accepting OIDC tokens from a configured issuer (e.g. GitHub Actions) with attribute mapping and conditions. | S |
| FR-IAM-010 | Deny policies and organisation/folder hierarchy. | C |

### 5.2 Cloud DNS

| ID | Requirement | Pri |
| --- | --- | --- |
| FR-DNS-001 | Managed zones: public and private (with network bindings), create/get/list/patch/delete; `nameServers` returned. | M |
| FR-DNS-002 | Resource record sets: CRUD for A, AAAA, CNAME, MX, TXT, SRV, NS, SOA, CAA, PTR; `changes.create` applied atomically with additions/deletions semantics and `changes.get` status. | M |
| FR-DNS-003 | An authoritative DNS server (UDP and TCP, default port 5353, configurable) answers queries for all zones with correct TTL, NXDOMAIN, NODATA and CNAME chasing within the zone. | M |
| FR-DNS-004 | Private zones are resolvable from GKE pods and nodes on bound networks; public zones resolvable from host and pods. Unknown names are forwarded to the host resolver (disable with `--dns-no-forward`). | M |
| FR-DNS-005 | Routing policies (weighted round robin, geolocation picks first entry) on record sets. | C |
| FR-DNS-006 | DNSSEC, response policies, peering zones. | C |

### 5.3 Artifact Registry

| ID | Requirement | Pri |
| --- | --- | --- |
| FR-AR-001 | Repositories: create/get/list/patch/delete for format `DOCKER`, mode `STANDARD_REPOSITORY`, in any valid location. | M |
| FR-AR-002 | An OCI Distribution v1.1 registry serves `LOCATION-docker.pkg.dev/PROJECT/REPO/IMAGE`: push, pull, chunked and monolithic uploads, cross-repo mount, manifest lists and OCI indexes (multi-arch), referrers API, tag list, delete. | M |
| FR-AR-003 | Registry auth accepts `oauth2accesstoken` + emulator bearer token and `_json_key`; `gcloud auth configure-docker` and `docker-credential-gcr` patterns work against the emulator. Anonymous pull allowed when IAM is `off`. | M |
| FR-AR-004 | Packages, versions, tags and docker images are listable through the AR API and agree with registry contents. | M |
| FR-AR-005 | GKE nodes in the same instance pull from the registry with no extra config, including in host mode and port-mapped mode. | M |
| FR-AR-006 | Remote repositories (pull-through cache of Docker Hub) and virtual repositories. | S |
| FR-AR-007 | Cleanup policies (dry-run and delete) evaluated on demand. | C |
| FR-AR-008 | Non-Docker formats (npm, Go, Python, generic). | C |

### 5.4 Cloud Storage

| ID | Requirement | Pri |
| --- | --- | --- |
| FR-GCS-001 | Buckets: CRUD, location and storage class (stored, not acted on), uniform bucket-level access, labels, CORS, versioning, retention policy, default event-based hold, website config. | M |
| FR-GCS-002 | Objects via JSON API: simple, multipart and resumable upload (with chunk resume after interruption), download with `Range`, `alt=media`, compose, copy, rewrite (with token loop), list with prefix/delimiter/glob/pagination, metadata patch, delete. | M |
| FR-GCS-003 | Generations and metagenerations; preconditions `ifGenerationMatch`, `ifGenerationNotMatch`, `ifMetagenerationMatch` and their 412 errors. | M |
| FR-GCS-004 | Checksums: MD5 and CRC32C computed and validated on upload; returned in headers and metadata. | M |
| FR-GCS-005 | XML API (S3-compatible subset): GET/PUT/HEAD/DELETE object, list, multipart upload; HMAC keys. | S |
| FR-GCS-006 | V4 signed URLs and signed POST policies signed by emulator SA keys (or `signBlob`) are verified, including expiry. | M |
| FR-GCS-007 | Pub/Sub notifications (`OBJECT_FINALIZE`, `OBJECT_DELETE`, `OBJECT_ARCHIVE`, `OBJECT_METADATA_UPDATE`) to emulated topics with GCS's payload and attributes. | M |
| FR-GCS-008 | Lifecycle rules (delete, set storage class) evaluated on a configurable clock with `gcpemu time advance` for tests. | S |
| FR-GCS-009 | Object ACLs (legacy fine-grained) when uniform access is off. | C |
| FR-GCS-010 | Throughput target: ≥ 200 MB/s single-stream upload and download on local SSD. | S |

### 5.5 Pub/Sub

| ID | Requirement | Pri |
| --- | --- | --- |
| FR-PS-001 | Topics and subscriptions: CRUD, labels, message retention, `PUBSUB_EMULATOR_HOST` compatibility for all official client libraries (gRPC). | M |
| FR-PS-002 | Publish with attributes and ordering keys; pull, streaming pull (flow control, lease extension), acknowledge, modifyAckDeadline, nack. | M |
| FR-PS-003 | At-least-once delivery with ack deadlines and redelivery; ordered delivery per ordering key when `enableMessageOrdering` is set. | M |
| FR-PS-004 | Dead-letter topics with `maxDeliveryAttempts` and `deliveryAttempt` populated; retry policy with exponential backoff. | M |
| FR-PS-005 | Subscription filters (attribute filter syntax). | M |
| FR-PS-006 | Push subscriptions to any URL, including GKE services via the LB or cluster DNS, with OIDC tokens signed by the emulator for the configured SA and audience; wrapped and unwrapped payloads. | M |
| FR-PS-007 | Exactly-once delivery semantics (ack results). | S |
| FR-PS-008 | Snapshots and seek (to time and snapshot); message retention and replay of acked messages. | S |
| FR-PS-009 | Schemas (Avro, Protobuf) with publish-time validation. | S |
| FR-PS-010 | BigQuery and Cloud Storage subscriptions. | C |

### 5.6 Cloud SQL for PostgreSQL

Each instance is a real PostgreSQL server in a container, so SQL behaviour is exact; the emulator fakes only the management layer around it.

| ID | Requirement | Pri |
| --- | --- | --- |
| FR-SQL-001 | Instances: insert, get, list, patch, delete, restart, stop/start (`activationPolicy`) for `POSTGRES_14` to `POSTGRES_18`; the major version selects the container image. Tier, disk size and availability type are stored and reported, not enforced, but tier and edition must agree as in GCP: an unset edition means Enterprise Plus from `POSTGRES_16`, which accepts only `db-perf-optimized-*` tiers. | M |
| FR-SQL-002 | Databases and users: CRUD; built-in users get passwords; `postgres` superuser behaves like Cloud SQL's `cloudsqlsuperuser` membership model. | M |
| FR-SQL-003 | Database flags: the Cloud SQL allow-list for PostgreSQL is validated and applied as server settings; restart-required flags trigger a restart LRO. | M |
| FR-SQL-004 | Connectivity: public IP mapped to a host port (reported as `ipAddresses`); private IP reachable from GKE pods on the bound VPC; authorized networks enforced on the public path. | M |
| FR-SQL-005 | Cloud SQL Auth Proxy and the Go connector (`cloud.google.com/go/cloudsqlconn`) work: the emulator implements `connectSettings` and `generateEphemeralCert` and terminates the connector's TLS on the instance's server-side proxy port (3307). | M |
| FR-SQL-006 | IAM database authentication: `CLOUD_IAM_USER` and `CLOUD_IAM_SERVICE_ACCOUNT` users log in with an emulator access token as password, checked for `roles/cloudsql.instanceUser`. | M |
| FR-SQL-007 | Server CA and SSL modes (`ALLOW_UNENCRYPTED_AND_ENCRYPTED`, `ENCRYPTED_ONLY`, `TRUSTED_CLIENT_CERTIFICATE_REQUIRED`); client certs via `sslCerts`. | S |
| FR-SQL-008 | On-demand backups, restore and clone, implemented as volume snapshots or `pg_dump`/`pg_restore`. | S |
| FR-SQL-009 | `import`/`export` of SQL dumps from and to emulated GCS. | S |
| FR-SQL-010 | Read replicas (streaming replication) and point-in-time recovery. | C |

### 5.7 GKE

Each GKE cluster is a real, CNCF-conformant Kubernetes cluster (a k3s-class distribution: each cluster's control plane runs in its own container, and each node in another), wrapped in the `container.googleapis.com` API and GKE's in-cluster behaviours.

| ID | Requirement | Pri |
| --- | --- | --- |
| FR-GKE-001 | Clusters (Standard mode): create, get, list, update, delete, with `Operation`s; `endpoint`, `masterAuth.clusterCaCertificate`, `currentMasterVersion`, `status` and `location` (zonal or regional) reported. | M |
| FR-GKE-002 | Kubernetes minor version selected by `initialClusterVersion`/release channel from the three most recent minors; `getServerConfig` reports them. Upgrades of control plane and node pools by version change. | M |
| FR-GKE-003 | Node pools: create, delete, resize (adds/removes node containers), labels, taints, machine type stored; `nodeCount` honoured up to a host limit (default 5 nodes per instance). Node arch = host arch. | M |
| FR-GKE-004 | `gcloud container clusters get-credentials` and `gke-gcloud-auth-plugin` produce a working kubeconfig; the API server accepts emulator IAM tokens and maps IAM roles (`container.admin`, `container.developer`, `container.viewer`) plus RBAC to Kubernetes auth. | M |
| FR-GKE-005 | Workload Identity Federation for GKE: `workloadPool = PROJECT.svc.id.goog`; pods whose KSA is annotated with `iam.gke.io/gcp-service-account` get that GSA's tokens from the metadata server, subject to `roles/iam.workloadIdentityUser`. | M |
| FR-GKE-006 | Image pulls from emulated Artifact Registry and from public registries; node-level image cache persists across cluster recreates in the same data dir. | M |
| FR-GKE-007 | Private clusters: nodes have no direct egress; outbound internet only via Cloud NAT (5.10) when configured. | M |
| FR-GKE-008 | Container-native load balancing: Services annotated `cloud.google.com/neg` produce standalone zonal NEGs (`GCE_VM_IP_PORT`) visible in the compute API, with endpoints kept in sync with ready pods. | M |
| FR-GKE-009 | Istio (or any mesh) can be installed with standard Helm charts; the cluster supports `LoadBalancer`-type Services via an internal L4 allocator. | M |
| FR-GKE-010 | GKE Ingress and Gateway API controllers (`gke-l7-global-external-managed`) that create LB resources from Kubernetes objects. | S |
| FR-GKE-011 | Network policy enforcement (Dataplane V2 semantics). | S |
| FR-GKE-012 | Autopilot mode, excluded from v1.0; until then, clusters with Autopilot enabled are rejected as UNIMPLEMENTED. | C |
| FR-GKE-013 | Cluster settings that install components, on create and update, removed (keeping CRDs, user objects and synced Secrets) when disabled: `networkConfig.gatewayApiConfig` installs the Gateway API CRDs of the channel (v1.5.1); `secretManagerConfig` installs the Secrets Store CSI driver as `secrets-store-gke.csi.k8s.io` with the `gke` provider reading Secret Manager as the pod's Workload Identity, with optional rotation; `secretSyncConfig` runs the `SecretSync` (`secret-sync.gke.io/v1`) controller that copies Secret Manager versions into Kubernetes Secrets. IAM matches GKE's `principal://…/subject/ns/NS/sa/KSA` and `principalSet://…/namespace/NS` members. | S |

### 5.8 Application Load Balancer

The emulator runs a real L7 proxy per forwarding rule, configured from the compute API resources exactly as GCP wires them: forwarding rule → target proxy → URL map → backend service or bucket → NEG.

| ID | Requirement | Pri |
| --- | --- | --- |
| FR-LB-001 | Resources: global addresses, global forwarding rules (`EXTERNAL_MANAGED`), target HTTP and HTTPS proxies, URL maps, backend services, backend buckets, health checks (HTTP, HTTPS, HTTP2, TCP), SSL certificates (self-managed and Google-managed), SSL policies, network endpoint groups. Regional external and internal ALB variants share the implementation. | M |
| FR-LB-002 | Each forwarding rule's IP:port is mapped to a host listener (127.0.0.x loopback alias on Linux, or a host port); the mapping is reported by `gcpemu env` and resolvable via emulated DNS A records. | M |
| FR-LB-003 | URL map routing: host rules, path matchers, path rules, advanced route rules (header, query and prefix/full/regex matches, priority), URL rewrite, redirects (HTTPS, host, path, response code), weighted backend services, request and response header actions, default service. | M |
| FR-LB-004 | TLS termination with self-managed certs, SNI selection across multiple certs and Certificate Manager certificate maps; Google-managed certs issued by the emulator's CA once DNS for the domain points at the forwarding rule (`PROVISIONING` → `ACTIVE`). | M |
| FR-LB-005 | Backend protocols HTTP, HTTPS and HTTP/2 to NEG endpoints and instance groups; HTTP/1.1, HTTP/2 and WebSocket from clients; gRPC passthrough. | M |
| FR-LB-006 | Backend mTLS: the LB presents a client certificate to backends as configured by the backend authentication resources defined in the current Network Security and Certificate Manager protos in googleapis/googleapis on GitHub (client certificate plus trust config), and validates the backend's server certificate against that trust config. An Istio ingress gateway with `MUTUAL` TLS must accept the LB and reject other clients. | M |
| FR-LB-007 | Health checks probe endpoints on their configured interval and thresholds; unhealthy endpoints are drained; `getHealth` reports per-endpoint state. | M |
| FR-LB-008 | Session affinity, locality LB policy (round robin, least request), timeouts, connection draining, retry policy, outlier detection, custom request/response headers including `{client_cert_*}` and `{cdn_cache_status}` variables. | S |
| FR-LB-009 | Frontend mTLS (client cert validation at the LB via server TLS policy). | S |
| FR-LB-010 | Access logs per request in Cloud Logging `httpRequest` JSON shape, including `statusDetails` and backend latency. | M |
| FR-LB-011 | Cloud Armor security policies. | C |

### 5.9 Cloud CDN

| ID | Requirement | Pri |
| --- | --- | --- |
| FR-CDN-001 | `enableCdn` and `cdnPolicy` on backend services and backend buckets turn on an in-memory + disk HTTP cache in front of that backend. | M |
| FR-CDN-002 | Cache modes `CACHE_ALL_STATIC`, `USE_ORIGIN_HEADERS`, `FORCE_CACHE_ALL`; `defaultTtl`, `maxTtl`, `clientTtl`; static content-type list matches GCP's; `Cache-Control`, `Expires`, `Vary`, `Set-Cookie` and `Authorization` handling follow Cloud CDN rules. | M |
| FR-CDN-003 | Cache key policy: include/exclude protocol, host, query string (allow/deny lists), named headers and cookies. | M |
| FR-CDN-004 | Responses carry `Age`; `{cdn_cache_status}` header variable yields `hit`, `miss`, `revalidated`, `uncacheable`. Byte-range requests and conditional revalidation (`If-None-Match`, `If-Modified-Since`) supported. | M |
| FR-CDN-005 | `urlMaps.invalidateCache` by host and path (with `/*` wildcards) as an LRO; invalidation effective before the LRO completes. | M |
| FR-CDN-006 | Negative caching with per-status TTLs; serve-while-stale; request coalescing. | S |
| FR-CDN-007 | Signed URLs and signed cookies with signing keys on backend services/buckets. | S |
| FR-CDN-008 | Cache size limit (default 1 GiB) with LRU eviction; `gcpemu cdn purge` clears all. | M |

### 5.10 Cloud NAT

| ID | Requirement | Pri |
| --- | --- | --- |
| FR-NAT-001 | Cloud Routers and their `nats[]`: create, get, list, patch, delete; source subnetwork ranges (`ALL_SUBNETWORKS_ALL_IP_RANGES`, list of subnetworks), NAT IP allocation (`AUTO_ONLY` or reserved addresses). | M |
| FR-NAT-002 | Egress enforcement: workloads in private GKE nodes reach the internet only if a NAT covers their subnet and region; otherwise connections fail as on GCP (timeout, configurable to immediate reject for fast tests). | M |
| FR-NAT-003 | Allowed egress is routed through an emulator egress gateway that applies `minPortsPerVm`, dynamic port allocation and endpoint-independent mapping limits, and reports port usage via `getNatMappingInfo` and `getRouterStatus`. | S |
| FR-NAT-004 | NAT logging (`TRANSLATIONS_ONLY`, `ERRORS_ONLY`, `ALL`) in Cloud Logging JSON shape. | S |
| FR-NAT-005 | `--offline` mode: NAT-allowed egress is redirected to a sink that records the attempt and returns a configurable response, so tests never hit the internet. | S |
| FR-NAT-006 | NAT rules (per-destination NAT IP selection). | C |

### 5.11 Secret Manager

| ID | Requirement | Pri |
| --- | --- | --- |
| FR-SM-001 | Secrets: create, get, list (with `filter`), patch with `update_mask`, delete; global secrets with automatic or user-managed replication, and regional secrets (`projects/*/locations/*/secrets/*`) also served on `secretmanager.<region>.rep.googleapis.com`. Names are reported with the project number. Etags guard updates and deletes. | M |
| FR-SM-002 | Versions: add (payload up to 64 KiB, optional CRC32C check), get, list, access, enable, disable and destroy; `latest` and user-defined version aliases resolve on get and access. | M |
| FR-SM-003 | Secret IAM policies; `secretmanager.*` permissions and the predefined Secret Manager roles under every IAM enforcement level. Editor does not grant `secretmanager.versions.access`. | M |
| FR-SM-004 | Time-driven behaviour on the Emulator clock: `expire_time`/`ttl` deletes the secret, `version_destroy_ttl` delays destruction, and `rotation` schedules `SECRET_ROTATE` notifications. | M |
| FR-SM-005 | Event notifications: every change to a secret with `topics` is published to the emulated Pub/Sub in the `JSON_API_V1` format. | M |
| FR-SM-006 | Managed rotation (`EnableManagedRotation`, `RotateSecret`, scheduled rotation) of a `CLOUD_SQL_DB_CREDENTIALS` regional secret sets a new password on the Cloud SQL user and stores it as the newest version. | S |

## 6. Cross-service integration

The emulator's value is in the seams: each row below is a path that must work end to end without user glue.

| ID | Integration | Behaviour | Pri |
| --- | --- | --- | --- |
| FR-INT-001 | LB → GKE via NEG | Backend service with a GKE standalone NEG routes to ready pods; scaling a Deployment updates the NEG and the LB within 5 s. | M |
| FR-INT-002 | LB → Istio with backend mTLS | Browser → TLS at LB → re-encrypted mTLS to Istio ingress gateway → service; traffic is encrypted on every hop. | M |
| FR-INT-003 | LB + CDN → GCS backend bucket | Static assets served from an emulated bucket through the CDN; API paths on the same URL map go to GKE. | M |
| FR-INT-004 | DNS → LB | An A record pointing at a global address resolves to that forwarding rule's local listener; `curl https://app.example.test` works with the emulator CA trusted. | M |
| FR-INT-005 | DNS ↔ GKE | Cluster CoreDNS forwards to emulated Cloud DNS for private zones; cluster-internal names stay local. | M |
| FR-INT-006 | GKE → Artifact Registry | Pods reference `LOCATION-docker.pkg.dev/...` images; pulls authenticate as the node SA. | M |
| FR-INT-007 | GKE → GCS, Pub/Sub, Cloud SQL via Workload Identity | Unmodified Go client libraries in pods obtain GSA tokens from the metadata server and reach emulated services by real hostname (in-cluster DNS rewrite) with no endpoint config in the app. | M |
| FR-INT-008 | GKE → Cloud SQL private IP | Pods connect to an instance's private IP directly or via the Cloud SQL connector with IAM auth. | M |
| FR-INT-009 | GKE → internet via NAT | Private cluster egress allowed only through a configured NAT (FR-NAT-002). | M |
| FR-INT-010 | GCS → Pub/Sub | Bucket notifications publish to topics (FR-GCS-007). | M |
| FR-INT-011 | Pub/Sub → GKE push | Push subscription to a service URL via the LB, with OIDC token verifiable by the app. | M |
| FR-INT-012 | IAM everywhere | In `enforce` mode, every service checks the caller's permissions through one shared policy engine; `audit` mode output lists exactly the roles a workload is missing. | M |
| FR-INT-013 | Cloud SQL ↔ GCS | Import/export dumps (FR-SQL-009). | S |

## 7. External interface requirements

The compatibility bar is the client, not the spec: a requirement is met when the listed client works against the emulator with only endpoint configuration.

### 7.1 Client compatibility matrix

| ID | Client | How it is pointed at the emulator | Must work for | Pri |
| --- | --- | --- | --- | --- |
| IF-001 | OpenTofu `google` and `google-beta` providers (current and previous minor) | `*_custom_endpoint` settings (FR-CORE-005), `access_token` from emulator | Every resource type behind the eleven services, including `plan` with no diff after `apply` and clean `destroy` | M |
| IF-002 | Go Cloud Client Libraries (`cloud.google.com/go/...`) | `option.WithEndpoint`, emulator env vars, or host mode | Storage, Pub/Sub, Secret Manager, IAM, Artifact Registry, Container, Compute, DNS, SQL Admin, Cloud SQL connector | M |
| IF-003 | `gcloud` CLI | `api_endpoint_overrides/*` properties set by `gcpemu env` | `storage`, `pubsub`, `secrets`, `container`, `compute` (LB, routers, NEGs), `dns`, `artifacts`, `sql`, `iam` command groups | M |
| IF-004 | `kubectl`, Helm, `istioctl` | Kubeconfig from `get-credentials` | Standard cluster operations | M |
| IF-005 | Docker, Podman, `crane`, `ko` | Registry host from `gcpemu env`; credential helper | Push and pull multi-arch images | M |
| IF-006 | `psql`, pgx, Cloud SQL Auth Proxy v2 | Host port or proxy with emulator endpoint | Connect, IAM auth | M |
| IF-007 | `gsutil`/`gcloud storage`, S3 clients via XML API | Endpoint override | Copy, sync, signed URLs | S |
| IF-008 | Client libraries in Node, Python, Java | Endpoint override / emulator env vars | Storage and Pub/Sub at minimum | S |
| IF-009 | `dig`, browsers, `curl` | Emulated DNS port; local CA trusted | Name resolution and HTTPS through the LB | M |

### 7.2 Protocol interfaces

- REST/JSON on HTTP/1.1 and HTTP/2 for every control-plane API, honouring `alt`, `fields`, `prettyPrint` and `$httpVersion` query parameters where the API does.
- gRPC for APIs whose official clients use gRPC (Pub/Sub, Storage gRPC is **C**, Artifact Registry, Container, IAM Credentials), with server reflection enabled.
- OCI Distribution v1.1 over HTTPS for the registry.
- PostgreSQL wire protocol (native) for Cloud SQL.
- DNS over UDP and TCP.
- HTTP/1.1, HTTP/2 and WebSocket for LB listeners.

### 7.3 Admin interface

The `/_emu/` admin API (FR-CORE-045) is versioned (`/_emu/v1/`), documented with an OpenAPI file shipped in the binary (`gcpemu admin openapi`), and is the only interface not modelled on GCP.

### 7.4 Certificates

On first start the emulator creates a root CA in the data dir (`ca.pem`). `gcpemu ca install` adds it to the OS and Docker/containerd trust stores (with confirmation), and GKE nodes trust it automatically. The CA never leaves the data dir and is regenerated by `gcpemu ca rotate`.

## 8. Non-functional requirements

Targets are measured on a 4-vCPU, 16 GB Linux runner with SSD (GitHub `ubuntu-24.04` class), images pre-cached.

### 8.1 Performance and footprint

| ID | Requirement | Target | Pri |
| --- | --- | --- | --- |
| NFR-PERF-001 | Cold start to ready, all services except GKE and Cloud SQL | ≤ 2 s | M |
| NFR-PERF-002 | Cloud SQL instance insert to `RUNNABLE` | ≤ 10 s | M |
| NFR-PERF-003 | GKE cluster create to `RUNNING` with 1 node pool of 1 node | ≤ 60 s | M |
| NFR-PERF-004 | Idle RSS of the binary, all non-container services | ≤ 150 MB | M |
| NFR-PERF-005 | Control-plane API p99 latency (non-LRO calls, instant mode) | ≤ 20 ms | M |
| NFR-PERF-006 | Pub/Sub publish-to-pull throughput, 1 KB messages | ≥ 10,000 msg/s | S |
| NFR-PERF-007 | LB + CDN cache hit p99 added latency | ≤ 2 ms | S |
| NFR-PERF-008 | LB throughput, cache miss to a local backend | ≥ 5,000 req/s | S |
| NFR-PERF-009 | Binary size (compressed download) | ≤ 120 MB | S |

### 8.2 Portability

| ID | Requirement | Pri |
| --- | --- | --- |
| NFR-PORT-001 | Release builds for linux/amd64, linux/arm64, darwin/arm64, darwin/amd64, built with `CGO_ENABLED=0`. | M |
| NFR-PORT-002 | Identical behaviour on amd64 and arm64; GKE nodes and Cloud SQL run native-arch images. | M |
| NFR-PORT-003 | GKE is Linux-only in v1.0; on macOS, selecting it fails at start with a clear message. Cloud SQL on macOS uses the user's Docker-compatible VM (Docker Desktop, Colima, OrbStack, Podman machine), and loopback aliases are replaced by host ports. | S |
| NFR-PORT-004 | Also published as a multi-arch OCI image for CI systems that prefer service containers (non-GKE services only when not privileged). | S |

### 8.3 Reliability and determinism

| ID | Requirement | Pri |
| --- | --- | --- |
| NFR-REL-001 | No data loss in `--data-dir` mode on SIGKILL for acknowledged writes (GCS objects, Pub/Sub publishes, control-plane mutations). | M |
| NFR-REL-002 | `--deterministic` makes IDs, generated names, timestamps (from a fake clock) and LRO ordering reproducible run to run. | S |
| NFR-REL-003 | A failed service does not take down others; it reports not-ready with a reason. | M |
| NFR-REL-004 | 24 h soak with continuous traffic shows no unbounded memory growth. | S |

### 8.4 Security

| ID | Requirement | Pri |
| --- | --- | --- |
| NFR-SEC-001 | Loopback-only by default; startup refuses `--bind 0.0.0.0` without `--i-understand-this-is-insecure` when IAM is `off`. | M |
| NFR-SEC-002 | The emulator never calls real Google endpoints or uses real Google credentials; real ADC files are ignored unless explicitly pointed at the emulator. | M |
| NFR-SEC-003 | Keys, tokens and the CA are generated locally, stored with 0600 permissions, and scoped to one data dir. | M |
| NFR-SEC-004 | Containers it launches carry instance labels and run with the least privilege the workload allows (only GKE nodes are privileged). | M |
| NFR-SEC-005 | Releases are signed (Sigstore/cosign) with SBOM and SLSA provenance. | S |

### 8.5 Maintainability

| ID | Requirement | Pri |
| --- | --- | --- |
| NFR-MNT-001 | API stubs, request/response types and validation are generated from the current protos in googleapis/googleapis on GitHub (pinned per release) and Google's discovery documents; a version bump regenerates them. | M |
| NFR-MNT-002 | Each service is an isolated Go package behind a common service interface (register routes, start, stop, ready, snapshot). | M |
| NFR-MNT-003 | ≥ 80% line coverage on control-plane packages; a nightly job runs the conformance suite (Section 11) against real GCP to detect drift. | S |
| NFR-MNT-004 | Licensed Apache-2.0. | M |

## 9. CI requirements

In CI the emulator must start in one step, need no cloud credentials, and leave a useful trail when a test fails.

| ID | Requirement | Pri |
| --- | --- | --- |
| FR-CI-001 | A first-party GitHub Action (`gcpemu/setup-gcpemu`) downloads and verifies the binary for the runner's OS/arch, caches it and container images (keyed by release), starts the emulator detached with given `services`, `seed` and `config`, waits for readiness and exports env vars to `$GITHUB_ENV`. | M |
| FR-CI-002 | The action's post step collects `gcpemu logs`, the request log, resource dump and kubeconfig into a workflow artifact on failure. | M |
| FR-CI-003 | `CI=true` defaults to `--ephemeral`, `--log-format json`, `--lro-latency instant`, disables the web console unless --console is passed, and fails fast (non-zero exit within the wait timeout) if any requested service cannot start, with the reason on stderr. | M |
| FR-CI-004 | Runs on GitHub-hosted `ubuntu-24.04` and `ubuntu-24.04-arm` with the preinstalled Docker; no `sudo` needed for non-GKE services. | M |
| FR-CI-005 | Parallel jobs on one self-hosted runner don't collide (FR-CORE-033, port 0). | M |
| FR-CI-006 | Works inside a CI job container when the Docker socket is mounted (Docker-outside-of-Docker) and directly in GitLab CI, Buildkite and plain Docker. | S |
| FR-CI-007 | `gcpemu wait --for 'gke/cluster=PROJECT/LOCATION/NAME' --for 'lb/health=BACKEND_SERVICE'` blocks until conditions hold, for scripting. | S |
| FR-CI-008 | Exit code of `gcpemu run -- <cmd>` is the command's; the emulator is started before and torn down after. | S |
| FR-CI-009 | Go test helper package (`gcpemu/testing`) starts an in-process instance per test or package with chosen services and returns configured clients. Not available for GKE/Cloud SQL without a runtime. | S |
| FR-CI-010 | Pre-pulled image bundle (`gcpemu images save/load`) for air-gapped runners. | C |

Illustrative workflow:

```yaml
- uses: gcpemu/setup-gcpemu@v1
  with:
    services: gke,lb,cdn,dns,ar,gcs,pubsub,sql,iam,nat
    seed: test/emu-seed.yaml
- run: tofu -chdir=infra apply -auto-approve -var-file=emu.tfvars
- run: go test ./... -tags=e2e
```

## 10. Fidelity and out of scope

The emulator is real where behaviour is the point (Kubernetes, PostgreSQL, HTTP, DNS, OCI) and simulated where GCP's behaviour is infrastructure the user never sees.

### 10.1 Fidelity levels

| Level | Meaning |
| --- | --- |
| Real | The actual software runs; behaviour is exact for the user's purposes |
| Faithful | Reimplemented to match GCP's documented and observed behaviour, covered by the conformance suite |
| Recorded | Accepted, stored and returned by the API, but has no runtime effect |
| Absent | Returns `UNIMPLEMENTED` (gRPC) or HTTP 501 with the field or method named |

### 10.2 Fidelity by service

| Service | Real | Faithful | Recorded only |
| --- | --- | --- | --- |
| GKE | Kubernetes, container runtime, pod networking, Secrets Store CSI driver | Cluster API, node pools, Workload Identity, NEG sync, private nodes, Gateway API CRD install, Secret Manager add-on and secret synchronization | Machine types, autoscaling bounds, maintenance windows, logging/monitoring config |
| ALB | HTTP/TLS proxying | URL map semantics, health checks, backend mTLS, managed-cert lifecycle | Global anycast, regional failover, capacity scaler, Cloud Armor |
| Cloud CDN | HTTP cache | Cache modes, keys, TTLs, invalidation, signed URLs | Edge locations, cache fill between PoPs |
| Cloud NAT | Packet egress | Coverage rules, port allocation reporting, logging | Exact port-allocation algorithm, NAT IP reputation |
| Cloud SQL | PostgreSQL | Admin API, connectors, IAM auth, flags | Tier/CPU/RAM, HA failover, maintenance, Query Insights |
| Artifact Registry | OCI registry | AR API, auth | Vulnerability scanning, CMEK |
| Cloud Storage | Byte storage | JSON/XML API, generations, preconditions, signed URLs, notifications | Storage classes, location/dual-region, Autoclass, CMEK |
| Pub/Sub | — | Delivery, ordering, DLQ, filters, push, exactly-once | Message storage policy, CMEK |
| Cloud DNS | DNS server | Zones, record sets, changes | DNSSEC signing, anycast name servers |
| IAM | — | Policies, roles, tokens, impersonation, metadata server | Org policy, recommender, audit logs as a separate API |
| Secret Manager | — | Secrets, versions, aliases, expiry, delayed destruction, event notifications, managed rotation of Cloud SQL credentials | Replication policy, CMEK, the secret's own identity (`policy_member`) |

### 10.3 Out of scope for v1.0

- Any GCP service not in Section 1.2 (BigQuery, Cloud Run, Firestore, Cloud Logging and Monitoring APIs, KMS, etc.)
- Billing, quotas (except a configurable soft cap per resource type), budgets, SLAs
- Organisation, folder and Shared VPC hierarchies; VPC peering; Private Service Connect; interconnect and VPN
- Classic Application Load Balancer and network (L4) load balancers except the in-cluster `LoadBalancer` Service allocator
- GKE Autopilot, GKE Enterprise/fleet features, Config Sync, Binary Authorization, GPUs, Windows nodes
- Cloud SQL for MySQL and SQL Server, and AlloyDB
- Performance testing that depends on GCP's real latency, scale or network topology
- Running as a shared multi-user service; the emulator is single-user by design

## 11. Verification, acceptance and open questions

v1.0 is accepted when every **M** requirement has a passing automated test and the reference stack below runs green in CI on both architectures.

### 11.1 Verification methods

| Method | Covers |
| --- | --- |
| Unit tests per service package | Validation, state transitions, error codes |
| Conformance suite: the same Go tests run against the emulator and, nightly, against a real GCP sandbox project; responses diffed field by field (ignoring IDs and timestamps) | Faithful-level behaviour, drift detection |
| Client compatibility tests for every row of 7.1 | IF-001 to IF-009 |
| OpenTofu acceptance: apply → plan (expect no diff) → destroy for each supported resource type | IF-001 |
| Kubernetes conformance (Sonobuoy quick mode) on an emulated GKE cluster | FR-GKE-001 to FR-GKE-003 |
| Benchmarks in CI with thresholds | Section 8.1 |

### 11.2 Reference stack acceptance test

Runs from one OpenTofu root module and one Helm release, on `ubuntu-24.04` and `ubuntu-24.04-arm`, in ≤ 10 minutes including image pulls from cache:

1. `tofu apply` creates a VPC, subnet, Cloud Router + NAT, a private GKE cluster with Workload Identity, an AR Docker repo, a GCS bucket with static assets, a Pub/Sub topic + push subscription, a Cloud SQL Postgres 16 instance with an IAM SA user, a Cloud DNS zone, and a global external ALB with Cloud CDN, a backend bucket, a NEG backend service and backend mTLS.
2. A multi-arch Go API image is built and pushed to AR; Istio and the app are installed; the Istio ingress gateway requires mTLS from the LB.
3. `curl https://app.example.test/` returns the static page from the bucket; the second request reports `hit` via `{cdn_cache_status}`.
4. `curl https://app.example.test/api/health` reaches the Go service through LB → Istio; a direct request to the gateway without the LB client cert is refused.
5. The service writes an object to GCS, inserts a row in Cloud SQL via the Go connector with IAM auth, and publishes a message, all using Workload Identity and no endpoint config in the app.
6. The bucket notification and push subscription deliver to the service with a verifiable OIDC token.
7. An outbound request from a pod succeeds; after deleting the NAT it fails.
8. With IAM set to `enforce`, removing `roles/storage.objectAdmin` from the GSA makes step 5 fail with `PERMISSION_DENIED`.
9. `urlMaps.invalidateCache` on `/*` makes the next static request a `miss`.
10. `tofu destroy` succeeds and `gcpemu status` shows no leftover containers.

### 11.3 Decisions

| Question | Decision |
| --- | --- |
| Product name | Keep `gcpemu` |
| Kubernetes distribution | Each cluster's control plane in its own container (simpler; \~150 MB more RAM per cluster accepted) |
| Cloud SQL without a container runtime | No: containers only |
| IAM default mode | `audit` |
| Backend mTLS API surface | Follow the current googleapis/googleapis protos on GitHub |
| GKE on macOS | Linux-only for GKE in v1.0 |
| Autopilot | Excluded from v1.0 |

### 11.4 Release plan

| Milestone | Services | Exit criterion |
| --- | --- | --- |
| M1 | Core, IAM, GCS, Pub/Sub, DNS, AR | Go libs, gcloud and OpenTofu tests green |
| M2 | Cloud SQL, GKE, NAT | Workload Identity and private cluster tests green |
| M3 | ALB, CDN, backend mTLS, NEGs | Reference stack steps 1–4 and 9 green |
| v1.0 | All | Full reference stack on both arches; all **M** requirements tested |

The web console ships with each milestone: the shell, dashboard, request log and IAM views in M1, then each service's view in the milestone that delivers the service.
