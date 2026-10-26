//go:build prerelease

package prerelease

import (
	"bufio"
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
)

// hostAttachmentTests are the Host attachment's (ADR 0014) tests: inward,
// a VPC workload reaching a host listener at its subnetwork's Host
// address, in the compute service, and outward, host clients reaching a
// cluster's Services at Loopback aliases, in the GKE service.
var hostAttachmentTests = []string{"TestHostAttachmentOffers", "TestHostAttachmentSurvivesRestart", wildcardTest,
	"TestHostServices"}

// wildcardTest skips where the VM relay holds the Host address, so on
// every VM runtime, saying vmRelaySkip.
const (
	wildcardTest = "TestOfferHeldOnWildcard"
	vmRelaySkip  = "the VM relay holds the Host address"
)

// hostAttachment is step 5 of both suites: it runs the Host attachment
// tests against the runtime, with gcpemu on the host. Any failure, or a
// skip other than wildcardTest's for the VM relay, fails the check: the
// outward test skips without Loopback aliases, which macOS has by now and
// Linux and Windows always have.
func hostAttachment(t *testing.T, ctx context.Context) {
	requireTools(t, "go")
	requireAliases(t, ctx)
	runs, out := goTests(ctx, hostAttachmentTests, "./services/compute", "./services/gke")
	for _, name := range hostAttachmentTests {
		switch r := runs[name]; r.ended {
		case "pass":
		case "skip":
			if name == wildcardTest && strings.Contains(r.log, vmRelaySkip) {
				t.Logf("%s skipped:\n%s", name, r.log)
				continue
			}
			t.Errorf("%s skipped, so the Host attachment check didn't run:\n%s", name, r.log)
		case "fail":
			t.Errorf("%s failed:\n%s", name, r.log)
		default:
			t.Errorf("%s didn't run:\n%s", name, out)
		}
	}
}

// testRun is how a top-level test of a go test run ended ("pass", "fail",
// "skip", or "" when it didn't run), and its output.
type testRun struct{ ended, log string }

// goTests runs the tests named in names, in pkgs, from the repository's
// root, against the runtime the environment selects, and returns how each
// ended and go test's whole output.
func goTests(ctx context.Context, names []string, pkgs ...string) (map[string]testRun, string) {
	out := goTest(ctx, append([]string{"-run", "^(" + strings.Join(names, "|") + ")$"}, pkgs...)...)
	return testRuns(out), out
}

// goTest runs go test -json with args from the repository's root and
// returns its output, whatever it exits with.
func goTest(ctx context.Context, args ...string) string {
	cmd := exec.CommandContext(ctx, "go", append([]string{"test", "-json", "-count=1", "-timeout", "25m"}, args...)...)
	cmd.Dir = repo
	out, _ := cmd.Output()
	return string(out)
}

// testRuns reads go test -json's output: how each top-level test ended,
// and its output and its subtests'. Under "" is how the package's run
// ended, with its own output.
func testRuns(out string) map[string]testRun {
	ended := map[string]string{}
	logs := map[string]*strings.Builder{}
	sc := bufio.NewScanner(strings.NewReader(out))
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	for sc.Scan() {
		var ev struct{ Action, Test, Output string }
		if json.Unmarshal(sc.Bytes(), &ev) != nil {
			continue
		}
		top, _, sub := strings.Cut(ev.Test, "/")
		if logs[top] == nil {
			logs[top] = &strings.Builder{}
		}
		switch ev.Action {
		case "output":
			logs[top].WriteString(ev.Output)
		case "pass", "fail", "skip":
			if !sub {
				ended[top] = ev.Action
			}
		}
	}
	runs := map[string]testRun{}
	for name, log := range logs {
		runs[name] = testRun{ended[name], log.String()}
	}
	return runs
}
