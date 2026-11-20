package main

import (
	"slices"
	"testing"
	"time"
)

func TestExpiringExceptions(t *testing.T) {
	files := map[string][]byte{
		govulncheckAllowFile: []byte(`[
  {"id": "GO-2026-1", "issue": "https://github.com/linuxuser586/gcpemu/issues/1", "expires": "2026-12-04"},
  {"id": "GO-2026-2", "issue": "https://github.com/linuxuser586/gcpemu/issues/2", "expires": "2026-12-05"}
]`),
		licenceExceptionsFile: []byte(`[
  {"id": "buffers@0.1.1", "issue": "https://github.com/linuxuser586/gcpemu/issues/3", "expires": "2026-11-21"}
]`),
		trivyIgnoreFile: []byte(`vulnerabilities:
  - id: CVE-2026-1
    statement: https://github.com/linuxuser586/gcpemu/issues/4
    expired_at: 2026-11-30
licenses:
  - id: GPL-3.0-only
    statement: https://github.com/linuxuser586/gcpemu/issues/5
    expired_at: 2027-02-28
`),
	}
	got, err := expiringExceptions(files, now, 14*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	// now is 2026-11-20 12:00 UTC: within 14 days is before 2026-12-04
	// 12:00, so an entry dated 2026-12-04 (it stops holding at 00:00)
	// is listed and one dated 2026-12-05 is not. Soonest first.
	want := []string{
		"`buffers@0.1.1` (.licence-exceptions.json) stops holding on 2026-11-21: https://github.com/linuxuser586/gcpemu/issues/3",
		"`CVE-2026-1` (.trivyignore.yaml) stops holding on 2026-11-30: https://github.com/linuxuser586/gcpemu/issues/4",
		"`GO-2026-1` (.govulncheck-allow.json) stops holding on 2026-12-04: https://github.com/linuxuser586/gcpemu/issues/1",
	}
	if !slices.Equal(got, want) {
		t.Errorf("expiring =\n%q\nwant\n%q", got, want)
	}
}
