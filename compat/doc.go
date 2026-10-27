// Package compat is the client compatibility suite (SRS 7.1, IF-003 to
// IF-007 and IF-009): the real gcloud, the aws CLI v2, kubectl with
// gke-gcloud-auth-plugin, Helm, Docker, crane, ko, psql, the Cloud SQL Auth
// Proxy v2 and dig run against a detached gcpemu instance with only endpoint
// configuration.
//
//	go test -tags compat ./compat -v -timeout 30m
//
// gcloud (with the gke-gcloud-auth-plugin component), the aws CLI, kubectl,
// dig and Docker come from PATH; their tests skip when one is missing.
// Helm, crane, ko and the Auth Proxy are downloaded at pinned,
// checksum-verified versions into $GCPEMU_COMPAT_CACHE (default <user cache
// dir>/gcpemu-compat), and psql runs from the PostgreSQL image. Cloud SQL,
// GKE and the Docker steps need a container runtime, which native Windows
// cannot use (ADR 0008), so only crane and ko are pinned for Windows. Some steps (crane
// copy from Docker Hub, ko's base image) need the internet. gcloud runs
// behind an Escape guard (internal/escapeguard), so a gcloud request that
// no endpoint override covers fails the step instead of reaching Google.
// GCPEMU_COMPAT_REQUIRE=1, which the Linux CI jobs set, fails a test whose
// client toolchain is missing or won't install instead of skipping it.
//
// The gcloud/storage subtests (#599) also sync a tree up with gcloud
// storage rsync, again with --delete-unmatched-destination-objects after a
// change and a deletion, and back down, comparing names and bytes. They
// sign a URL with gcloud storage sign-url and an emulator-issued key, fetch
// it with no credentials, and check that a modified signature and an
// expired URL are refused.
//
// The aws subtests (#600) use an HMAC key the emulator issued, against the
// XML API with path-style addressing: aws s3 cp of a small object and of
// one that goes multipart, with --metadata, then ls and rm; the same
// four-step sync round trip as gcloud's, with --delete; aws s3 presign,
// with a modified signature refused; and a wrong secret, an inactive key
// and a deleted key each refused with a 403. The aws CLI runs behind the
// Escape guard with every AWS_* variable from the environment dropped and
// its config and credentials files in a temp directory, so the developer's
// AWS profile is never read.
//
// The universe subtests (#215) probe the Python and Node client libraries
// with an emulator-issued key, and Python google-auth with the metadata
// server (#244), from the scripts in sdk/. They install pinned library
// versions into the same cache with python3 (python on Windows) -m venv
// and npm, and skip when either is missing or the install fails. The
// Python probes run behind the Escape guard. The go-mtls subtest (#243)
// builds the Go probe in sdk/universego and runs it behind the Escape
// guard, which names the host it dialed. The node-mtls subtest (#246)
// does the same with sdk/universemtls.js.
//
// The flows subtests (#601, IF-008) run sdk/flows.py and sdk/flows.js,
// with the same pinned libraries plus Node Cloud Storage, configured only
// by `gcpemu env` and the scheme its STORAGE_EMULATOR_HOST leaves out
// (Node takes it as an apiEndpoint): each creates a bucket, writes, reads back, lists and
// deletes an object of binary content, and creates a topic and a
// subscription, publishes a message with an attribute and receives it
// through the library's streaming-pull subscriber within a bounded time.
// Both run behind the Escape guard, and a canary run without the emulator
// variables must reach the guard.
//
// The gcloud/builds subtests (#393) run gcloud builds submit with
// --config and --tag, builds list and cancel, and create and run a Build
// trigger. They also build the Go probe in sdk/buildgo, which creates a
// Build with the Cloud Build client. gcloud can't stream the log without
// Host mode (SRS 11.3), so they pass --async or set options.logging NONE.
// TestHostMode (Linux only) starts an Instance with --host-mode and runs
// the same submits with the log streaming. The Escape guard tunnels the
// hostnames Host mode serves to the Google front end, so the test changes
// neither /etc/hosts nor the system resolver.
//
// TestOpenTofu is the OpenTofu acceptance (SRS 11.1, IF-001): each module
// in tofu/ is applied, planned with no changes expected and destroyed with
// the google and google-beta providers at their current and previous
// minor versions, behind an Escape guard (internal/escapeguard) that fails
// a module on any request leaving the machine. It needs tofu on PATH
// (`task tofu`; `task compat` skips it).
package compat
