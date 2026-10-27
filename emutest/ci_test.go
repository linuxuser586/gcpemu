package emutest

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// TestNoCIDefaults is ADR 0039: the CI defaults (FR-CI-003) are the CLI's,
// so under CI=true an emutest Instance still logs text and serves the Web
// console, as it does on a laptop.
func TestNoCIDefaults(t *testing.T) {
	t.Setenv("CI", "true")
	inst := Start(t, []string{"dns"})
	if f := inst.in.Config.LogFormat; f != "text" {
		t.Errorf("LogFormat = %q, want text", f)
	}
	resp, err := http.Get(inst.GatewayURL() + "/console/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(b), "<html") {
		t.Errorf("/console/ = %d %.80q, want the console page", resp.StatusCode, b)
	}
}
