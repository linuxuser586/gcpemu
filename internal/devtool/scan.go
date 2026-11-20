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
	"strings"
	"time"
)

// ADR 0090: which scans fail CI and which only report.

// govulncheckVersion is installed with go install, so go.mod gains
// nothing.
const govulncheckVersion = "v1.9.0"

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
	for _, p := range r.Problems {
		fmt.Fprintf(os.Stderr, "%s: %s\n", govulncheckAllowFile, p)
	}
	for _, f := range r.Unallowed {
		fixed := "no fixed version"
		if f.Fixed != "" {
			fixed = "fixed in " + f.Fixed
		}
		fmt.Fprintf(os.Stderr, "%s (%s, %s): %s\n  https://pkg.go.dev/vuln/%s\n", f.ID, f.Module, fixed, f.Summary, f.ID)
	}
	if len(r.Problems) > 0 || len(r.Unallowed) > 0 {
		fmt.Fprintf(os.Stderr, "vulncheck: fix each finding, or add it to %s with an issue and an expiry date (ADR 0090)\n", govulncheckAllowFile)
		return errReported
	}
	fmt.Println("vulncheck: no unallowed vulnerabilities called")
	return nil
}

// trivyVersionLine finds the Trivy version .github/install-trivy.sh pins,
// the one place it is written.
var trivyVersionLine = regexp.MustCompile(`(?m)^version=(\S+)$`)

// needTrivy checks that Trivy is on PATH and that .trivyignore.yaml is
// valid, before any Trivy check runs.
func needTrivy() error {
	b, err := os.ReadFile(".github/install-trivy.sh")
	if err != nil {
		return err
	}
	m := trivyVersionLine.FindSubmatch(b)
	if m == nil {
		return errors.New(".github/install-trivy.sh pins no version=")
	}
	want := string(m[1])
	have, err := output("", "trivy", "--version")
	if err != nil {
		fmt.Fprintf(os.Stderr, "trivy is not on PATH: install Trivy %s (.github/install-trivy.sh installs it, checksum-verified, on Linux and macOS)\n", want)
		return errReported
	}
	if first, _, _ := strings.Cut(have, "\n"); strings.TrimSpace(strings.TrimPrefix(first, "Version:")) != want {
		fmt.Fprintf(os.Stderr, "warning: CI runs Trivy %s; this is %s\n", want, first)
	}
	ignore, err := os.ReadFile(trivyIgnoreFile)
	if err != nil {
		return err
	}
	if p := trivyIgnoreProblems(ignore, time.Now()); len(p) > 0 {
		fmt.Fprintln(os.Stderr, strings.Join(p, "\n"))
		return errReported
	}
	return nil
}

// runLicenceCheck is `just licence-check`: SRS 2.4.2's licence rule over
// go.mod, the console's and setup-gcpemu's lockfiles and the compat
// suite's POM. Trivy reads licences from installed dependencies (the Go
// module cache, node_modules, ~/.m2), so those are installed first.
func runLicenceCheck([]string) error {
	if err := needTrivy(); err != nil {
		return err
	}
	cmd := exec.Command("trivy", "fs", "--scanners", "license", "--format", "json", "--quiet", "--ignorefile", trivyIgnoreFile, ".")
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("trivy: %w", err)
	}
	r, err := licenceReport(out)
	if err != nil {
		return fmt.Errorf("reading Trivy's output: %w", err)
	}
	for _, u := range r.Unknown {
		fmt.Fprintf(os.Stderr, "warning: no licence found for %s; check it by hand\n", u)
	}
	for _, u := range r.Unread {
		fmt.Fprintln(os.Stderr, u)
	}
	for _, d := range r.Disallowed {
		fmt.Fprintln(os.Stderr, d)
	}
	if len(r.Unread) > 0 {
		fmt.Fprintln(os.Stderr, "licence-check: run go mod download, pnpm install in console, npm ci in setup-gcpemu and mvn -f compat/sdk/java/pom.xml dependency:resolve first")
	}
	if len(r.Disallowed) > 0 {
		fmt.Fprintf(os.Stderr, "licence-check: SRS 2.4.2 allows Apache-2.0, MIT, BSD, ISC and MPL-2.0 (ADR 0090); replace the dependency, or add the licence to %s with an issue and an expiry date\n", trivyIgnoreFile)
	}
	if len(r.Unread) > 0 || len(r.Disallowed) > 0 {
		return errReported
	}
	fmt.Println("licence-check: every licence found is allowed")
	return nil
}

