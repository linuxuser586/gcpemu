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
// and the last day it holds.
type exception struct {
	ID      string `json:"id"`
	Issue   string `json:"issue"`
	Expires string `json:"expires"`
	// Reason says why the finding can wait; it is not checked.
	Reason string `json:"reason"`
}

var issueURL = regexp.MustCompile(`^https://github\.com/linuxuser586/gcpemu/issues/[0-9]+$`)

// problems lists what makes e invalid on the date today. An entry that has
// a problem suppresses nothing.
func (e exception) problems(today time.Time) []string {
	var out []string
	if e.ID == "" {
		out = append(out, "an entry has no id")
	}
	if !issueURL.MatchString(e.Issue) {
		out = append(out, e.ID+": issue must link a gcpemu GitHub issue (https://github.com/linuxuser586/gcpemu/issues/N)")
	}
	if d, err := time.Parse(time.DateOnly, e.Expires); err != nil {
		out = append(out, e.ID+": expires must be a date (YYYY-MM-DD)")
	} else if y, m, day := today.Date(); d.Before(time.Date(y, m, day, 0, 0, 0, 0, time.UTC)) {
		out = append(out, fmt.Sprintf("%s: expired on %s; fix the finding, or extend the entry with the issue's agreement", e.ID, e.Expires))
	}
	return out
}

// allowlistFile holds the govulncheck findings `just vulncheck` lets pass:
// a JSON array of exceptions.
const allowlistFile = ".govulncheck-allow.json"

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
// findings against allow, the committed allowlist, on the date today.
// govulncheck also reports vulnerable modules and packages the code
// imports without calling the vulnerable symbol; only a call fails, as it
// does in govulncheck's own text output.
func vulnReport(stream io.Reader, allow []byte, today time.Time) (vulnResult, error) {
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
	var entries []exception
	dec = json.NewDecoder(bytes.NewReader(allow))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&entries); err != nil {
		r.Problems = append(r.Problems, fmt.Sprintf("%s: %v", allowlistFile, err))
		entries = nil
	}
	allowed := map[string]bool{}
	for _, e := range entries {
		p := e.problems(today)
		r.Problems = append(r.Problems, p...)
		allowed[e.ID] = len(p) == 0
	}
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
