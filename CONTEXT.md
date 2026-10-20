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
A GCP product the emulator promises to cover, as counted in the SRS (e.g. Cloud SQL, Cloud Load Balancing with Cloud CDN, Secret Manager, Cloud Build). There are twelve.

**Service**:
An independently enabled unit of emulation that implements all or part of a Product. One Product may be split across several Services, which is why there are more Services than Products.
_Avoid_: module, component, emulated API

**Service ID**:
The short canonical name of a Service (e.g. `gcs`, `sql`, `gke`, `ar`). It is used everywhere the emulator chooses the name: flags, config, latency keys and endpoint listings.
_Avoid_: using the API name (`storage`, `sqladmin`, `container`) except where GCP's wire format requires it

**Runtime Service**:
A Service that does nothing without a container runtime, because its resources are Managed containers (Cloud SQL, GKE, Cloud NAT, Cloud Build). An Instance with no usable runtime runs every other Service.
_Avoid_: container service, container-backed service

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

**Escaped request**:
A request that a client configured against an Instance still sends to a real Google host, because no Endpoint override or Host mode covers it.
_Avoid_: leak, outbound call

**Read-through**:
A read of a bucket the Instance doesn't have that the Google front end makes from real Cloud Storage for a caller on a `--gfe-bind` address carrying none of the Instance's credentials, such as the Go module proxy's downloads (ADR 0021).
_Avoid_: escape, passthrough, proxying (unqualified)

**Invalid universe**:
The universe domain `gcpemu.invalid` that the emulator gives clients, so that an API with no Endpoint override goes to a host that can never resolve and fails loudly instead of becoming an Escaped request. It does not reach hosts a client hard-codes outside any universe, such as the mTLS endpoints some libraries hard-code. Service-account keys created at the API gateway's address and the metadata server that host processes use belong to it, and gcloud refuses credentials stored for real Google. Keys created through the Google front end belong to Google's universe instead, since their clients reach the Instance by Google's hostnames (ADR 0021).
_Avoid_: fake universe, emulator universe

