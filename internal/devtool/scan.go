package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	goruntime "runtime"
	"slices"
	"strings"
	"time"
)

// ADR 0090: which scans fail CI and which only report.

// govulncheckVersion is installed with go install, so go.mod gains
// nothing.
const govulncheckVersion = "v1.10.0"

// vulncheckGOOS are the platforms govulncheck analyses: it follows build
// constraints, so a vulnerable call in a Windows-only file is reached only
// in the windows run.
var vulncheckGOOS = []string{"linux", "darwin", "windows"}

// runVulncheck is `just vulncheck`: govulncheck on every platform, with
// the called findings checked against .govulncheck-allow.json.
func runVulncheck([]string) error {
	allow, err := os.ReadFile(govulncheckAllowFile)
	if err != nil {
		return err
	}
	// Installed for the host into a temporary GOBIN, since GOOS, which
	// sets the platform it analyses, would also cross-compile go run's
	// build of it.
	bin, err := os.MkdirTemp("", "govulncheck")
	if err != nil {
		return err
	}
	defer os.RemoveAll(bin)
	install := exec.Command("go", "install", "golang.org/x/vuln/cmd/govulncheck@"+govulncheckVersion)
	install.Env = append(os.Environ(), "GOBIN="+bin)
	install.Stdout, install.Stderr = os.Stderr, os.Stderr
	if err := install.Run(); err != nil {
		return fmt.Errorf("go install govulncheck: %w", err)
	}
	exe := filepath.Join(bin, "govulncheck")
	if goruntime.GOOS == "windows" {
		exe += ".exe"
	}
	var stream bytes.Buffer
	for _, goos := range vulncheckGOOS {
		fmt.Fprintf(os.Stderr, "govulncheck %s (GOOS=%s)\n", govulncheckVersion, goos)
		cmd := exec.Command(exe, "-format", "json", "./...")
		cmd.Env = append(os.Environ(), "GOOS="+goos)
		cmd.Stdout, cmd.Stderr = &stream, os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("govulncheck (GOOS=%s): %w", goos, err)
		}
	}
	r, err := vulnReport(&stream, allow, time.Now())
	if err != nil {
		return fmt.Errorf("reading govulncheck's output: %w", err)
	}
	for _, p := range slices.Concat(r.Problems, r.Stale) {
		fmt.Fprintln(os.Stderr, p)
	}
	for _, f := range r.Unallowed {
		fixed := "no fixed version"
		if f.Fixed != "" {
			fixed = "fixed in " + f.Fixed
		}
		fmt.Fprintf(os.Stderr, "%s (%s, %s): %s\n  https://pkg.go.dev/vuln/%s\n", f.ID, f.Module, fixed, f.Summary, f.ID)
	}
	if len(r.Problems)+len(r.Unallowed) > 0 {
		fmt.Fprintf(os.Stderr, "vulncheck: fix each finding, or add it to %s with an issue and an expiry date (ADR 0090)\n", govulncheckAllowFile)
	}
	if len(r.Stale) > 0 {
		fmt.Fprintln(os.Stderr, "vulncheck: a stale scan exception lets nothing pass; delete it (ADR 0090)")
	}
	if len(r.Problems)+len(r.Stale)+len(r.Unallowed) > 0 {
		return errReported
	}
	fmt.Println("vulncheck: no unallowed vulnerabilities called")
	return nil
}

// trivyVersionLine finds the Trivy version .github/install-trivy.sh pins,
// the one place it is written.
var trivyVersionLine = regexp.MustCompile(`(?m)^version=(\S+)$`)

// needTrivy checks that Trivy is on PATH, and returns .trivyignore.yaml
// for the check to judge.
func needTrivy() ([]byte, error) {
	b, err := os.ReadFile(".github/install-trivy.sh")
	if err != nil {
		return nil, err
	}
	m := trivyVersionLine.FindSubmatch(b)
	if m == nil {
		return nil, errors.New(".github/install-trivy.sh pins no version=")
	}
	want := string(m[1])
	have, err := output("", "trivy", "--version")
	if err != nil {
		fmt.Fprintf(os.Stderr, "trivy is not on PATH: install Trivy %s (.github/install-trivy.sh installs it, checksum-verified, on Linux and macOS)\n", want)
		return nil, errReported
	}
	if first, _, _ := strings.Cut(have, "\n"); strings.TrimSpace(strings.TrimPrefix(first, "Version:")) != want {
		fmt.Fprintf(os.Stderr, "warning: CI runs Trivy %s; this is %s\n", want, first)
	}
	return os.ReadFile(trivyIgnoreFile)
}

