package main

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// govulncheckStream is govulncheck -format json output, trimmed: an OSV
// entry, then a module-level, a package-level and a symbol-level finding
// for GO-2026-6859, and only a module-level one for GO-2026-5932, which the
// code never calls.
const govulncheckStream = `{
  "config": {"protocol_version": "v1.0.0", "scanner_name": "govulncheck", "scanner_version": "v1.9.0"}
}
{
  "osv": {"id": "GO-2026-6859", "summary": "HTTP/1 server crash due to MaxBytesReader header race in net/http"}
}
{
  "osv": {"id": "GO-2026-5932", "summary": "Panic in golang.org/x/crypto/ssh"}
}
{
  "finding": {"osv": "GO-2026-5932", "fixed_version": "v0.58.1", "trace": [{"module": "golang.org/x/crypto", "version": "v0.58.0"}]}
}
{
  "finding": {"osv": "GO-2026-6859", "fixed_version": "v1.27.4", "trace": [{"module": "stdlib", "version": "v1.27.3"}]}
}
{
  "finding": {"osv": "GO-2026-6859", "fixed_version": "v1.27.4", "trace": [{"module": "stdlib", "version": "v1.27.3", "package": "net/http"}]}
}
{
  "finding": {"osv": "GO-2026-6859", "fixed_version": "v1.27.4", "trace": [
    {"module": "stdlib", "version": "v1.27.3", "package": "net/http", "function": "ServeHTTP"},
    {"module": "github.com/linuxuser586/gcpemu", "package": "github.com/linuxuser586/gcpemu/internal/admin", "function": "Run"}
  ]}
}
`

// now is mid-day: an entry holds until its date begins, in UTC, as Trivy
// applies expired_at.
var now = time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)

func TestVulncheckReportsCalledFindingsOnly(t *testing.T) {
	r, err := vulnReport(strings.NewReader(govulncheckStream), []byte(`[]`), now)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Problems) != 0 {
		t.Errorf("problems: %v", r.Problems)
	}
	var ids []string
	for _, f := range r.Unallowed {
		ids = append(ids, f.ID)
	}
	if !slices.Equal(ids, []string{"GO-2026-6859"}) {
		t.Fatalf("unallowed = %v, want [GO-2026-6859]", ids)
	}
	f := r.Unallowed[0]
	if f.Summary != "HTTP/1 server crash due to MaxBytesReader header race in net/http" || f.Fixed != "v1.27.4" || f.Module != "stdlib" {
		t.Errorf("finding = %+v", f)
	}
}

func TestVulncheckAllowlist(t *testing.T) {
	const issue = "https://github.com/linuxuser586/gcpemu/issues/1"
	for _, tc := range []struct {
		name          string
		allow         string
		wantUnallowed bool
		wantProblem   string
	}{
		{name: "entry covers the finding",
			allow: `[{"id": "GO-2026-6859", "issue": "` + issue + `", "expires": "2026-12-31"}]`},
		{name: "entry expires tomorrow, still valid",
			allow: `[{"id": "GO-2026-6859", "issue": "` + issue + `", "expires": "2026-11-21"}]`},
		{name: "entry expires today",
			allow:         `[{"id": "GO-2026-6859", "issue": "` + issue + `", "expires": "2026-11-20"}]`,
			wantUnallowed: true, wantProblem: "GO-2026-6859: expired on 2026-11-20"},
		{name: "no expiry",
			allow:         `[{"id": "GO-2026-6859", "issue": "` + issue + `"}]`,
			wantUnallowed: true, wantProblem: "GO-2026-6859: expires must be a date (YYYY-MM-DD)"},
		{name: "no issue",
			allow:         `[{"id": "GO-2026-6859", "expires": "2026-12-31"}]`,
			wantUnallowed: true, wantProblem: "GO-2026-6859: issue must link a gcpemu GitHub issue"},
		{name: "issue elsewhere",
			allow:         `[{"id": "GO-2026-6859", "issue": "see chat", "expires": "2026-12-31"}]`,
			wantUnallowed: true, wantProblem: "GO-2026-6859: issue must link a gcpemu GitHub issue"},
		{name: "unknown field",
			allow:         `[{"id": "GO-2026-6859", "issue": "` + issue + `", "expires": "2026-12-31", "expiry": "2027-01-01"}]`,
			wantUnallowed: true, wantProblem: `unknown field "expiry"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, err := vulnReport(strings.NewReader(govulncheckStream), []byte(tc.allow), now)
			if err != nil {
				t.Fatal(err)
			}
			if got := len(r.Unallowed) > 0; got != tc.wantUnallowed {
				t.Errorf("unallowed = %v, want any: %v", r.Unallowed, tc.wantUnallowed)
			}
			if tc.wantProblem == "" && len(r.Problems) > 0 || tc.wantProblem != "" && !slices.ContainsFunc(r.Problems, func(p string) bool { return strings.Contains(p, tc.wantProblem) }) {
				t.Errorf("problems = %q, want %q", r.Problems, tc.wantProblem)
			}
		})
	}
}
