// Package prerelease holds the Pre-release host checks (docs/release.md,
// #606): the checks run on macOS and Windows hosts before a release is
// tagged, on the VM runtimes CI cannot run. They drive the real gcloud,
// kubectl and cloud-sql-proxy against the Instance the way a user on that
// host would.
//
// The tests are behind the "prerelease" build tag so that `go test ./...`
// is unaffected. `task prerelease` runs them:
//
//	task prerelease                          # the Required runtimes
//	task prerelease RUNTIMES=colima,orbstack # docker contexts to check
//	task prerelease IMAGE=gcpemu:rc          # Windows: an image already built
//
// On macOS (and on Linux, where they run only for runtimes named in
// RUNTIMES, so the suite can be worked on there) each runtime runs the
// Reference stack (`task e2e`), two Cloud SQL instances at once over
// loopback aliases, a GKE LoadBalancer Service from the host and the VLAN
// (8021q) check, whose result goes into README.md's runtime table. On
// Windows each runtime runs the release's image as a Containerized
// Instance and reaches Cloud SQL, a GKE LoadBalancer Service and a
// forwarding rule from the host. On both, each runtime then runs the
// Host attachment (ADR 0014) tests of the compute and GKE services, with
// gcpemu on the host, and the Resolver routes check: an Ephemeral Instance
// of its own, prerelease-dns, with a Cloud DNS zone and a GKE Service,
// whose `gcpemu dns install` routes resolve both through the host's
// resolver until `gcpemu dns uninstall` takes them away. On Windows its
// Instance is on the host, gcpemu.exe, with dns on port 53 (ADR 0026), so
// `task prerelease` builds it there too.
//
// TestResolverRoutes is the same check on Linux, without the GKE Service,
// for CI: it runs only with GCPEMU_RESOLVER_TESTS=1.
//
// The run ends with a summary table, printed and written to
// dist/prerelease-<os>-<commit>.md.
package prerelease
