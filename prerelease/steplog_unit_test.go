//go:build prerelease

package prerelease

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
	"time"
)

// The steps' logs (#1139) run on any host: TestFailedStepLog runs a fake
// run, TestFakeRun, in a copy of the test binary.

func TestReportLinksFailedSteps(t *testing.T) {
	s := &runSummary{
		commit: "abc1234",
		results: []result{
			{"colima", "setup", "pass"},
			{"colima", "1-reference-stack", "fail"},
			{"colima", "2-two-sql-instances", "pass"},
			{"orbstack", "setup", "skip"},
		},
		versions: map[string]string{"colima": "0.9.1"},
		notes:    map[string]string{"orbstack": "runtime orbstack does not answer"},
	}
	md, _ := s.markdown(time.Now())
	dir := "prerelease-" + goruntime.GOOS + "-abc1234/"
	for _, want := range []string{
		"| [fail](" + dir + "colima/1-reference-stack.log) |",
		"- orbstack: runtime orbstack does not answer ([log](" + dir + "orbstack/setup.log))",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("the report does not have %q:\n%s", want, md)
		}
	}
	if strings.Contains(md, "2-two-sql-instances.log") {
		t.Errorf("the report links a step that passed:\n%s", md)
	}
}

// TestStepLogsSwitch writes through the tee as a run does, with NULs of
// the output's own and marks split across writes.
func TestStepLogsSwitch(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	var term bytes.Buffer
	l := newStepLogs(w, &term)
	copied := make(chan struct{})
	go func() { l.copy(r); close(copied) }()
	dir := t.TempDir()
	a, b := filepath.Join(dir, "rt", "setup.log"), filepath.Join(dir, "rt", "1-step.log")

	w.WriteString("before\n")
	l.to(a)
	w.WriteString("in a \x00 with a NUL\x00")
	w.WriteString(logMark[:5])
	w.WriteString(logMark[5:] + b)
	w.WriteString("\x00in b\n")
	<-l.acks // as to does
	l.end()
	w.WriteString("after\n")
	w.Close()
	<-copied

	if want := "before\nin a \x00 with a NUL\x00in b\nafter\n"; term.String() != want {
		t.Errorf("the terminal got %q, want %q", term.String(), want)
	}
	for path, want := range map[string]string{a: "in a \x00 with a NUL\x00", b: "in b\n"} {
		if got, err := os.ReadFile(path); err != nil || string(got) != want {
			t.Errorf("%s holds %q, %v; want %q", path, got, err, want)
		}
	}
}

// TestFailedStepLog runs TestFakeRun, whose step fails, and finds the
// failure the terminal showed in the step's log, which the report links.
func TestFailedStepLog(t *testing.T) {
	dist := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestFakeRun$", "-test.v")
	cmd.Env = append(os.Environ(), fakeRunEnv+"="+dist)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("the fake run passed:\n%s", out)
	}
	// stream's t.Fatalf.
	const failure = " say: exit status 1"
	if !bytes.Contains(out, []byte(failure)) {
		t.Fatalf("the terminal does not show %q:\n%s", failure, out)
	}
	reports, _ := filepath.Glob(filepath.Join(dist, "prerelease-*.md"))
	if len(reports) != 1 {
		t.Fatalf("reports in %s: %v; the run printed:\n%s", dist, reports, out)
	}
	md, err := os.ReadFile(reports[0])
	if err != nil {
		t.Fatal(err)
	}
	name := strings.TrimSuffix(filepath.Base(reports[0]), ".md")
	link := name + "/fake/1-fails.log"
	if !bytes.Contains(md, []byte("[fail]("+link+")")) {
		t.Errorf("the report does not link %s:\n%s", link, md)
	}
	stepLog, err := os.ReadFile(filepath.Join(dist, filepath.FromSlash(link)))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"fake gcpemu: say fails", failure} {
		if !bytes.Contains(stepLog, []byte(want)) {
			t.Errorf("the step's log does not have %q:\n%s", want, stepLog)
		}
	}
	setupLog, err := os.ReadFile(filepath.Join(dist, name, "fake", "setup.log"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(setupLog, []byte("setting up")) || bytes.Contains(setupLog, []byte(failure)) {
		t.Errorf("the setup's log holds:\n%s", setupLog)
	}
	teardownLog, err := os.ReadFile(filepath.Join(dist, name, "fake", "teardown.log"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(teardownLog, []byte("tearing down")) {
		t.Errorf("the teardown's log holds:\n%s", teardownLog)
	}
}

// TestFakeRun is the run of TestFailedStepLog: a runtime whose setup
// logs and starts something its cleanup logs, and whose step streams a command that fails with a word on its
// stderr.
func TestFakeRun(t *testing.T) {
	if os.Getenv(fakeRunEnv) == "" {
		t.Skip("TestFailedStepLog runs it")
	}
	t.Run("fake", func(t *testing.T) {
		done := setup(t, "fake")
		t.Log("setting up")
		t.Cleanup(func() { t.Log("tearing down") })
		done()
		step(t, "fake", "1-fails", time.Minute, func(t *testing.T, ctx context.Context) {
			cmd := exec.CommandContext(ctx, os.Args[0], "say")
			cmd.Env = append(os.Environ(), fakeGcpemuEnv+"=1")
			stream(t, cmd)
		})
	})
}
