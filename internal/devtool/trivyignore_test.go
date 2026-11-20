package main

import (
	"slices"
	"strings"
	"testing"
)

func TestTrivyIgnore(t *testing.T) {
	const valid = `# Exceptions to the Trivy checks (ADR 0090).
vulnerabilities:
  - id: CVE-2026-97054
    statement: https://github.com/linuxuser586/gcpemu/issues/1 net/http, fixed in Go 1.27.4
    expired_at: 2026-12-31

licenses:
  - id: BlueOak-1.0.0
    paths:
      - setup-gcpemu/package-lock.json
    statement: "https://github.com/linuxuser586/gcpemu/issues/2"
    expired_at: 2026-11-20
`
	for _, tc := range []struct {
		name, file, want string
	}{
		{name: "valid", file: valid},
		{name: "empty", file: "# nothing ignored\n"},
		{name: "expired",
			file: strings.Replace(valid, "2026-11-20", "2026-11-19", 1),
			want: "BlueOak-1.0.0: expired on 2026-11-19"},
		{name: "no expiry",
			file: strings.Replace(valid, "    expired_at: 2026-12-31\n", "", 1),
			want: "CVE-2026-97054: expires must be a date"},
		{name: "no issue",
			file: strings.Replace(valid, "https://github.com/linuxuser586/gcpemu/issues/1 ", "", 1),
			want: "CVE-2026-97054: issue must link a gcpemu GitHub issue"},
		{name: "flow style",
			file: valid + "secrets: [{id: aws-access-key-id}]\n",
			want: "line 13: unsupported YAML"},
		{name: "unknown field",
			file: strings.Replace(valid, "expired_at: 2026-12-31", "expires: 2026-12-31", 1),
			want: `line 5: unknown field "expires"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := trivyIgnoreProblems([]byte(tc.file), today)
			if tc.want == "" && len(got) > 0 || tc.want != "" && !slices.ContainsFunc(got, func(p string) bool { return strings.Contains(p, tc.want) }) {
				t.Errorf("problems = %q, want %q", got, tc.want)
			}
		})
	}
}
