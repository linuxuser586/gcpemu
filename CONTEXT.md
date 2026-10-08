# gcpemu

A local emulator of a subset of Google Cloud, so that real clients (gcloud, OpenTofu, SDKs, kubectl, psql) can run against a laptop or CI runner instead of a GCP project.

## Language

### The emulator

**Instance**:
One running emulator process together with its state. Two processes are always two Instances, even if they share an Instance name.
_Avoid_: emulator (when a specific running one is meant), server

**Instance name**:
The user-chosen label for an Instance's configuration, e.g. "default".

**Instance ID**:
The unique identifier that tags everything an Instance owns, such as its containers and networks.

**Product**:
A GCP product the emulator promises to cover, as counted in the SRS (e.g. Cloud SQL, Cloud Load Balancing with Cloud CDN). There are ten.

**Service**:
An independently enabled unit of emulation that implements all or part of a Product. One Product may be split across several Services, which is why there are more Services than Products.
_Avoid_: module, component, emulated API

**Service ID**:
The short canonical name of a Service (e.g. `gcs`, `sql`, `gke`, `ar`). It is used everywhere the emulator chooses the name: flags, config, latency keys and endpoint listings.
_Avoid_: using the API name (`storage`, `sqladmin`, `container`) except where GCP's wire format requires it

**API name**:
GCP's own name for the API a Service serves (e.g. `sqladmin`, `container`). It appears only on the wire.

**Control plane**:
The resource-management APIs that create, read, update and delete GCP resources.

**Data plane**:
The traffic those resources carry: object bytes, load-balanced requests, Postgres connections, DNS queries.

**Service endpoint**:
The address at which an Instance serves one Service.
_Avoid_: endpoint (unqualified)

**Endpoint override**:
Client-side configuration (an env var, provider setting or SDK option) that points a client at a Service endpoint instead of at Google.

**API gateway**:
The single listener that receives Control plane requests and routes each one to the right Service.
_Avoid_: gateway (unqualified)

**Host mode**:
A way of running an Instance so that it answers on real Google hostnames, using local DNS and a locally trusted CA. Clients then need no Endpoint override.
_Avoid_: "mode" for anything else

**Google front end**:
The component that terminates TLS for real Google hostnames in Host mode and hands requests to the API gateway. Its in-container piece is the *host-mode relay*.
_Avoid_: frontend (unqualified)

**Managed container**:
A container that an Instance starts, owns and removes, e.g. a Cloud SQL instance's Postgres, a GKE node, the NAT egress proxy, the host-mode container.

**Sidecar agent**:
The emulator's helper process that runs inside a Managed container.
_Avoid_: agent (unqualified)

**NAT egress proxy**:
The Managed container through which Cloud NAT traffic leaves the emulated network.
_Avoid_: egress gateway

**State snapshot**:
A named, saved copy of all of an Instance's state that can be restored later.
_Avoid_: snapshot (unqualified, collides with Pub/Sub snapshot)

**Reset**:
Removing all resources and data from an Instance while keeping its configuration. It does not re-apply the Seed file. To get back to a known state, restore a State snapshot instead.

**Seed file**:
A declarative file listing the Projects, resources and Fault rules an Instance should contain.

**Seeding**:
Applying a Seed file to an Instance. It is idempotent, and at startup it finishes before the Instance reports ready.

**Ephemeral Instance**:
An Instance whose state disappears when it exits.

**Persistent Instance**:
An Instance that keeps its state in a data directory across restarts.

**Emulator clock**:
The time an Instance believes it is. It can be advanced, but never rewound, to trigger time-driven behaviour.

**Deterministic Instance**:
An Instance whose generated IDs and timestamps are reproducible across runs, for golden-file tests.
_Avoid_: deterministic mode

**Fault rule**:
A rule that makes matching requests fail, drop or slow down on purpose. Using them is *Fault injection*. Injected latency delays a response, unlike Operation latency, which delays an Operation's completion.

**Principal**:
The identity a request is attributed to for IAM checks.

**Default principal**:
The Principal assumed for requests that carry no credentials.

**Web console**:
The browser UI an Instance serves for inspecting and operating its Services.

**IAM enforcement level**:
Whether an Instance ignores IAM (off), logs denials but allows them (audit), or rejects them (enforce). Audit is the default.
_Avoid_: IAM mode

### Fidelity

**Fidelity**:
How closely an emulated behaviour matches GCP. It is stated per behaviour as one of the four Fidelity levels.

**Real**:
Fidelity level for behaviour performed by the actual upstream software (e.g. Postgres for Cloud SQL, k3s for GKE).

**Faithful**:
Fidelity level for behaviour reimplemented by the emulator and conformance-tested against GCP.

**Recorded**:
Fidelity level for configuration that is stored and returned but has no effect.
_Avoid_: using "recorded" for logging or capturing traffic. Say "logged" or "captured" instead.

**Absent**:
Fidelity level for behaviour that is not emulated and answers UNIMPLEMENTED.

### GCP concepts as the emulator treats them

**Operation**:
A GCP resource representing asynchronous work on another resource. Every Service's operations, including Cloud DNS changes, are one concept regardless of their wire format.
_Avoid_: LRO, job, task

**Operation latency**:
How long an Operation takes before it completes, configured per Service ID. Operations complete instantly unless it is set.

**Project**:
A GCP project that exists in an Instance. It is created implicitly the first time a request references it, unless the Instance runs with Strict projects. Only a request handled by a Service counts as a reference. Choosing a Project in the Web console only scopes what is shown and creates nothing.

**Strict projects**:
An Instance setting under which referencing a Project that does not exist is an error instead of creating it.

**Cloud SQL instance**:
A Cloud SQL database server resource. Always write it in full so it is not confused with an Instance.
_Avoid_: instance (unqualified)

**Request log**:
The emulator's log of API requests it has received.
_Avoid_: recording, records

### Testing

**Client compatibility suite**:
Tests that drive real CLIs and SDKs against an Instance to prove they work unmodified.
_Avoid_: compat tests (when OpenTofu acceptance is meant too)

**OpenTofu acceptance**:
Tests that apply, re-plan and destroy OpenTofu configurations covering every resource type behind the Products.

**Reference stack**:
An end-to-end deployment of a representative application across all Products, used as the final acceptance test.
_Avoid_: "acceptance" on its own, e2e

## Flagged ambiguities

- "Service" also appears as GCP and Kubernetes terms. Always qualify those: *backend service*, *Kubernetes Service*, *service account*. The container network that Services share is the *services network*.
- "Snapshot" on its own is ambiguous. Use *State snapshot* or *Pub/Sub snapshot*.
- "Endpoint" on its own is ambiguous. Use *Service endpoint*, *Endpoint override*, or GCP's *network endpoint* (NEG).
- "Gateway" and "frontend" on their own are ambiguous. Load balancer frontends and the network's gateway IP keep their GCP meanings and are always qualified.
- The Cloud SQL Auth Proxy keeps its product name and is not a Sidecar agent.