// runLicenceCheck is `just licence-check`: SRS 2.4.2's licence rule over
// what gcpemu ships (ADR 0090): go.mod, and the console's and
// setup-gcpemu's production dependencies. Trivy reads licences from
// installed dependencies (the Go module cache, node_modules), so those are
// installed first.
func runLicenceCheck([]string) error {
	ignore, err := needTrivy()
	if err != nil {
		return err
	}
	exceptions, err := os.ReadFile(licenceExceptionsFile)
	if err != nil {
		return err
	}
	r, err := scanLicences(exceptions, ignore)
	if err != nil {
		return err
	}
	for _, lines := range [][]string{r.Problems, r.Stale, r.Unread, r.Disallowed} {
		for _, l := range lines {
			fmt.Fprintln(os.Stderr, l)
		}
	}
	for _, u := range r.Unlicensed {
		fmt.Fprintf(os.Stderr, "%s: no licence found\n", u)
	}
	if len(r.Unread) > 0 {
		fmt.Fprintln(os.Stderr, "licence-check: run go mod download, pnpm install in console and npm ci in setup-gcpemu first")
	}
	if len(r.Disallowed) > 0 {
		fmt.Fprintf(os.Stderr, "licence-check: ADR 0090 allows Apache-2.0, MIT, BSD, ISC and MPL-2.0 (SRS 2.4.2), and BlueOak-1.0.0; replace the dependency, or add a scan exception for the licence to %s\n", trivyIgnoreFile)
	}
	if len(r.Unlicensed) > 0 {
		fmt.Fprintf(os.Stderr, "licence-check: a package without a licence grants nothing; replace it, or add a scan exception for name@version to %s\n", licenceExceptionsFile)
	}
	if len(r.Stale) > 0 {
		fmt.Fprintln(os.Stderr, "licence-check: a stale scan exception lets nothing pass; delete it (ADR 0090)")
	}
	if len(r.Problems)+len(r.Stale)+len(r.Unread)+len(r.Disallowed)+len(r.Unlicensed) > 0 {
		return errReported
	}
	fmt.Println("licence-check: every package has an allowed licence")
	return nil
}

// scanLicences runs Trivy's licence scan over the working tree and
// judges it with exceptions, .licence-exceptions.json, and ignore,
// .trivyignore.yaml: licence-check's scan and the weekly report's
// (-licences) alike, so the two cannot judge differently.
func scanLicences(exceptions, ignore []byte) (licenceResult, error) {
	// The compat suite's POM is a test client, never shipped.
	// --show-suppressed lists what .trivyignore.yaml suppressed, so its
	// licenses entries can be judged stale (ADR 0090).
	cmd := exec.Command("trivy", "fs", "--scanners", "license", "--format", "json", "--quiet", "--show-suppressed", "--ignorefile", trivyIgnoreFile, "--skip-files", "compat/sdk/java/pom.xml", ".")
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return licenceResult{}, fmt.Errorf("trivy: %w", err)
	}
	r, err := licenceReport(out, exceptions, ignore, time.Now())
	if err != nil {
		return licenceResult{}, fmt.Errorf("reading Trivy's output: %w", err)
	}
	return r, nil
}

// imageScanArgs are the trivy image flags of the release image scan and
// the weekly image report alike. --show-suppressed lists what
// .trivyignore.yaml suppressed, which the weekly report judges stale
// entries by; image-scan only prints it.
var imageScanArgs = []string{"image", "--scanners", "vuln", "--severity", "HIGH,CRITICAL", "--ignorefile", trivyIgnoreFile, "--show-suppressed", "--quiet"}

