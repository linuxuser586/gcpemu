package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"time"
)

// exception is the part every entry of the committed exception lists
// shares, the govulncheck allowlist's and .trivyignore.yaml's alike
// (ADR 0090): the finding it suppresses, the issue that tracks fixing it
// and the date it stops holding, from 00:00 UTC, as Trivy applies
// expired_at.
type exception struct {
	ID      string `json:"id"`
	Issue   string `json:"issue"`
	Expires string `json:"expires"`
	// Reason says why the finding can wait; it is not checked.
	Reason string `json:"reason"`
}

// issueURL is a gcpemu issue's link; an exception's must be exactly one.
var issueURL = regexp.MustCompile(`https://github\.com/linuxuser586/gcpemu/issues/[0-9]+`)

// exceptionFields names an exception list's issue and expiry fields, for
// its problems' messages.
type exceptionFields struct{ issue, expires string }

var (
	allowFields  = exceptionFields{issue: "issue", expires: "expires"}
	ignoreFields = exceptionFields{issue: "statement", expires: "expired_at"}
)

// problems lists what makes e invalid at now. An entry that has a problem
// suppresses nothing.
func (e exception) problems(now time.Time, names exceptionFields) []string {
	var out []string
	if e.ID == "" {
		out = append(out, "an entry has no id")
	}
	if e.Issue == "" {
		out = append(out, fmt.Sprintf("%s: %s must link a gcpemu GitHub issue (https://github.com/linuxuser586/gcpemu/issues/N)", e.ID, names.issue))
	}
	if d, err := time.Parse(time.DateOnly, e.Expires); err != nil {
		out = append(out, fmt.Sprintf("%s: %s must be a date (YYYY-MM-DD)", e.ID, names.expires))
	} else if !now.Before(d) {
		out = append(out, fmt.Sprintf("%s: expired on %s; fix the finding, or move the date with the issue's agreement", e.ID, e.Expires))
	}
	return out
}

// parseExceptions reads a JSON array of exceptions from file, returning
// the IDs of the valid ones at now and the problems of the rest. A file
// that does not parse suppresses nothing.
func parseExceptions(b []byte, file string, now time.Time) (valid map[string]bool, problems []string) {
	entries, err := decodeExceptions(b)
	if err != nil {
		return nil, []string{fmt.Sprintf("%s: %v", file, err)}
	}
	valid = map[string]bool{}
	for _, e := range entries {
		p := e.problems(now, allowFields)
		for _, q := range p {
			problems = append(problems, file+": "+q)
		}
		valid[e.ID] = len(p) == 0
	}
	return valid, problems
}

// decodeExceptions decodes a JSON array of exceptions. An issue that is
// not exactly a gcpemu issue's link is dropped, so problems reports it.
func decodeExceptions(b []byte) ([]exception, error) {
	var entries []exception
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&entries); err != nil {
		return nil, err
	}
	for i, e := range entries {
		if issueURL.FindString(e.Issue) != e.Issue {
			entries[i].Issue = ""
		}
	}
	return entries, nil
}

// govulncheckAllowFile holds the govulncheck findings `just vulncheck`
// lets pass: a JSON array of exceptions.
const govulncheckAllowFile = ".govulncheck-allow.json"

// vulnFinding is one vulnerability govulncheck found the code calling.
type vulnFinding struct {
	ID      string
	Summary string
	Module  string
	Fixed   string
}

// vulnResult is what `just vulncheck` reports.
type vulnResult struct {
	// Unallowed are the called findings no allowlist entry covers.
	Unallowed []vulnFinding
	// Problems are invalid or expired allowlist entries, each failing the
	// check by itself.
	Problems []string
}

// govulncheckMessage is one object of govulncheck's -format json stream.
type govulncheckMessage struct {
	OSV *struct {
		ID      string `json:"id"`
		Summary string `json:"summary"`
	} `json:"osv"`
	Finding *struct {
		OSV          string `json:"osv"`
		FixedVersion string `json:"fixed_version"`
		Trace        []struct {
			Module   string `json:"module"`
			Function string `json:"function"`
		} `json:"trace"`
	} `json:"finding"`
}

// vulnReport reads govulncheck's -format json stream and checks its called
// findings against allow, the committed allowlist, at now.
// govulncheck also reports vulnerable modules and packages the code
// imports without calling the vulnerable symbol; only a call fails, as it
// does in govulncheck's own text output.
func vulnReport(stream io.Reader, allow []byte, now time.Time) (vulnResult, error) {
	var r vulnResult
	summaries := map[string]string{}
	seen := map[string]bool{}
	dec := json.NewDecoder(stream)
	for {
		var m govulncheckMessage
		if err := dec.Decode(&m); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return r, err
		}
		if m.OSV != nil {
			summaries[m.OSV.ID] = m.OSV.Summary
		}
		f := m.Finding
		if f == nil || len(f.Trace) == 0 || f.Trace[0].Function == "" || seen[f.OSV] {
			continue
		}
		seen[f.OSV] = true
		r.Unallowed = append(r.Unallowed, vulnFinding{ID: f.OSV, Module: f.Trace[0].Module, Fixed: f.FixedVersion})
	}
	allowed, problems := parseExceptions(allow, govulncheckAllowFile, now)
	r.Problems = problems
	var unallowed []vulnFinding
	for _, f := range r.Unallowed {
		if !allowed[f.ID] {
			f.Summary = summaries[f.ID]
			unallowed = append(unallowed, f)
		}
	}
	r.Unallowed = unallowed
	return r, nil
}
