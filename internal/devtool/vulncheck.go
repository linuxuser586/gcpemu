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
	// section is a .trivyignore.yaml entry's section: "vulnerabilities"
	// or "licenses". It says which scan judges whether it is stale.
	section string
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

// judgeExceptions checks entries, file's exceptions, at now. found holds
// the IDs a scan found, when that scan covers everything the entries
// could apply to; nil when it does not, so none is judged stale. A stale
// entry, one that matched no finding, has nothing left to let pass: it is
// reported as stale whatever its other problems, since the fix is to
// delete it. valid holds the IDs of the entries that hold.
func judgeExceptions(entries []exception, file string, names exceptionFields, found map[string]bool, now time.Time) (valid map[string]bool, problems, stale []string) {
	valid = map[string]bool{}
	for _, e := range entries {
		if found != nil && !found[e.ID] {
			stale = append(stale, staleMessage(file, e))
			continue
		}
		p := e.problems(now, names)
		for _, q := range p {
			problems = append(problems, file+": "+q)
		}
		valid[e.ID] = len(p) == 0
	}
	return valid, problems, stale
}

// staleMessage is a stale entry's line, for a check's output and the
// weekly scan's rolling issue alike.
func staleMessage(file string, e exception) string {
	msg := fmt.Sprintf("%s: %s matched no finding; delete it", file, e.ID)
	if e.Issue != "" {
		msg += " and close " + e.Issue + " if nothing else holds it open"
	}
	return msg
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
	// Stale are allowlist entries no called finding matched, on any
	// GOOS. Each fails the check.
	Stale []string
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
	called := map[string]bool{}
	for _, f := range r.Unallowed {
		called[f.ID] = true
	}
	entries, err := decodeExceptions(allow)
	if err != nil {
		r.Problems = []string{fmt.Sprintf("%s: %v", govulncheckAllowFile, err)}
	}
	allowed, problems, stale := judgeExceptions(entries, govulncheckAllowFile, allowFields, called, now)
	r.Problems = append(r.Problems, problems...)
	r.Stale = stale
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
