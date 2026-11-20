package main

import (
	"slices"
	"strings"
	"testing"
)

func TestLicenceAllowed(t *testing.T) {
	for name, want := range map[string]bool{
		"Apache-2.0":                       true,
		"MIT":                              true,
		"mit":                              true,
		"MIT-0":                            true,
		"MIT/X11":                          true,
		"BSD-2-Clause":                     true,
		"BSD-3-Clause":                     true,
		"0BSD":                             true,
		"ISC":                              true,
		"MPL-2.0":                          true,
		"(Apache-2.0 AND BSD-3-Clause)":    true,
		"MIT OR GPL-3.0-only":              true,
		"(MIT OR GPL-2.0) AND Apache-2.0":  true,
		"Apache-2.0 WITH LLVM-exception":   true,
		"GPL-3.0-only":                     false,
		"LGPL-2.1-or-later":                false,
		"BlueOak-1.0.0":                    false,
		"MIT AND GPL-2.0-only":             false,
		"(MIT OR Apache-2.0) AND AGPL-3.0": false,
		"":                                 false,
	} {
		if got := licenceAllowed(name); got != want {
			t.Errorf("licenceAllowed(%q) = %v, want %v", name, got, want)
		}
	}
}

// trivyLicences is trivy fs --scanners license --format json output,
// trimmed: go.mod's packages with their licences, then the license-class
// result that lists each licence finding.
const trivyLicences = `{
  "Results": [
    {"Target": "go.mod", "Class": "lang-pkgs", "Type": "gomod", "Packages": [
      {"ID": "github.com/linuxuser586/gcpemu", "Relationship": "root"},
      {"ID": "cel.dev/cel-go@v0.33.2", "Licenses": ["Apache-2.0"]},
      {"ID": "example.com/gpl@v1.0.0", "Licenses": ["GPL-3.0-only"]},
      {"ID": "example.com/nolicence@v1.0.0"}
    ]},
    {"Target": "console/pnpm-lock.yaml", "Class": "lang-pkgs", "Type": "pnpm", "Packages": [
      {"ID": "react@19.3.1"}
    ]},
    {"Target": "go.mod", "Class": "license", "Licenses": [
      {"PkgName": "cel.dev/cel-go", "FilePath": "go.mod", "Name": "Apache-2.0"},
      {"PkgName": "example.com/gpl", "FilePath": "go.mod", "Name": "GPL-3.0-only"}
    ]}
  ]
}`

func TestLicenceReport(t *testing.T) {
	r, err := licenceReport([]byte(trivyLicences), []byte(`[]`), now)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"go.mod: example.com/gpl is GPL-3.0-only"}; !slices.Equal(r.Disallowed, want) {
		t.Errorf("disallowed = %q, want %q", r.Disallowed, want)
	}
	// A package with no licence grants nothing, so it fails like a
	// disallowed one.
	if want := []string{"go.mod: example.com/nolicence@v1.0.0"}; !slices.Equal(r.Unlicensed, want) {
		t.Errorf("unlicensed = %q, want %q", r.Unlicensed, want)
	}
	// No licence was read from the pnpm lockfile, so its node_modules is
	// missing: the check must not pass without having looked.
	if len(r.Unread) != 1 || !strings.HasPrefix(r.Unread[0], "console/pnpm-lock.yaml") {
		t.Errorf("unread = %q, want console/pnpm-lock.yaml", r.Unread)
	}
}

func TestLicenceExceptions(t *testing.T) {
	const issue = "https://github.com/linuxuser586/gcpemu/issues/1"
	for _, tc := range []struct {
		name           string
		exceptions     string
		wantUnlicensed bool
		wantProblem    string
	}{
		{name: "entry covers the package",
			exceptions: `[{"id": "example.com/nolicence@v1.0.0", "issue": "` + issue + `", "expires": "2026-12-31"}]`},
		{name: "another version is not covered",
			exceptions:     `[{"id": "example.com/nolicence@v0.9.0", "issue": "` + issue + `", "expires": "2026-12-31"}]`,
			wantUnlicensed: true},
		{name: "expired entry",
			exceptions:     `[{"id": "example.com/nolicence@v1.0.0", "issue": "` + issue + `", "expires": "2026-11-20"}]`,
			wantUnlicensed: true, wantProblem: "example.com/nolicence@v1.0.0: expired on 2026-11-20"},
		{name: "no issue",
			exceptions:     `[{"id": "example.com/nolicence@v1.0.0", "expires": "2026-12-31"}]`,
			wantUnlicensed: true, wantProblem: "issue must link a gcpemu GitHub issue"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, err := licenceReport([]byte(trivyLicences), []byte(tc.exceptions), now)
			if err != nil {
				t.Fatal(err)
			}
			if got := len(r.Unlicensed) > 0; got != tc.wantUnlicensed {
				t.Errorf("unlicensed = %q, want any: %v", r.Unlicensed, tc.wantUnlicensed)
			}
			if tc.wantProblem == "" && len(r.Problems) > 0 || tc.wantProblem != "" && !slices.ContainsFunc(r.Problems, func(p string) bool { return strings.Contains(p, tc.wantProblem) }) {
				t.Errorf("problems = %q, want %q", r.Problems, tc.wantProblem)
			}
		})
	}
}