// runImageScan is `just image-scan <ref>`: a release image fails on any
// HIGH or CRITICAL vulnerability that has a fix.
func runImageScan(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: image-scan <image>")
	}
	ignore, err := needTrivy()
	if err != nil {
		return err
	}
	// One image cannot judge whether an entry is stale (ADR 0090).
	if p := trivyIgnoreProblems(ignore, time.Now()); len(p) > 0 {
		fmt.Fprintln(os.Stderr, strings.Join(p, "\n"))
		return errReported
	}
	cmd := exec.Command("trivy", append(imageScanArgs, "--ignore-unfixed", "--exit-code", "1", args[0])...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "image-scan: %s has fixable HIGH or CRITICAL vulnerabilities; fix them, or add each to %s with an issue and an expiry date (ADR 0090)\n", args[0], trivyIgnoreFile)
		return errReported
	}
	return nil
}

// runProductImages prints every Product image, one per line. The lister
// imports the emulator, which devtool must not (ADR 0061), so it is its
// own command.
func runProductImages([]string) error {
	images, err := productImageList()
	if err != nil {
		return err
	}
	for _, i := range images {
		fmt.Println(i)
	}
	return nil
}

// productImageList runs the Product image lister.
func productImageList() ([]string, error) {
	cmd := exec.Command("go", "run", "./internal/devtool/productimages")
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("product-images: %w", err)
	}
	return strings.Fields(string(out)), nil
}

// runImageReport is `just image-report`, the weekly image scan, report
// only (ADR 0090): it prints a Markdown summary of every Product and
// Helper image's HIGH and CRITICAL vulnerabilities, and those of the
// Release images -release names, already built, and fails only when it
// cannot scan one, after reporting the others. It also lists the stale
// .trivyignore.yaml vulnerabilities entries and the scan exceptions that
// stop holding within two weeks, so none expires unannounced. -licences
// runs licence-check's scan too, over the installed dependencies, and
// lists the stale licence scan exceptions (#1211); it fails only when
// that scan cannot run. With -issue FILE, it writes the fixable findings
// and those exceptions there as the rolling issue's body, and writes
// nothing when there are none.
func runImageReport(args []string) error {
	var err error
	fl := flag.NewFlagSet("image-report", flag.ContinueOnError)
	issueFile := fl.String("issue", "", "write the rolling issue's body here when there is anything to report")
	release := fl.String("release", "", "comma-separated Release images, already built, to scan too")
	withLicences := fl.Bool("licences", false, "judge the licence scan exceptions too (needs the dependencies installed)")
	if err := fl.Parse(args); err != nil {
		return err
	}
	// Not needTrivy: an invalid or expired scan exception is reported
	// below rather than stopping a report-only scan; Trivy itself ignores
	// an expired one.
	if _, err := exec.LookPath("trivy"); err != nil {
		_, err = needTrivy()
		return err
	}
	files := map[string][]byte{}
	for _, f := range []string{govulncheckAllowFile, licenceExceptionsFile, trivyIgnoreFile} {
		if files[f], err = os.ReadFile(f); err != nil {
			return err
		}
	}
	expiring, err := expiringExceptions(files, time.Now(), 14*24*time.Hour)
	if err != nil {
		return err
	}
	product, err := productImageList()
	if err != nil {
		return err
	}
	helper, err := helperImageList(".")
	if err != nil {
		return err
	}
	images := imageLists{Product: product, Helper: helper}
	if *release != "" {
		images.Release = strings.Split(*release, ",")
	}
	// Unreadable lines are reported by every PR's checks; here they only
	// leave entries unjudged.
	ignored, _ := parseTrivyIgnore(files[trivyIgnoreFile])
	// A registry's rate limit on one image must not hide the others'
	// findings: a failed scan is reported in its row and fails the run
	// at the end. A reference in two lists is scanned once.
	scans := map[string][]byte{}
	toScan := imagesToScan(images.Product, images.Helper, images.Release)
	for _, image := range toScan {
		fmt.Fprintf(os.Stderr, "scanning %s\n", image)
		cmd := exec.Command("trivy", append(imageScanArgs, "--format", "json", image)...)
		cmd.Stderr = os.Stderr
		if out, err := cmd.Output(); err != nil {
			fmt.Fprintf(os.Stderr, "trivy %s: %v\n", image, err)
		} else {
			scans[image] = out
		}
	}
	// A lockfile left unread, as by a failed install, is reported in the
	// stale section; only a scan that could not run fails the report.
	var licences *licenceJudgment
	if *withLicences {
		fmt.Fprintln(os.Stderr, "scanning dependency licences")
		licences = &licenceJudgment{}
		licences.result, licences.err = scanLicences(files[licenceExceptionsFile], files[trivyIgnoreFile])
	}
	r, err := reportImages(images, expiring, ignored, scans, licences)
	if err != nil {
		return err
	}
	fmt.Print(r.Summary)
	if *issueFile != "" && r.Issue != "" {
		if err := os.WriteFile(*issueFile, []byte(r.Issue), 0o644); err != nil {
			return err
		}
	}
	var failed []error
	if r.Failed > 0 {
		failed = append(failed, fmt.Errorf("%d of %d images could not be scanned", r.Failed, len(toScan)))
	}
	if licences != nil && licences.err != nil {
		failed = append(failed, fmt.Errorf("the licence scan could not run: %w", licences.err))
	}
	if err := errors.Join(failed...); err != nil {
		return err
	}
	return nil
}

