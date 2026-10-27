// Package conformance is the Conformance suite (SRS 11.1, ADR 0023): tests
// that use only public GCP APIs, through the official Go client libraries,
// so the same test body runs against an in-process Instance or a real
// project. GCP's responses are kept as Recordings, and the emulator's are
// compared with them.
//
// The tests are behind the "conformance" build tag, so `go test ./...` is
// unaffected. They run in one of two modes:
//
//	task conformance         # = go test -tags conformance ./conformance -v
//	task conformance:record  # GCPEMU_CONFORMANCE_RECORD=1, against GCPEMU_CONFORMANCE_PROJECT
//
// Compare mode, the default, runs each test against an Instance that
// emutest starts (not Deterministic), behind the Escape guard: the clients'
// transport and gRPC dialer reach loopback only, so a request that would
// leave the machine fails the test, naming the host. No credentials are
// needed. Each response is compared with the test's Recording; any
// difference is a Mismatch and fails the test with the field's path, e.g.
// exchanges[2].response.body.labels.color. A test without a Recording
// skips and says to run `task conformance:record`.
//
// Record mode (GCPEMU_CONFORMANCE_RECORD=1) runs the Products' tests
// against the project GCPEMU_CONFORMANCE_PROJECT names, with Application
// Default Credentials and without the Escape guard, and writes their
// Recordings. TestSweep runs first and deletes what earlier runs left
// there more than a day ago. Every resource name carries the run's suffix
// (gcpemu-<suffix>-…, the start time in base 36 and four random
// characters), every resource whose API takes labels is labelled
// gcpemu-conformance=<suffix>, and each test deletes what it creates.
// A step that GCP's eventual consistency can fail is retried against GCP
// only, keeping the last attempt's exchanges; against the emulator it is
// tried once, so a Mismatch cannot hide behind a retry.
//
// A Recording is testdata/<product>/<TestName>.json: the test's exchanges
// in order, each with its request's method, path and body (kept for
// context and readable Drift diffs, never compared) and the normalised
// response's HTTP status or gRPC code and body. Exchanges are captured at
// the wire, by an http.RoundTripper for REST and gRPC interceptors whose
// messages are stored in protojson, so a field the client library does
// not know is still compared. Polls of a long-running Operation are left
// out, since their number depends on timing. Files are written with keys
// in order and a trailing newline, so a Recording that did not change
// produces no diff.
//
// Both targets' responses are normalised alike before comparing and before
// writing. Generated values (id, generation, messageId, ackId, …, an
// Operation's ID, and the keys a test names Generated, each with a written
// reason) become placeholders that stay consistent within a test, <id:1>,
// <generation:2>, so a create and a
// later get returning the same ID are still checked; a value made from the
// run's own names stays as is. Timestamps become <timestamp> but must
// parse as RFC 3339, and etags <etag:n> but must keep their shape. The
// project ID, project number, suffix and the scheme and host of URLs
// become <project>, <project-number>, <suffix> and <host>. A test may also
// Ignore a field, naming it with a written reason; mark a repeated field
// Unordered, naming its sort key (default name); and keep only its own
// elements of a list with RunOnly. Every other order is compared. An error
// is compared by its HTTP status or gRPC code, its reasons and its field
// violations, not by its message unless the test asks with CompareMessages.
//
// TestSelfRecording records every test against the emulator, standing in
// for GCP, twice (the files must be byte-identical) and then compares with
// what it recorded. TestChangedRecordingNamesTheField changes one field
// of a Recording and checks the Mismatch names it.
package conformance
