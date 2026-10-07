# gcpemu

A single Go binary that emulates Google Cloud services on a workstation or in CI, so the
same OpenTofu modules, client SDKs and application code that target GCP run unmodified
except for endpoint configuration. See [the SRS](GCP%20Local%20Emulator%20Software.md).

## Status

Milestones **M1** (core, IAM, Cloud Storage, Pub/Sub, Cloud DNS, Artifact Registry) and
**M2** (compute networking, Cloud SQL, GKE, Cloud NAT) are implemented. M3 (ALB, CDN,
backend mTLS) is not started.

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
| GKE | `services/gke` | container v1 (gRPC + REST) over real k3s clusters; node pools, IAM-backed kube auth, Workload Identity, private nodes via NAT, NEG sync |

## Quick start

```sh
make build
bin/gcpemu start --detach --services gcs,pubsub --seed seed.yaml
eval "$(bin/gcpemu env)"       # STORAGE_EMULATOR_HOST, PUBSUB_EMULATOR_HOST, GCE_METADATA_HOST, …
bin/gcpemu status
bin/gcpemu tofu-provider --project my-project   # provider "google" block with custom endpoints
bin/gcpemu stop
```

Other commands: `reset`, `logs [service] [--requests]`, `time advance 24h`,
`fault add|list|clear`, `doctor`, `version`.

### Endpoints

All control-plane APIs are served by the gateway (default `127.0.0.1:4510`), under
`/<api>/` (for example `/pubsub/v1/…`, `/iam/v1/…`) and at their native root paths where
unambiguous (`/storage/v1/`, `/dns/v1/`, `/compute/v1/`, `/sql/v1beta4/`). Per-service ports:
GCS 4443, Pub/Sub 8085, registry 5000, DNS 5353 (UDP+TCP), metadata 8988, Cloud SQL host ports
54320+. `gcpemu env` also exports `KUBECONFIG` for GKE clusters. `--port name=0` picks a free port; the
choices are written to `<data-dir>/endpoints.json` and printed by `gcpemu env`.

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

## Development

```sh
make vet race      # gofmt, vet, tests with -race
make release       # static binaries for linux/darwin × amd64/arm64
```

Each service is an isolated package implementing `emu.Service`
(`internal/emu/emu.go`); cross-service contracts live in `internal/emu/xservice.go`.

Licensed under Apache-2.0.
