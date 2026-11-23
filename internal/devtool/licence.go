package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// licenceExceptionsFile holds the packages `just licence-check` lets pass
// although Trivy found no licence for them: a JSON array of exceptions
// whose id is Trivy's package ID, name@version, so a new version is
// looked at again.
const licenceExceptionsFile = ".licence-exceptions.json"

// allowedLicences is SRS 2.4.2's list (Apache-2.0, MIT, BSD, MPL-2.0),
// read as licence families (ADR 0090): MIT's variants, every BSD variant
// including 0BSD, and ISC, the OpenBSD project's licence. ADR 0090 admits
// BlueOak-1.0.0 beside them: permissive, with a patent grant. Lower case.
var allowedLicences = map[string]bool{
	"apache-2.0": true,
	"mit":        true, "mit-0": true, "x11": true, "mit/x11": true,
	"bsd": true, "0bsd": true, "isc": true,
	"mpl-2.0":       true,
	"blueoak-1.0.0": true,
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
	// Unlicensed are packages Trivy read no licence for that
	// .licence-exceptions.json does not cover. A package without a licence
	// grants nothing, so each fails the check. Trivy reports no finding for
	// them, so .trivyignore.yaml cannot name them.
	Unlicensed []string
	// Problems are invalid or expired entries of .licence-exceptions.json
	// and .trivyignore.yaml. Each fails the check.
	Problems []string
	// Stale are entries of .licence-exceptions.json, and of
	// .trivyignore.yaml's licenses section, that matched no finding. Each
	// fails the check. None is judged when a lockfile is Unread, since
	// the check did not see everything they could apply to.
	Stale []string
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
		ExperimentalModifiedFindings []trivySuppressed `json:"ExperimentalModifiedFindings"`
	} `json:"Results"`
}

// licenceReport checks trivy fs --scanners license --show-suppressed
// --format json output, run with ignore, .trivyignore.yaml, which drops
// the licence findings it covers, and with exceptions,
// .licence-exceptions.json, for packages with none. It judges which
// entries of both are stale, from the licences Trivy found, suppressed or
// not, and the packages it read none for.
func licenceReport(b, exceptions, ignore []byte, now time.Time) (licenceResult, error) {
	var r licenceResult
	var rep trivyReport
	if err := json.Unmarshal(b, &rep); err != nil {
		return r, err
	}
	licences := map[string]bool{}   // every licence found, suppressed or not
	unlicensed := map[string]bool{} // every package read no licence for
	type pkg struct{ target, id string }
	var none []pkg // the same packages, with their lockfiles
	for _, res := range rep.Results {
		switch res.Class {
		case "license":
			for _, l := range res.Licenses {
				licences[l.Name] = true
				if !licenceAllowed(l.Name) {
					r.Disallowed = append(r.Disallowed, fmt.Sprintf("%s: %s is %s", res.Target, l.PkgName, l.Name))
				}
			}
			for _, m := range res.ExperimentalModifiedFindings {
				if m.Source == trivyIgnoreFile {
					licences[m.Finding.Name] = true
				}
			}
		case "lang-pkgs":
			pkgs := 0
			var ids []string
			for _, p := range res.Packages {
				if p.Relationship == "root" {
					continue
				}
				pkgs++
				if len(p.Licenses) == 0 {
					ids = append(ids, p.ID)
				}
			}
			if pkgs > 0 && len(ids) == pkgs {
				r.Unread = append(r.Unread, fmt.Sprintf("%s: no licence read for any of its %d packages; install its dependencies first", res.Target, pkgs))
				continue
			}
			for _, id := range ids {
				unlicensed[id] = true
				none = append(none, pkg{res.Target, id})
			}
		}
	}
	if len(r.Unread) > 0 {
		licences, unlicensed = nil, nil
	}
	excepted, problems, stale := judgeExceptionFile(exceptions, licenceExceptionsFile, unlicensed, now)
	r.Problems, r.Stale = problems, stale
	ignored, unread := parseTrivyIgnore(ignore)
	r.Problems = append(r.Problems, unread...)
	// Only the licenses section is this scan's to judge; the others are
	// checked for problems alone.
	licenceEntries, others := splitSection(ignored, "licenses")
	_, problems, stale = judgeExceptions(licenceEntries, trivyIgnoreFile, ignoreFields, licences, now)
	r.Problems, r.Stale = append(r.Problems, problems...), append(r.Stale, stale...)
	_, problems, _ = judgeExceptions(others, trivyIgnoreFile, ignoreFields, nil, now)
	r.Problems = append(r.Problems, problems...)
	for _, p := range none {
		if !excepted[p.id] {
			r.Unlicensed = append(r.Unlicensed, p.target+": "+p.id)
		}
	}
	return r, nil
}
