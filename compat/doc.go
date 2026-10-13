// Package compat is the client compatibility suite (SRS 7.1, IF-003 to
// IF-006 and IF-009): the real gcloud, kubectl with gke-gcloud-auth-plugin,
// Helm, Docker, crane, ko, psql, the Cloud SQL Auth Proxy v2 and dig run
// against a detached gcpemu instance with only endpoint configuration.
//
//	go test -tags compat ./compat -v -timeout 30m
//
// gcloud (with the gke-gcloud-auth-plugin component), kubectl, dig and
// Docker come from PATH; their tests skip when one is missing. Helm, crane,
// ko and the Auth Proxy are downloaded at pinned, checksum-verified
// versions into $GCPEMU_COMPAT_CACHE (default <user cache dir>/gcpemu-compat),
// and psql runs from the PostgreSQL image. Cloud SQL and GKE need a
// container runtime; some steps (crane copy from Docker Hub, ko's base
// image) need the internet. gcloud runs behind an Escape guard
// (internal/escapeguard), so a gcloud request that no endpoint override
// covers fails the step instead of reaching Google.
//
// The universe subtests (#215) probe the Python and Node client libraries
// with an emulator-issued key, and Python google-auth with the metadata
// server (#244), from the scripts in sdk/. They install pinned library
// versions into the same cache with python3 -m venv and npm, and skip when
// either is missing or the install fails. The Python probes run behind the
// Escape guard. The go-mtls subtest (#243) builds the Go probe in
// sdk/universego and runs it behind the Escape guard, which names the host
// it dialed.
//
// TestOpenTofu is the OpenTofu acceptance (SRS 11.1, IF-001): each module
// in tofu/ is applied, planned with no changes expected and destroyed with
// the google and google-beta providers at their current and previous
// minor versions, behind an Escape guard (internal/escapeguard) that fails
// a module on any request leaving the machine. It needs tofu on PATH
// (`make tofu`; `make compat` skips it).
package compat
