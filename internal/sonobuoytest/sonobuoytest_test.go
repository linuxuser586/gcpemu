package sonobuoytest

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// report is `sonobuoy results --plugin e2e` output, as Sonobuoy 0.57
// prints it.
func report(status string, passed, failed int) string {
	return fmt.Sprintf("Plugin: e2e\nStatus: %s\nTotal: %d\nPassed: %d\nFailed: %d\nSkipped: 7000\n", status, 7000+passed+failed, passed, failed)
}

// fake is a sonobuoy that records its calls: retrieve writes a tarball
// holding tarball, and results answers with report.
type fake struct {
	calls       [][]string
	timeouts    []time.Duration
	tarball     string
	report      string
	runErr      error
	retrieveErr error
}

// sonobuoy is Quick.Sonobuoy.
func (f *fake) sonobuoy(timeout time.Duration, args ...string) (string, error) {
	f.calls = append(f.calls, args)
	f.timeouts = append(f.timeouts, timeout)
	switch args[0] {
	case "run":
		return "run output", f.runErr
	case "retrieve":
		if f.retrieveErr != nil {
			return "retrieve output", f.retrieveErr
		}
		name := "results.tar.gz"
		if i := slices.Index(args, "--filename"); i >= 0 {
			name = args[i+1]
		}
		return "", os.WriteFile(filepath.Join(args[1], name), []byte(f.tarball), 0o644)
	case "results":
		if slices.Contains(args, "detailed") {
			return `{"name":"a","status":"failed"}` + "\n" + `{"name":"b","status":"skipped"}`, nil
		}
		return f.report, nil
	case "status":
		return "e2e running", nil
	}
	return "", fmt.Errorf("unexpected sonobuoy %v", args)
}

// call returns the arguments of the first sonobuoy name call.
func (f *fake) call(name string) []string {
	for _, c := range f.calls {
		if c[0] == name {
			return c
		}
	}
	return nil
}

// quick is a Quick running f, keeping a failed run's tarball in keep, and
// its log.
func quick(f *fake, keep string) (Quick, *strings.Builder) {
	var log strings.Builder
	return Quick{
		Sonobuoy: f.sonobuoy,
		Keep:     keep,
		Logf:     func(format string, args ...any) { fmt.Fprintf(&log, format+"\n", args...) },
	}, &log
}

func TestRunPasses(t *testing.T) {
	f := &fake{tarball: "tgz", report: report("passed", 1, 0)}
	q, _ := quick(f, "")
	if err := q.Run(t.TempDir(), "1.36.5-gke.100"); err != nil {
		t.Fatalf("Run = %v, want nil", err)
	}
	want := []string{"run", "--mode", "quick", "--plugin", "e2e", "--kubernetes-version", "v1.36.5", "--wait=20"}
	if got := f.call("run"); !slices.Equal(got, want) {
		t.Errorf("sonobuoy %v, want %v", got, want)
	}
	// Sonobuoy reports its own timeout, with the plugins' status, only if
	// the process outlives --wait.
	if f.timeouts[0] <= 20*time.Minute {
		t.Errorf("sonobuoy run timeout %v, want longer than --wait=20", f.timeouts[0])
	}
}

func TestRunFailedKeepsTarball(t *testing.T) {
	f := &fake{tarball: "tgz", report: report("failed", 0, 1)}
	keep := filepath.Join(t.TempDir(), "results", "kc-1-36.tar.gz")
	q, log := quick(f, keep)
	if err := q.Run(t.TempDir(), "1.36.5-gke.100"); err == nil {
		t.Fatal("Run = nil, want an error")
	}
	if b, err := os.ReadFile(keep); err != nil || string(b) != "tgz" {
		t.Errorf("kept tarball %q, %v; want the retrieved one", b, err)
	}
	if !strings.Contains(log.String(), `"name":"a"`) || strings.Contains(log.String(), `"name":"b"`) {
		t.Errorf("log %q: want the failed test and not the skipped one", log)
	}
}

func TestJudgeReportPassed(t *testing.T) {
	if err := judgeReport(report("passed", 1, 0)); err != nil {
		t.Errorf("judgeReport(passed, 1 passed) = %v, want nil", err)
	}
}

func TestJudgeReportRefuses(t *testing.T) {
	for name, r := range map[string]string{
		"failed":       report("failed", 0, 1),
		"unknown":      report("unknown", 0, 0),
		"timeout":      report("timeout", 0, 1), // the plugin outran its timeout
		"zero passed":  report("passed", 0, 0),
		"not finished": "", // no results for the plugin in the tarball
		"no counts":    "Plugin: e2e\nStatus: passed\n",
	} {
		if err := judgeReport(r); err == nil {
			t.Errorf("judgeReport(%s) = nil, want an error", name)
		}
	}
}

func TestRunPassedRemovesStaleTarball(t *testing.T) {
	keep := filepath.Join(t.TempDir(), "kc-1-36.tar.gz")
	if err := os.WriteFile(keep, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	q, _ := quick(&fake{tarball: "tgz", report: report("passed", 1, 0)}, keep)
	if err := q.Run(t.TempDir(), "1.36.5-gke.100"); err != nil {
		t.Fatalf("Run = %v, want nil", err)
	}
	if _, err := os.Stat(keep); !os.IsNotExist(err) {
		t.Errorf("stat %s: %v; want the earlier run's tarball gone", keep, err)
	}
}

func TestRunFailedRunStillRetrieves(t *testing.T) {
	f := &fake{tarball: "tgz", report: report("passed", 1, 0), runErr: fmt.Errorf("exit status 1")}
	q, _ := quick(f, "")
	q.Diagnose = func() string { return "pod logs" }
	err := q.Run(t.TempDir(), "1.36.5-gke.100")
	if err == nil || !strings.Contains(err.Error(), "run output") || !strings.Contains(err.Error(), "pod logs") {
		t.Errorf("Run = %v, want the run's output and the diagnosis", err)
	}
	if f.call("retrieve") == nil {
		t.Error("no sonobuoy retrieve after a failed run")
	}
}

func TestRunRetrieveFailedShowsStatus(t *testing.T) {
	f := &fake{retrieveErr: fmt.Errorf("exit status 1")}
	q, _ := quick(f, "")
	q.Diagnose = func() string { return "pod logs" }
	err := q.Run(t.TempDir(), "1.36.5-gke.100")
	if err == nil || !strings.Contains(err.Error(), "e2e running") || !strings.Contains(err.Error(), "pod logs") {
		t.Errorf("Run = %v, want sonobuoy status and the diagnosis", err)
	}
}