// imageScanArgs are the trivy image flags of the release image scan and
// the weekly Product image scan alike.
var imageScanArgs = []string{"image", "--scanners", "vuln", "--severity", "HIGH,CRITICAL", "--ignorefile", trivyIgnoreFile, "--quiet"}

// runImageScan is `just image-scan <ref>`: a release image fails on any
// HIGH or CRITICAL vulnerability that has a fix.
func runImageScan(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: image-scan <image>")
	}
	if err := needTrivy(); err != nil {
		return err
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

// runProductImageScan is the weekly Product image scan, report only (ADR
// 0090): it prints a Markdown summary of every Product image's HIGH and
// CRITICAL vulnerabilities, and fails only when it cannot scan one, after
// reporting the others. With -fixable FILE, it writes the fixable ones
// there as the rolling issue's body, and writes nothing when there are
// none.
func runProductImageScan(args []string) error {
	fl := flag.NewFlagSet("product-image-scan", flag.ContinueOnError)
	fixableFile := fl.String("fixable", "", "write the fixable findings here, as Markdown, when there are any")
	if err := fl.Parse(args); err != nil {
		return err
	}
	if err := needTrivy(); err != nil {
		return err
	}
	images, err := productImageList()
	if err != nil {
		return err
	}
	// A registry's rate limit on one image must not hide the others'
	// findings: a failed scan is reported in its row and fails the run
	// at the end.
	scans := map[string][]byte{}
	failed := 0
	for _, image := range images {
		fmt.Fprintf(os.Stderr, "scanning %s\n", image)
		cmd := exec.Command("trivy", append(imageScanArgs, "--format", "json", image)...)
		cmd.Stderr = os.Stderr
		if out, err := cmd.Output(); err != nil {
			fmt.Fprintf(os.Stderr, "trivy %s: %v\n", image, err)
			failed++
		} else {
			scans[image] = out
		}
	}
	summary, fixable, err := productScanMarkdown(images, scans)
	if err != nil {
		return err
	}
	fmt.Print(summary)
	if *fixableFile != "" && fixable != "" {
		if err := os.WriteFile(*fixableFile, []byte(fixable), 0o644); err != nil {
			return err
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d Product images could not be scanned", failed, len(images))
	}
	return nil
}

// issueBodyLimit keeps the rolling issue's body under GitHub's 65536
// characters, with room for the workflow's preamble.
const issueBodyLimit = 60000

// productScanMarkdown summarises trivy image --format json output per
// image; an image missing from scans could not be scanned. summary, for
// the job summary, is a table of every image's HIGH and CRITICAL counts
// and each fixable finding in full; fixable, for the rolling issue, lists
// each image's fixable vulnerability IDs ("" when there are none).
func productScanMarkdown(images []string, scans map[string][]byte) (summary, fixable string, err error) {
	var s, details, f strings.Builder
	truncated := false
	s.WriteString("## Product image scan (report only, ADR 0090)\n\n| Image | CRITICAL | HIGH | Fixable |\n|---|---|---|---|\n")
	for _, image := range images {
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
			fmt.Fprintf(&s, "| `%s` | | | scan failed |\n", image)
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
		fmt.Fprintf(&s, "| `%s` | %d | %d | %d |\n", image, counts["CRITICAL"], counts["HIGH"], len(fixes))
		if len(fixes) == 0 {
			continue
		}
		fmt.Fprintf(&details, "\n<details><summary><code>%s</code>: %d fixable</summary>\n\n%s\n\n</details>\n", image, len(fixes), strings.Join(fixes, "\n"))
		line := fmt.Sprintf("- `%s`:", image)
		for _, sev := range []string{"CRITICAL", "HIGH"} {
			if len(ids[sev]) > 0 {
				line += fmt.Sprintf(" %s %s.", sev, strings.Join(ids[sev], ", "))
			}
		}
		switch {
		case truncated:
		case f.Len()+len(line) > issueBodyLimit:
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