// imagesToScan lists the images of each list in turn, each reference
// once.
func imagesToScan(lists ...[]string) []string {
	var refs []string
	seen := map[string]bool{}
	for _, image := range slices.Concat(lists...) {
		if !seen[image] {
			seen[image] = true
			refs = append(refs, image)
		}
	}
	return refs
}

// issueBodyLimit keeps the rolling issue's body under GitHub's 65536
// characters, with room for the workflow's preamble.
const issueBodyLimit = 60000

// imageReport is the weekly image scan's report.
type imageReport struct {
	// Summary, for the job summary, has every image section, each a
	// table of every image's HIGH and CRITICAL counts and each fixable
	// finding in full.
	Summary string
	// Issue, for the rolling issue, lists each image's fixable
	// vulnerability IDs, leaving out a section with none; "" when there
	// is nothing to report.
	Issue string
	// Failed counts the images, each once, that could not be scanned.
	Failed int
}

// imageLists are the images the weekly scan reports, by section.
type imageLists struct {
	Product, Helper []string
	// Release are the Release images, built from main for the runner's
	// architecture; none when -release names none.
	Release []string
}

// reportImages summarises trivy image --show-suppressed --format json
// output for the Product, Helper and Release images, then lists the stale
// and the expiring scan exceptions once, at the end. ignored are
// .trivyignore.yaml's entries; the images judge its vulnerabilities
// section, and licences, when -licences ran the licence check (#1211),
// judges .licence-exceptions.json and its licenses section. An image
// missing from scans could not be scanned; a reference in two lists is
// scanned once and reported in both sections.
func reportImages(images imageLists, expiring []string, ignored []exception, scans map[string][]byte, licences *licenceJudgment) (imageReport, error) {
	var r imageReport
	isProduct := map[string]bool{}
	for _, image := range images.Product {
		isProduct[image] = true
	}
	failed := map[string]bool{}
	budget := issueBodyLimit
	type imageSectionSpec struct {
		title     string
		images    []string
		isProduct map[string]bool
	}
	sections := []imageSectionSpec{
		{"Product images", images.Product, nil},
		{"Helper images", images.Helper, isProduct},
	}
	if len(images.Release) > 0 {
		sections = append(sections, imageSectionSpec{"Release images", images.Release, nil})
	}
	for _, sec := range sections {
		summary, fixable, err := imageSection(sec.images, sec.isProduct, scans, budget)
		if err != nil {
			return r, err
		}
		r.Summary += "## " + sec.title + "\n\n" + summary
		if fixable != "" {
			if r.Issue != "" {
				r.Issue += "\n"
			}
			r.Issue += "## " + sec.title + "\n\n" + fixable
			budget -= len(fixable)
		}
		for _, image := range sec.images {
			if scans[image] == nil {
				failed[image] = true
			}
		}
	}
	r.Failed = len(failed)
	stale, err := staleImageExceptions(ignored, images, failed, scans)
	if err != nil {
		return r, err
	}
	licenceLines := licences.lines()
	if lines := slices.Concat(stale.lines, licenceLines); len(lines) > 0 {
		section := "\n" + staleExceptionsHeading + "\n\n- " + strings.Join(lines, "\n- ") + "\n"
		r.Summary += section
		// An image judgment skipped is no news by itself: the failed
		// scan already fails the run. A licence line always is, since
		// nothing else would announce it.
		if stale.judged || len(licenceLines) > 0 || r.Issue != "" || len(expiring) > 0 {
			r.Issue += section
		}
	}
	// A stale entry is to be deleted, not fixed or renewed.
	staleRefs := slices.Concat(stale.refs, licences.staleRefs())
	expiring = slices.DeleteFunc(slices.Clone(expiring), func(line string) bool {
		return slices.ContainsFunc(staleRefs, func(ref string) bool {
			return strings.HasPrefix(line, ref+" ")
		})
	})
	if len(expiring) > 0 {
		section := "\n### Scan exceptions that stop holding within two weeks\n\n- " + strings.Join(expiring, "\n- ") + "\n"
		r.Summary += section
		r.Issue += section
	}
	return r, nil
}

