package main

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// trivyIgnoreFile holds the Trivy findings the licence and image checks
// let pass, in Trivy's own format, which Trivy applies (--ignorefile).
const trivyIgnoreFile = ".trivyignore.yaml"

var (
	// The block-style YAML trivyIgnoreProblems understands: a section, an
	// entry's first field, a further field, and an item of a list field.
	ignoreSection = regexp.MustCompile(`^([a-z]+):$`)
	ignoreEntry   = regexp.MustCompile(`^  - ([a-z_]+):(.*)$`)
	ignoreField   = regexp.MustCompile(`^    ([a-z_]+):(.*)$`)
	ignoreItem    = regexp.MustCompile(`^      - (.+)$`)
)

// trivyIgnoreProblems checks .trivyignore.yaml against ADR 0090: every
// entry links an issue in its statement and has an expired_at date that
// has not begun. Trivy itself stops honouring an expired entry, but would
// keep one with no expiry forever. devtool uses only the standard library
// (ADR 0061), so this reads only block-style YAML, the subset the file
// is written in, and reports anything else rather than guess.
func trivyIgnoreProblems(b []byte, now time.Time) []string {
	entries, out := parseTrivyIgnore(b)
	for _, e := range entries {
		out = append(out, e.problems(now, ignoreFields)...)
	}
	return out
}

// parseTrivyIgnore reads .trivyignore.yaml's entries, and the lines it
// could not read.
func parseTrivyIgnore(b []byte) ([]exception, []string) {
	var out []string
	var entries []exception
	var cur *exception
	section := ""
	listField := false
	for i, line := range strings.Split(string(b), "\n") {
		n := i + 1
		if t := strings.TrimSpace(line); t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		var key, val string
		switch {
		case ignoreSection.MatchString(line):
			cur, listField = nil, false
			section = ignoreSection.FindStringSubmatch(line)[1]
			continue
		case ignoreItem.MatchString(line) && listField:
			continue
		case ignoreEntry.MatchString(line):
			m := ignoreEntry.FindStringSubmatch(line)
			entries = append(entries, exception{section: section})
			cur = &entries[len(entries)-1]
			key, val = m[1], m[2]
		case ignoreField.MatchString(line) && cur != nil:
			m := ignoreField.FindStringSubmatch(line)
			key, val = m[1], m[2]
		default:
			out = append(out, fmt.Sprintf("%s line %d: unsupported YAML; write block-style entries as the file's others are", trivyIgnoreFile, n))
			continue
		}
		val = strings.Trim(strings.TrimSpace(val), `"'`)
		listField = false
		switch key {
		case "id":
			cur.ID = val
		case "statement":
			cur.Issue = issueURL.FindString(val)
		case "expired_at":
			cur.Expires = val
		case "paths", "purls":
			listField = val == ""
		default:
			out = append(out, fmt.Sprintf("%s line %d: unknown field %q", trivyIgnoreFile, n, key))
		}
	}
	return entries, out
}

// splitSection splits .trivyignore.yaml's entries into those of section
// and the rest.
func splitSection(entries []exception, section string) (in, rest []exception) {
	for _, e := range entries {
		if e.section == section {
			in = append(in, e)
		} else {
			rest = append(rest, e)
		}
	}
	return in, rest
}

// trivySuppressed is a finding an ignore file let pass, one of a result's
// ExperimentalModifiedFindings with --show-suppressed (experimental in
// Trivy 0.76). ADR 0090 relies on it to judge .trivyignore.yaml's entries
// stale: renamed, every entry reads as stale, and the check fails.
type trivySuppressed struct {
	// Source is the --ignorefile as given.
	Source  string `json:"Source"`
	Finding struct {
		// Name is a licence finding's licence.
		Name string `json:"Name"`
		// VulnerabilityID is a vulnerability finding's ID.
		VulnerabilityID string `json:"VulnerabilityID"`
	} `json:"Finding"`
}