**Foreign credentials**:
Credentials a client picks up that no Instance issued, such as a service-account key or gcloud's application-default login from real Google. Their token requests go to real Google, so they become Escaped requests even where every API has an Endpoint override.
_Avoid_: real keys, external credentials (GCP's `external_account` is one kind of Foreign credentials)

**API gateway**:
The single listener that receives Control plane requests and routes each one to the right Service.
_Avoid_: gateway (unqualified)

**Host mode**:
A way of running an Instance so that it answers on real Google hostnames, using local DNS and a locally trusted CA. Clients then need no Endpoint override.
_Avoid_: "mode" for anything else

**Instance CA**:
The root certificate authority an Instance creates the first time it starts. It signs Google-managed certificates and Host mode serving certificates, and clients trust it once. It is made of a certificate and a private key, which always belong together: an Instance never uses one without the other. An Instance keeps its CA until the user rotates it, and never replaces either part on its own. Rotating it replaces the certificate and key together: Google-managed certificates are re-issued from the new CA, and self-managed certificates are the user's and are left as they are.
_Avoid_: local CA, root CA, emulator CA

**Host trust**:
The places on the user's machine that trust the Instance CA: the OS certificate store, container runtimes' registry trust and browser certificate databases. `gcpemu ca install` adds the Instance CA to them and `gcpemu doctor` reports where it is. A place is part of Host trust once it holds the Instance CA, even if a client on the host is set up not to read it, such as a kubelet whose containerd does not read the registry trust; doctor reports that client separately.
_Avoid_: trust store (that is part of a Trust config)

**Google front end**:
The component that terminates TLS for real Google hostnames in Host mode and hands requests to the API gateway. Its in-container piece is the *host-mode relay*. With `--gfe-bind` it also serves another machine on the LAN, to real credentials only, and makes Read-throughs there (ADR 0021).
_Avoid_: frontend (unqualified)

**Managed container**:
A container that an Instance starts, owns and removes, e.g. a Cloud SQL instance's Postgres, a GKE node, the NAT egress proxy, the host-mode container.

**Disrupted container**:
A Managed container that the container runtime stopped or started again without the Instance doing it, e.g. when a VM runtime or daemon restarts. An Instance with one is not ready until it is stopped and started.
_Avoid_: crashed container, dead container

**Sidecar agent**:
The emulator's helper process that runs inside a Managed container.
_Avoid_: agent (unqualified)

**NAT egress proxy**:
The Managed container through which Cloud NAT traffic leaves the emulated network.
_Avoid_: egress gateway, NAT gateway

**NAT gateway**:
One Cloud NAT configuration on a Cloud Router, as GCP names it: which subnetwork ranges it translates and with which NAT IPs. The NAT egress proxy carries out what the NAT gateways of a VPC network say.
_Avoid_: NAT config (unqualified), NAT (unqualified, when the configuration is meant)

**Edge relay**:
The Managed container that relays clients to the load balancer's Data plane on forwarding rule ports the emulator cannot bind itself, serves external forwarding rule addresses to VPC workloads, and relays the Data plane to backends the host cannot route to, such as GKE pods.
_Avoid_: edge (unqualified), LB proxy

**LAN front-end relay**:
The Managed container that listens on `--gfe-bind` addresses at port 443 when the emulator cannot bind them itself, and relays each connection to the Google front end, which checks its credentials as on any `--gfe-bind` listener.
_Avoid_: gfe relay, Edge relay (that is the load balancer's)

**VM runtime**:
A container runtime whose networks live inside a virtual machine the host cannot route into, e.g. Docker Desktop, Colima, OrbStack or Podman machine on macOS, Docker Desktop or Podman machine on Windows, and Docker Desktop on Linux.

**VM relay**:
The Managed container through which an Instance on a VM runtime reaches its other Managed containers, and they reach it.
_Avoid_: bridge container, tunnel

**Loopback alias**:
An extra `127.0.0.x` address on the host's loopback interface. Linux and Windows have every one of them; macOS has only `127.0.0.1` until `gcpemu loopback install` adds the rest. Each Cloud SQL instance and each forwarding rule gets its own when they exist, and so does each Kubernetes Service a host client reaches through the Host attachment.
_Avoid_: loopback IP (unqualified), alias (unqualified)

**Host attachment**:
The development aid, not modelled on GCP, that links the host with an Instance's VPC networks in both directions: host clients reach a GKE cluster's Kubernetes Services by name, and VPC workloads reach Offered ports at the Host address. It exists so that a process on the host can stand in for a pod, which GCP has no equivalent for (ADR 0014).
_Avoid_: VPN, tunnel, bridge

**Host address**:
The address at which workloads on one VPC container network reach the host: the VPC container network's own gateway IP where the host can route to it, and an address the VM relay holds on a VM runtime. It stays the same for the life of a Persistent Instance. It is not the subnetwork's Gateway address, though the two can be the same IP when there is no Fallback range.
_Avoid_: host IP, gateway (unqualified)

**Offered port**:
A host port that a caller has opened to VPC workloads at the Host address. It stays offered until it is withdrawn, across restarts of a Persistent Instance.
_Avoid_: exposed port, declared port

**LoadBalancer Service ingress**:
The address a GKE `LoadBalancer` Service reports as its own, at which host clients reach it: a node address on the cluster's VPC; on a VM runtime or a Translated network, a loopback address the emulator serves; for a Containerized Instance on a Translated network, its nodes' Fallback-range addresses; or, for a Containerized Instance on a VM runtime, the host side of a Published port (ADR 0012), empty while none is free.
_Avoid_: external IP, LB address

**Published port**:
A port that a Containerized Instance's own container publishes to the host. On a VM runtime it is the only way host clients reach a Containerized Instance's GKE `LoadBalancer` Services, and on any runtime it is the only way they reach its forwarding rules (ADR 0012). Its host side, not the container port, is what the Instance reports.
_Avoid_: exposed port, mapped port

**Own port**:
The port a GKE `LoadBalancer` Service or a forwarding rule declares, as opposed to the Published port it ends up served on. A Service is served on its own port only when the container publishes it at the same host port, since its clients expect that port on the host; a forwarding rule is served on it at any host port, since its clients learn the host side from the rule's environment variable.
_Avoid_: service port, rule port (unqualified)

**Excluded port range**:
A range of ports that Windows reserves for Hyper-V, WSL and others, which no process on the host can bind. Windows has no privileged ports, so a port it refuses is in one of these ranges. A forwarding rule or Kubernetes Service on such a port is unavailable to host clients; the Edge relay is never used for it, since its host port would hit the same reservation (ADR 0015).
_Avoid_: reserved port, privileged port (on Windows)

**State snapshot**:
A named, saved copy of all of an Instance's state that can be restored later.
_Avoid_: snapshot (unqualified, collides with Pub/Sub snapshot)

**Datastore**:
A GKE cluster's k3s server state: the k3s server's `server/` directory, holding the kine sqlite database, the cluster CA and credentials, and the join token. A State snapshot saves the Datastore of each RUNNING cluster, so the cluster keeps its Kubernetes objects and CA when the snapshot is restored, including into another Instance. Node volumes (pulled images and local-path PersistentVolume data) are not part of it. A cluster that is not RUNNING when the snapshot is saved comes back with a new Datastore: none of its earlier Kubernetes objects and a new cluster CA.
_Avoid_: seed (collides with Seed file and Seeding), etcd (k3s here uses kine on sqlite)

**Staged datastore**:
A Datastore restored from a State snapshot that has not yet been written into its cluster's server volume. The cluster's server takes it on its next start, before the server container first runs. Reset and deleting the cluster drop it.

**Reset**:
Removing all resources and data from an Instance while keeping its configuration. It does not re-apply the Seed file. To get back to a known state, restore a State snapshot instead. While a Reset or a State snapshot restore runs, the Instance is not ready and refuses every request but the Admin API's as unavailable, cutting off those already in progress.

**Seed file**:
A declarative file listing the Projects, resources and Fault rules an Instance should contain.

**Seeding**:
Applying a Seed file to an Instance. It is idempotent, and at startup it finishes before the Instance reports ready.

**Wait condition**:
A named state of an emulated resource, written `KIND=TARGET`, that a script can block on until it holds or can no longer hold, e.g. a GKE cluster that is running or a backend service that can take traffic. A resource that does not exist yet has not met its Wait condition; one that has failed or is being deleted never will.
_Avoid_: condition (on its own), readiness check

**Ephemeral Instance**:
An Instance whose state disappears when it exits.

**Persistent Instance**:
An Instance that keeps its state in a data directory across restarts.

**Containerized Instance**:
An Instance that itself runs in a container, such as the OCI image, and joins its own networks instead of relying on the host's (ADR 0003). An Instance in a container on the host's network is not one: it runs as on the host. It still sees only its own container's files, such as its trust store, not the host's.
_Avoid_: container mode, in-container, DooD, host-network mode (for a container on the host's network, which is not one)

**Abandoned Instance ID**:
An Instance ID that still tags containers, networks or volumes on the host but that no data directory holds, so no future Instance will reclaim them.
_Avoid_: orphan, dead instance

**Emulator clock**:
The time an Instance believes it is. It can be advanced, but never rewound, to trigger time-driven behaviour. In a Deterministic Instance it moves only when an API request arrives or the clock is advanced on purpose, never because real time passed.

**Deterministic Instance**:
An Instance whose generated IDs and timestamps are reproducible across runs, for golden-file tests. The guarantee holds for a client that sends one API request at a time; console and admin traffic do not affect it.
_Avoid_: deterministic mode

**Fault rule**:
A rule that makes matching requests fail, drop or slow down on purpose. Using them is *Fault injection*. Injected latency delays a response, unlike Operation latency, which delays an Operation's completion.

**Principal**:
The identity a request is attributed to for IAM checks.

**Default principal**:
The Principal assumed for requests that carry no credentials.

**Web console**:
The browser UI an Instance serves for inspecting and operating its Services.

**Admin API**:
The emulator-specific API under `/_emu/` for inspecting and operating an Instance (health, reset, fault injection, the request log and the like). It is the only interface not modelled on GCP, and the Web console is one of its clients.

**Admin token**:
A secret generated at each start that every Admin API and Web console request must present when the API gateway is bound beyond loopback. Public GCP APIs never need it, because IAM governs those.
_Avoid_: session token, console token

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
How long an Operation takes before it completes, configured per Service ID (`gke=20s`, `lb=5s`), with `*` for every Service. The API names `container` and `sqladmin` still work as aliases for `gke` and `sql`. Operations complete instantly unless it is set.

**Project**:
A GCP project that exists in an Instance. It is created implicitly the first time a request references it, unless the Instance runs with Strict projects. Only a request handled by a Service counts as a reference. Choosing a Project in the Web console only scopes what is shown and creates nothing.

**Connect gateway**:
GKE's fleet API through which clients reach a cluster's Kubernetes API as their own Principal. The emulator treats every GKE cluster as a fleet membership named like the cluster. It is how the Web console reads namespaces, workloads, pods and logs (ADR 0002).

**Secret Manager secret**:
A named container in Secret Manager whose payloads are held in its Secret versions. Always qualify it so it is not confused with a Kubernetes Secret.
_Avoid_: secret (unqualified)

**Secret version**:
One immutable payload of a Secret Manager secret, which can be enabled, disabled or destroyed.

**Secret sync**:
GKE's copying of Secret versions into Kubernetes Secrets inside a cluster.

**Deny policy**:
An IAM v2 policy attached to a Project (or a folder or organization) that refuses the permissions it names to the Principals it names before any allow policy is consulted, so no role grants them back. It binds the Default principal too.
_Avoid_: deny rule (one rule inside a Deny policy)

**Strict projects**:
An Instance setting under which referencing a Project that does not exist is an error instead of creating it.

**Certificate map**:
A Certificate Manager resource that a global target proxy serves TLS from, through its Certificate map entries, choosing a certificate by the hostname the client asks for (SNI).
_Avoid_: cert map, map (unqualified)

**Certificate map entry**:
One rule of a Certificate map: a hostname, or a wildcard of it one label deep, and the one to four global certificates of scope `DEFAULT` that serve it. A Certificate map has at most one entry per hostname.
_Avoid_: map entry, entry (unqualified)

**Primary entry**:
The Certificate map entry that serves every hostname no other entry of its map matches, and any client that names no hostname. An exact hostname wins over a wildcard, and a wildcard over the Primary entry.
_Avoid_: default entry, fallback entry

**DNS authorization**:
A Certificate Manager resource that proves control of one domain (and its subdomains) by a CNAME record it asks to be added to the domain's DNS zone. A Google-managed certificate names the DNS authorizations of its domains, and is issued once each record resolves. Its domain and type cannot be changed.
_Avoid_: domain verification, DNS challenge (that is the record, not the resource)

**Certificate issuance config**:
A Certificate Manager resource that has a CA Service CA pool issue the Google-managed certificates that name it, with their lifetime, rotation window and key algorithm, none of which can be changed once it is created.
_Avoid_: issuance config (in user-facing text, except where space is short), CA config

**Trust config**:
A Certificate Manager resource holding the CAs (trust anchors and intermediate CAs) and allowlisted certificates that verify the other side of mutual TLS. Server TLS policies and Backend authentication configs name one.
_Avoid_: trust store (that is one part of a Trust config), CA bundle

**Server TLS policy**:
A Network Security resource that a target HTTPS proxy names to verify its clients' certificates against a Trust config (mutual TLS), either rejecting clients that fail or letting them through.
_Avoid_: mTLS policy, TLS policy (unqualified)

**Backend authentication config**:
A Network Security resource that a backend service names to verify its backends' certificates against a Trust config or the public roots, optionally presenting a client certificate of its own.
_Avoid_: backend TLS policy, BAC (in user-facing text)

**Client TLS policy**:
A Network Security resource for the client side of service-to-service TLS (Cloud Service Mesh), with the SNI, client certificate and CAs a client uses. The load balancer does not use it.
_Avoid_: TLS policy (unqualified)

**TLS policies**:
The section of the Certificate Manager view that lists Server TLS policies, Backend authentication configs and Client TLS policies together, though only two of the three are named "policy" by the API.
_Avoid_: using it for a single resource

**Cloud SQL instance**:
A Cloud SQL database server resource. Always write it in full so it is not confused with an Instance.
_Avoid_: instance (unqualified)

**On-demand backup**:
A backup of a Cloud SQL instance taken at someone's request while the Cloud SQL instance exists. It never expires: it lasts until it is deleted, or until its Cloud SQL instance is deleted unless it becomes a Retained backup. Always qualify it, so it is not confused with a Final backup.
_Avoid_: manual backup

**Final backup**:
A backup of a Cloud SQL instance taken as part of deleting it. It outlives the Cloud SQL instance and is deleted when it expires. Unlike a Retained backup, the delete takes it. Always qualify it, so it is not confused with an on-demand backup taken while the Cloud SQL instance exists.
_Avoid_: last backup, deletion backup

**Retained backup**:
An on-demand backup that outlives its deleted Cloud SQL instance because the Cloud SQL instance was set to keep its backups on delete. A Final backup also outlives its Cloud SQL instance, but it is not a Retained backup. Always qualify it.
_Avoid_: orphaned backup, kept backup

**Recovery window**:
The span from the earliest to the latest time a Cloud SQL instance with point-in-time recovery can be recovered to. Its start is set by the transaction log retention period, measured on the Cloud SQL instance's own database clock.
_Avoid_: retention window, PITR window

**Base backup**:
A physical copy of a Cloud SQL instance's data that point-in-time recovery replays the transaction log on top of. It is kept or discarded by transaction log retention alone. Always qualify it, so it is not confused with an on-demand backup or a Final backup.

**WAL archive**:
A Cloud SQL instance's archived transaction log together with its Base backups: everything its Recovery window is built from.
_Avoid_: archive (unqualified), transaction log storage

**Request log**:
The emulator's log of API requests it has received.
_Avoid_: recording, records

**Declared range**:
The CIDR that a subnetwork or a private services access allocation has in GCP's API, as the user declared it.

**Gateway address**:
The address GCP reports as a subnetwork's gateway: the second address of its Declared range, after the network address, whatever range its VPC container network uses.
_Avoid_: gateway (unqualified), Host address

**VPC container network**:
The container network that backs one subnetwork or one private services access allocation of a VPC network. A VPC network has as many as it has subnetworks and allocations, and the Managed containers attached to a subnetwork hold their addresses on it.
_Avoid_: VPC bridge, network (unqualified, when the container network rather than the GCP resource is meant)

**Fallback range**:
The CIDR that a VPC container network uses instead of its Declared range, because the Declared range overlapped another network on the host.
_Avoid_: actual range, real range

**Translated network**:
A VPC container network on a Fallback range whose Managed containers still hold, and report, addresses from the Declared range, aliased onto their VPC interfaces (ADR 0004). A network that cannot be translated reports Fallback-range addresses.

**Build**:
One execution of a Cloud Build config: its Build steps run in order or by their declared dependencies, and its images are pushed when they finish.
_Avoid_: job, pipeline, run (as a noun)

**Build step**:
One container in a Build, sharing the Build's workspace with the other Build steps.
_Avoid_: stage, task

**Build trigger**:
A stored recipe that starts a Build when it is run or when the event it watches arrives.
_Avoid_: trigger (unqualified)

**Build daemon**:
The Managed container, one per Instance, whose Docker daemon every Build's steps use to build, run and push images. Its image cache outlives Builds and Reset.
_Avoid_: dind, docker host

**Build agent**:
The Sidecar agent that gives Build steps the metadata server, Google hostnames and Artifact Registry as the Build's service account.
_Avoid_: build sidecar

**Substitution**:
A `$KEY` variable expanded in a Build's config when the Build is created, or, when automapped, set as environment variables in its Build steps. *Built-in substitutions* come from the Build itself (its Project, ID, location and service account) or from its source or Build trigger. *User substitutions* are the `_KEY` values the caller supplies.
_Avoid_: variable, parameter, template var

### Testing

**Client compatibility suite**:
Tests that drive real CLIs and SDKs against an Instance to prove they work unmodified.
_Avoid_: compat tests (when OpenTofu acceptance is meant too)

**Conformance suite**:
Tests that make the same requests of an Instance and of a real GCP project and compare the responses field by field, ignoring values that legitimately differ, such as IDs and timestamps. It is how Faithful behaviour is checked against GCP.
_Avoid_: "conformance" on its own, parity tests

**Recording**:
GCP's responses to the Conformance suite, captured from a real project and kept so that the emulator's responses can be compared with them without reaching GCP.
_Avoid_: golden, snapshot

**Drift**:
GCP's responses changing from one Recording to the next. It means the emulator may no longer be Faithful, even though nothing in the emulator changed.
_Avoid_: using "drift" for the emulator disagreeing with a Recording; that is a Mismatch

**Mismatch**:
The emulator's response to a Conformance suite request differing from the Recording.
_Avoid_: drift, diff

**OpenTofu acceptance**:
Tests that apply, re-plan and destroy OpenTofu configurations covering every resource type behind the Products.

**Kubernetes conformance**:
Upstream Kubernetes' own conformance tests, run with Sonobuoy against an emulated GKE cluster at each supported minor version. It checks the clusters, not their agreement with GCP.
_Avoid_: "conformance" on its own

**Reference stack**:
An end-to-end deployment of a representative application across all Products, used as the final acceptance test.
_Avoid_: "acceptance" on its own, e2e

**Containerized Instance tests**:
Tests that build the OCI image and drive a Containerized Instance from the host.
_Avoid_: image tests

**Image readiness check**:
Starting a release's amd64 OCI image, with the host's container runtime, and requiring it to report ready before any of its tags is pushed. Unlike the Containerized Instance tests, it runs the image built from the release's binaries.
_Avoid_: smoke test

**Signed release**:
A release whose `SHA256SUMS` comes with a Sigstore bundle proving it was produced by this repository's release workflow for that same tag. Releases from the first signed release onward are always signed; setup-gcpemu refuses one that isn't, and only warns for releases before it.
_Avoid_: "verified release" (setup-gcpemu always hash-verifies the binary, signed or not)

**Escape guard**:
A proxy that the OpenTofu acceptance, the Reference stack and the gcloud runs in the Client compatibility suite go through, so that any request leaving the machine fails the test, naming the host. Loopback never goes through it. In a Host mode test it passes requests for the hostnames Host mode serves to the Google front end, which runs on the same machine.
_Avoid_: egress check, leak detector

## Flagged ambiguities

- "Service" also appears as GCP and Kubernetes terms. Always qualify those: *backend service*, *Kubernetes Service*, *service account*. The container network that Services share is the *services network*.
- "Snapshot" on its own is ambiguous. Use *State snapshot* or *Pub/Sub snapshot*.
- "Datastore" on its own always means a GKE cluster's k3s Datastore. The GCP product is always called *Cloud Datastore* or *Firestore in Datastore mode*.
- "Endpoint" on its own is ambiguous. Use *Service endpoint*, *Endpoint override*, or GCP's *network endpoint* (NEG).
- "Gateway" and "frontend" on their own are ambiguous. Load balancer frontends and the network's gateway IP keep their GCP meanings and are always qualified.
- "Secret" on its own is ambiguous. Use *Secret Manager secret*, *Secret version* or *Kubernetes Secret*.
- "Build" on its own is ambiguous: it also means compiling gcpemu, a Docker image build or the Web console's Vite build. Use *Build* (capitalised) only for a Cloud Build execution, and say *image build* or *compile* for the others.
- "Map" and "entry" on their own are ambiguous: URL maps have host and path rules, and certificate maps have entries. Use *URL map*, *Certificate map* and *Certificate map entry*.
- A *global* certificate is one in location `global`. That is its location, not its scope: a certificate with scope `ALL_REGIONS` is also in `global`, but serves cross-region load balancers.
- "Trigger" on its own is ambiguous. Use *Build trigger*; a Fault rule *matches*, it is not triggered.
- The Cloud SQL Auth Proxy keeps its product name and is not a Sidecar agent.
- "Condition" on its own is ambiguous: Kubernetes objects inside a GKE cluster have their own conditions. Use *Wait condition* for what a script blocks on.
- "Kubernetes pulls" on its own is ambiguous: an emulated GKE cluster's nodes pull with their own registry settings, apart from Host trust. Say *a kubelet on the host* for a Kubernetes cluster that runs directly on the user's machine and pulls through the host's containerd.
- "Conformance" on its own is ambiguous: the *Conformance suite* compares the emulator's responses with real GCP's, while *Kubernetes conformance* runs upstream's tests against an emulated cluster. Always qualify it.