const staleExceptionsHeading = "### Stale scan exceptions"

// licenceJudgment is the weekly scan's judgment of the licence scan
// exceptions (#1211): .licence-exceptions.json and .trivyignore.yaml's
// licenses section, from the licence check's result.
type licenceJudgment struct {
	result licenceResult
	// err is why the licence scan did not run; result is then empty.
	err error
}

// notJudgedLicences names the entries the licence judgment covers, for the
// line saying none was judged.
const notJudgedLicences = licenceExceptionsFile + " entries and " + trivyIgnoreFile + " licenses entries"

// lines are the judgment's lines for the stale section: the stale
// entries, or one line saying why none was judged; none when l is nil,
// as without -licences.
func (l *licenceJudgment) lines() []string {
	switch {
	case l == nil:
		return nil
	case l.err != nil:
		return []string{notJudgedLicences + " not judged this week, since the licence scan failed: " + l.err.Error()}
	case len(l.result.Unread) > 0:
		// licence-check judges nothing then either (ADR 0090).
		var lockfiles []string
		for _, u := range l.result.Unread {
			target, _, _ := strings.Cut(u, ":")
			lockfiles = append(lockfiles, target)
		}
		return []string{notJudgedLicences + " not judged this week: no licence was read from " + strings.Join(lockfiles, ", ") + ", so install its dependencies first"}
	}
	return l.result.Stale
}

// staleRefs name the stale entries as exceptionRef does; none when l is
// nil or judged nothing.
func (l *licenceJudgment) staleRefs() []string {
	if l == nil {
		return nil
	}
	return l.result.staleRefs
}

// staleImages is the weekly scan's judgment of .trivyignore.yaml's
// vulnerabilities entries.
type staleImages struct {
	// lines are the stale entries' messages, or one line saying why
	// none was judged; none when nothing is stale.
	lines []string
	// judged is false when the scan did not see everything the entries
	// could apply to.
	judged bool
	// refs name the stale entries as exceptionRef does.
	refs []string
}

// staleImageExceptions judges .trivyignore.yaml's vulnerabilities
// entries against every image's findings, suppressed or not (ADR 0090).
// Only a scan of every Product, Helper and Release image covers
// everything they could apply to, so when one could not be scanned, or no
// Release image was given, none is judged and the line says why.
func staleImageExceptions(ignored []exception, images imageLists, failed map[string]bool, scans map[string][]byte) (staleImages, error) {
	entries, _ := splitSection(ignored, "vulnerabilities")
	switch {
	case len(entries) == 0:
		return staleImages{judged: true}, nil
	case len(failed) > 0:
		return staleImages{lines: []string{fmt.Sprintf("%s vulnerabilities entries not judged this week: %d image(s) could not be scanned, and an entry might still hold for one.", trivyIgnoreFile, len(failed))}}, nil
	case len(images.Release) == 0:
		return staleImages{lines: []string{trivyIgnoreFile + " vulnerabilities entries not judged: no Release image was scanned (-release), and an entry might still hold for one."}}, nil
	}
	found := map[string]bool{}
	for image, b := range scans {
		var rep struct {
			Results []struct {
				Vulnerabilities []struct {
					VulnerabilityID string `json:"VulnerabilityID"`
				} `json:"Vulnerabilities"`
				ExperimentalModifiedFindings []trivySuppressed `json:"ExperimentalModifiedFindings"`
			} `json:"Results"`
		}
		if err := json.Unmarshal(b, &rep); err != nil {
			return staleImages{}, fmt.Errorf("%s: %w", image, err)
		}
		for _, res := range rep.Results {
			for _, v := range res.Vulnerabilities {
				found[v.VulnerabilityID] = true
			}
			for _, m := range res.ExperimentalModifiedFindings {
				if m.Source == trivyIgnoreFile {
					found[m.Finding.VulnerabilityID] = true
				}
			}
		}
	}
	_, _, stale := judgeExceptions(entries, trivyIgnoreFile, ignoreFields, found, time.Time{})
	return staleImages{lines: stale, judged: true, refs: staleRefs(entries, trivyIgnoreFile, found)}, nil
}

