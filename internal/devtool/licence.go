package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

// allowedLicences is SRS 2.4.2's list (Apache-2.0, MIT, BSD, MPL-2.0),
// read as licence families (ADR 0090): MIT's variants, every BSD variant
// including 0BSD, and ISC, the OpenBSD project's licence. Lower case.
var allowedLicences = map[string]bool{
	"apache-2.0": true,
	"mit":        true, "mit-0": true, "x11": true, "mit/x11": true,
	"bsd": true, "0bsd": true, "isc": true,
	"mpl-2.0": true,
}

// licenceAllowed reports whether a licence name or SPDX expression is
// allowed: an OR needs one allowed side, an AND both, and a WITH exception
// is judged by its licence.
func licenceAllowed(expr string) bool {
	toks := strings.Fields(strings.NewReplacer("(", " ( ", ")", " ) ").Replace(expr))
	p := spdxParser{toks: toks}
	ok := p.or()
	return p.pos == len(toks) && p.err == nil && ok
}

// spdxParser evaluates an SPDX licence expression against
// allowedLicences: or := and {OR and}; and := term {AND term};
// term := ( or ) | licence [WITH exception].
type spdxParser struct {
	toks []string
	pos  int
	err  error
}

func (p *spdxParser) peek() string {
	if p.pos < len(p.toks) {
		return strings.ToUpper(p.toks[p.pos])
	}
	return ""
}

func (p *spdxParser) or() bool {
	ok := p.and()
	for p.peek() == "OR" {
		p.pos++
		ok = p.and() || ok
	}
	return ok
}

func (p *spdxParser) and() bool {
	ok := p.term()
	for p.peek() == "AND" {
		p.pos++
		ok = p.term() && ok
	}
	return ok
}

func (p *spdxParser) term() bool {
	switch t := p.peek(); t {
	case "", ")", "AND", "OR", "WITH":
		p.err = fmt.Errorf("unexpected %q", t)
		return false
	case "(":
		p.pos++
		ok := p.or()
		if p.peek() != ")" {
			p.err = fmt.Errorf("missing )")
			return false
		}
		p.pos++
		return ok
	}
	name := strings.ToLower(p.toks[p.pos])
	p.pos++
	if p.peek() == "WITH" {
		p.pos += 2
	}
	return allowedLicences[name] || strings.HasPrefix(name, "bsd-")
}

// licenceResult is what `just licence-check` reports.
type licenceResult struct {
	// Disallowed are "<lockfile>: <package> is <licence>" for each licence
	// outside the allowed families that .trivyignore.yaml does not cover.
	// Each fails the check.
	Disallowed []string
	// Unknown are packages Trivy read no licence for. Trivy reports no
	// finding for them, so .trivyignore.yaml cannot cover them: they are
	// listed for review, not failed.
	Unknown []string
	// Unread are lockfiles Trivy read no licence from at all: their
	// dependencies are not installed, so the check did not look. Each
	// fails the check.
	Unread []string
}

// trivyReport is the part of trivy fs --format json output the licence
// check reads.
type trivyReport struct {
	Results []struct {
		Target   string `json:"Target"`
		Class    string `json:"Class"`
		Packages []struct {
			ID           string   `json:"ID"`
			Relationship string   `json:"Relationship"`
			Licenses     []string `json:"Licenses"`
		} `json:"Packages"`
		Licenses []struct {
			PkgName string `json:"PkgName"`
			Name    string `json:"Name"`
		} `json:"Licenses"`
	} `json:"Results"`
}

// licenceReport checks trivy fs --scanners license --format json output,
// run with .trivyignore.yaml, which drops the licence findings it covers.
func licenceReport(b []byte) (licenceResult, error) {
	var r licenceResult
	var rep trivyReport
	if err := json.Unmarshal(b, &rep); err != nil {
		return r, err
	}
	for _, res := range rep.Results {
		switch res.Class {
		case "license":
			for _, l := range res.Licenses {
				if !licenceAllowed(l.Name) {
					r.Disallowed = append(r.Disallowed, fmt.Sprintf("%s: %s is %s", res.Target, l.PkgName, l.Name))
				}
			}
		case "lang-pkgs":
			pkgs, read := 0, 0
			for _, p := range res.Packages {
				if p.Relationship == "root" {
					continue
				}
				pkgs++
				if len(p.Licenses) > 0 {
					read++
				} else {
					r.Unknown = append(r.Unknown, res.Target+": "+p.ID)
				}
			}
			if pkgs > 0 && read == 0 {
				r.Unread = append(r.Unread, fmt.Sprintf("%s: no licence read for any of its %d packages; install its dependencies first", res.Target, pkgs))
				r.Unknown = r.Unknown[:len(r.Unknown)-pkgs]
			}
		}
	}
	return r, nil
}
