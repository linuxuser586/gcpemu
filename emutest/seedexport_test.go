package emutest_test

import (
	"strings"
	"testing"

	"github.com/linuxuser586/gcpemu/emutest"
)

// TestSeedExportAcrossServices is FR-CORE-013 and FR-UI-017 over the
// admin API: an Instance's exported seed file, uploaded to it once reset,
// recreates the same resources, so a second export is identical.
func TestSeedExportAcrossServices(t *testing.T) {
	inst := emutest.Start(t, []string{"gcs", "pubsub", "dns", "secrets", "compute", "lb"})
	gw := inst.GatewayURL()
	call(t, "POST", gw+"/_emu/v1/seed", `projects: [export-proj]
gcs:
  buckets:
    - name: export-bucket
      project: export-proj
      labels: {env: dev}
      objects:
        - name: notes.txt
          content: exported
pubsub:
  topics:
    - name: export-topic
      project: export-proj
  subscriptions:
    - name: export-sub
      project: export-proj
      topic: export-topic
      ackDeadlineSeconds: 30
dns:
  zones:
    - name: export-zone
      project: export-proj
      dnsName: export.test.
      records:
        - {name: www, type: A, rrdatas: [192.0.2.1]}
secrets:
  secrets:
    - project: export-proj
      id: api-key
      versions: [{data: s3cret}]
faults:
  - id: export-rule
    service: gcs
    code: UNAVAILABLE
    disabled: true
`)
	first := string(call(t, "GET", gw+"/_emu/v1/seed", ""))
	for _, want := range []string{"export-bucket", "content: exported", "export-sub", "export.test.", "s3cret", "export-rule"} {
		if !strings.Contains(first, want) {
			t.Errorf("export lacks %q:\n%s", want, first)
		}
	}

	call(t, "POST", gw+"/_emu/v1/reset", "")
	call(t, "DELETE", gw+"/_emu/v1/faults", "")
	call(t, "POST", gw+"/_emu/v1/seed", first)
	if second := string(call(t, "GET", gw+"/_emu/v1/seed", "")); second != first {
		t.Errorf("export after re-seeding differs:\n%s\nwant\n%s", second, first)
	}
	if got := string(call(t, "GET", gw+"/storage/v1/b/export-bucket/o/notes.txt?alt=media", "")); got != "exported" {
		t.Errorf("re-seeded object = %q", got)
	}
}