// imageSection summarises one list of images: summary is the table and
// each fixable finding in full, and fixable lists each image's fixable
// vulnerability IDs, within budget characters ("" when there are none).
// An image in isProduct is marked as also being a Product image.
func imageSection(images []string, isProduct map[string]bool, scans map[string][]byte, budget int) (summary, fixable string, err error) {
	var s, details, f strings.Builder
	truncated := false
	s.WriteString("| Image | CRITICAL | HIGH | Fixable |\n|---|---|---|---|\n")
	for _, image := range images {
		name := "`" + image + "`"
		if isProduct[image] {
			name += " (also a Product image)"
		}
		var rep struct {
			Results []struct {
				Target          string `json:"Target"`
				Vulnerabilities []struct {
					VulnerabilityID  string `json:"VulnerabilityID"`
					PkgName          string `json:"PkgName"`
					InstalledVersion string `json:"InstalledVersion"`
					FixedVersion     string `json:"FixedVersion"`
					Severity         string `json:"Severity"`
				} `json:"Vulnerabilities"`
			} `json:"Results"`
		}
		if scans[image] == nil {
			fmt.Fprintf(&s, "| %s | | | scan failed |\n", name)
			continue
		}
		if err := json.Unmarshal(scans[image], &rep); err != nil {
			return "", "", fmt.Errorf("%s: %w", image, err)
		}
		counts := map[string]int{}
		var fixes []string
		ids := map[string][]string{} // severity -> unique fixable IDs
		seen := map[string]bool{}
		for _, r := range rep.Results {
			for _, v := range r.Vulnerabilities {
				counts[v.Severity]++
				if v.FixedVersion == "" {
					continue
				}
				fixes = append(fixes, fmt.Sprintf("- %s %s: %s %s, fixed in %s (%s)", v.Severity, v.VulnerabilityID, v.PkgName, v.InstalledVersion, v.FixedVersion, r.Target))
				if !seen[v.VulnerabilityID] {
					seen[v.VulnerabilityID] = true
					ids[v.Severity] = append(ids[v.Severity], v.VulnerabilityID)
				}
			}
		}
		fmt.Fprintf(&s, "| %s | %d | %d | %d |\n", name, counts["CRITICAL"], counts["HIGH"], len(fixes))
		if len(fixes) == 0 {
			continue
		}
		fmt.Fprintf(&details, "\n<details><summary><code>%s</code>: %d fixable</summary>\n\n%s\n\n</details>\n", image, len(fixes), strings.Join(fixes, "\n"))
		line := fmt.Sprintf("- %s:", name)
		for _, sev := range []string{"CRITICAL", "HIGH"} {
			if len(ids[sev]) > 0 {
				line += fmt.Sprintf(" %s %s.", sev, strings.Join(ids[sev], ", "))
			}
		}
		switch {
		case truncated:
		case f.Len()+len(line) > budget:
			f.WriteString("- …and more images; see the run's job summary.\n")
			truncated = true
		default:
			f.WriteString(line + "\n")
		}
	}
	if details.Len() > 0 {
		s.WriteString("\n### Fixable findings\n" + details.String())
	}
	return s.String(), f.String(), nil
}
