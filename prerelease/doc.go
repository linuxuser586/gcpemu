// Package prerelease holds the Pre-release host checks (docs/release.md,
// #606): the checks run on macOS and Windows hosts before a release is
// tagged, on the VM runtimes CI cannot run. They drive the real gcloud,
// kubectl and cloud-sql-proxy against the Instance the way a user on that
// host would.
//
// The tests are behind the "prerelease" build tag so that `go test ./...`
// is unaffected. `just prerelease` runs them:
//
//	just prerelease                          # the Required runtimes
//	just prerelease colima,orbstack          # docker contexts to check
//	just prerelease desktop-linux gcpemu:rc  # an image already built
//
// On macOS (and on Linux, where they run only for the runtimes named on
// its command line, so the suite can be worked on there) each runtime
// runs the Reference stack (`just e2e`), two Cloud SQL instances at once over
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
// `just prerelease` builds it there too. Next, the VM relay check: with
// GCPEMU_VM_RELAY cleared, gcpemu doctor on the host must report the
// relay on, not forced (ADR 0056), and the VM relay's own tests,
// which CI runs only on Linux with the relay forced, must all pass
// against the runtime. Then the Registry relay check: an Instance on the
// host with Artifact Registry, to and from which the runtime's own docker
// (podman on Podman machine) pushes and pulls an image at
// $GCPEMU_REGISTRY (ADR 0043). Last, the same push and pull against a
// Containerized Instance's registry, published at the same host port,
// which needs no Registry relay (#835): on Windows the container of the
// first checks, on macOS one of the release's image of its own.
//
// The Instances the checks start on the host run in Test data dirs
// (ADR 0057) under gcpemu-prerelease in the OS temp dir. A check's cleanup
// reclaims its Instance's dir when gcpemu stop fails, and a run starts by
// reclaiming the dirs of ended tests, such as those a killed run left.
//
// TestResolverRoutes is the same check on Linux, without the GKE Service,
// for CI: it runs only with GCPEMU_RESOLVER_TESTS=1.
//
// The run ends with a summary table, printed and written to
// dist/prerelease-<os>-<commit>.md. Each runtime's setup and steps write
// their output, the commands they stream and their own t.Log and t.Fatal
// lines, to dist/prerelease-<os>-<commit>/<runtime>/<step>.log as well as
// the terminal (#1139), and the table's fail cells and the notes under it
// link to those logs. What setup started logs its cleanup to teardown.log.
package prerelease
