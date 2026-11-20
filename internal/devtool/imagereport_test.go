package main

import (
	"slices"
	"strings"
	"testing"
)

// trivyJSON is trivy image --format json output with one HIGH finding,
// fixable when fixed is set.
func trivyJSON(id, fixed string) []byte {
	return []byte(`{"Results": [{"Target": "alpine 3.22", "Vulnerabilities": [
  {"VulnerabilityID": "` + id + `", "PkgName": "libssl3", "InstalledVersion": "3.5.0-r0", "FixedVersion": "` + fixed + `", "Severity": "HIGH"}
]}]}`)
}

// section returns the part of md from heading to the next heading of the
// same level, or "" when md has no such heading.
func section(md, heading string) string {
	_, after, ok := strings.Cut(md, heading+"\n")
	if !ok {
		return ""
	}
	if i := strings.Index(after, "\n## "); i >= 0 {
		after = after[:i]
	}
	return after
}

// TestImageReportSharedRef: k3s is both GKE's Product image and the
// Edge relay's Helper image. One scan reports it in both sections.
func TestImageReportSharedRefScannedOnceShownInBoth(t *testing.T) {
	const k3s = "rancher/k3s:v1.0.0-k3s1"
	r, err := reportImages([]string{"postgres:18", k3s}, []string{"busybox:1.0", k3s}, nil, map[string][]byte{
		"postgres:18":  trivyJSON("CVE-2026-0001", ""),
		"busybox:1.0": trivyJSON("CVE-2026-0002", ""),
		k3s:            trivyJSON("CVE-2026-0003", "3.5.1-r0"),
	})
	if err != nil {
		t.Fatal(err)
	}
	product, helper := section(r.Summary, "## Product images"), section(r.Summary, "## Helper images")
	if !strings.Contains(product, "| `"+k3s+"` | 0 | 1 | 1 |") {
		t.Errorf("Product section lacks k3s's row:\n%s", product)
	}
	if !strings.Contains(helper, "| `"+k3s+"` (also a Product image) | 0 | 1 | 1 |") {
		t.Errorf("Helper section lacks k3s's marked row:\n%s", helper)
	}
	for name, md := range map[string]string{"summary": r.Summary, "issue": r.Issue} {
		if !strings.Contains(section(md, "## Helper images"), "CVE-2026-0003") || !strings.Contains(section(md, "## Product images"), "CVE-2026-0003") {
			t.Errorf("%s does not list k3s's fixable finding in both sections:\n%s", name, md)
		}
	}
	if r.Failed != 0 {
		t.Errorf("Failed = %d, want 0", r.Failed)
	}
}

// TestImageReportIssueLeavesOutCleanSection: the job summary always has
// both image sections; the issue body only the one with something
// fixable. Expiring scan exceptions come once, at the end of each.
func TestImageReportIssueLeavesOutCleanSection(t *testing.T) {
	r, err := reportImages([]string{"postgres:18"}, []string{"busybox:1.0"}, []string{"CVE-2026-0009 (.trivyignore.yaml): expires 2026-12-01"}, map[string][]byte{
		"postgres:18":  trivyJSON("CVE-2026-0001", ""),
		"busybox:1.0": trivyJSON("CVE-2026-0002", "1.0.1"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.Summary, "## Product images\n") || !strings.Contains(r.Summary, "## Helper images\n") {
		t.Errorf("summary lacks an image section:\n%s", r.Summary)
	}
	if strings.Contains(r.Issue, "## Product images") {
		t.Errorf("issue has a Product section with nothing fixable:\n%s", r.Issue)
	}
	if !strings.Contains(section(r.Issue, "## Helper images"), "- `busybox:1.0`: HIGH CVE-2026-0002.") {
		t.Errorf("issue lacks busybox's finding:\n%s", r.Issue)
	}
	const expiring = "### Scan exceptions that stop holding within two weeks"
	for name, md := range map[string]string{"summary": r.Summary, "issue": r.Issue} {
		if strings.Count(md, expiring) != 1 || !strings.HasSuffix(strings.TrimSpace(md), "expires 2026-12-01") {
			t.Errorf("%s does not end with the expiring exceptions, once:\n%s", name, md)
		}
	}
}

// TestImageReportHelperScanFailed: a Helper image that could not be
// scanned gets its row, the others are still reported, and the failures
// of both sections are counted, a shared reference once.
func TestImageReportHelperScanFailed(t *testing.T) {
	const k3s = "rancher/k3s:v1.0.0-k3s1"
	r, err := reportImages([]string{"postgres:18", k3s}, []string{"busybox:1.0", "docker:1.0.0-dind", k3s}, nil, map[string][]byte{
		"postgres:18":  trivyJSON("CVE-2026-0001", "18.1"),
		"busybox:1.0": trivyJSON("CVE-2026-0002", ""),
	})
	if err != nil {
		t.Fatal(err)
	}
	helper := section(r.Summary, "## Helper images")
	for _, row := range []string{
		"| `docker:1.0.0-dind` | | | scan failed |",
		"| `" + k3s + "` (also a Product image) | | | scan failed |",
		"| `busybox:1.0` | 0 | 1 | 0 |",
	} {
		if !strings.Contains(helper, row) {
			t.Errorf("Helper section lacks %q:\n%s", row, helper)
		}
	}
	if !strings.Contains(r.Issue, "CVE-2026-0001") {
		t.Errorf("issue lacks postgres's finding:\n%s", r.Issue)
	}
	if r.Failed != 2 {
		t.Errorf("Failed = %d, want 2 (docker and k3s, once)", r.Failed)
	}
}

// TestImagesToScanOnce: a reference in both lists is scanned once.
func TestImagesToScanOnce(t *testing.T) {
	got := imagesToScan([]string{"postgres:18", "rancher/k3s:v1.0.0-k3s1"}, []string{"busybox:1.0", "rancher/k3s:v1.0.0-k3s1"})
	want := []string{"postgres:18", "rancher/k3s:v1.0.0-k3s1", "busybox:1.0"}
	if !slices.Equal(got, want) {
		t.Errorf("imagesToScan = %q, want %q", got, want)
	}
}
