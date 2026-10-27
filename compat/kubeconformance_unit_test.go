//go:build compat

package compat

import (
	"fmt"
	"slices"
	"testing"
)

func TestNewestPatches(t *testing.T) {
	got := newestPatches([]string{
		"1.36.4-gke.100", "1.37.1-gke.100", "1.35.8-gke.100",
		"1.37.0-gke.100", "1.35.9-gke.100", "1.36.5-gke.100", "1.36.10-gke.100",
	})
	want := []string{"1.35.9-gke.100", "1.36.10-gke.100", "1.37.1-gke.100"}
	if !slices.Equal(got, want) {
		t.Errorf("newestPatches = %v, want %v", got, want)
	}
}

// report is `sonobuoy results --plugin e2e` output, as Sonobuoy 0.57
// prints it.
func report(status string, passed, failed int) string {
	return fmt.Sprintf("Plugin: e2e\nStatus: %s\nTotal: %d\nPassed: %d\nFailed: %d\nSkipped: 7000\n", status, 7000+passed+failed, passed, failed)
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
