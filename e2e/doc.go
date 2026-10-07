// Package e2e holds the SRS 11.2 reference stack acceptance test: one
// OpenTofu root module (stack/), a Go API (app/) and the test that drives
// them against an in-process emulator.
//
// The test is behind the "e2e" build tag so that `go test ./...` is
// unaffected:
//
//	GCPEMU_NET_TESTS=1 go test -tags e2e ./e2e -run TestReferenceStack -timeout 30m -v
//
// It needs a container runtime, OpenTofu (tofu) on PATH and, for steps 2
// and later, internet access (GCPEMU_NET_TESTS=1) to download Helm, the
// Istio release (for its charts) and the Istio images. See README.md ("Reference stack").
package e2e
