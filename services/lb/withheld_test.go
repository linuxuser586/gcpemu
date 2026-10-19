package lb

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/linuxuser586/gcpemu/internal/emu"
)

// A withheld GCPEMU_LB_* variable is logged once when it appears, naming
// the rules and their qualified variables, and again only after it has
// cleared (ADR 0013).
func TestWarnWithheldOnce(t *testing.T) {
	var logs bytes.Buffer
	eps := emu.NewEndpoints()
	d := &dataplane{s: &Service{env: &emu.Env{Endpoints: eps, Log: slog.New(slog.NewTextHandler(&logs, nil))}}}
	warnings := func() int { return strings.Count(logs.String(), "level=WARN") }

	eps.Set("lb:p/global/web", "127.0.0.2:8080")
	d.warnWithheld()
	if n := warnings(); n != 0 {
		t.Fatalf("%d warnings for a unique name:\n%s", n, logs.String())
	}
	eps.Set("lb:p/us-central1/web", "127.0.0.3:8080")
	d.warnWithheld()
	d.warnWithheld()
	if n := warnings(); n != 1 {
		t.Fatalf("%d warnings for one collision, want 1:\n%s", n, logs.String())
	}
	for _, want := range []string{"variable=GCPEMU_LB_WEB", "projects/p/global/forwardingRules/web", "projects/p/regions/us-central1/forwardingRules/web",
		"GCPEMU_LB_P_GLOBAL_WEB", "GCPEMU_LB_P_US_CENTRAL1_WEB"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("warning lacks %s:\n%s", want, logs.String())
		}
	}
	eps.Delete("lb:p/us-central1/web")
	d.warnWithheld()
	eps.Set("lb:p/us-central1/web", "127.0.0.3:8080")
	d.warnWithheld()
	if n := warnings(); n != 2 {
		t.Errorf("%d warnings after the collision cleared and came back, want 2:\n%s", n, logs.String())
	}
}
