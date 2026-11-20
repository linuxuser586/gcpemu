package main

import (
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

// productImageFiles are the files that set Product images (CONTEXT.md):
// Cloud SQL's postgres images, a GKE cluster's k3s and the system images it
// preloads, the Managed emulator images and the vendored GKE add-on
// manifests. Renovate must never propose an update to them (#896).
var productImageFiles = []string{
	"services/sql/images.go",
	"services/gke/versions.go",
	"internal/instance/managed.go",
	"services/gke/addons/gateway-api-v1.5.1-experimental.yaml.gz",
	"services/gke/addons/gateway-api-v1.5.1-standard.yaml.gz",
	"services/gke/addons/managed-otel.yaml",
	"services/gke/addons/managed-prometheus-v0.17.0.yaml.gz",
	"services/gke/addons/secret-manager.yaml",
	"services/gke/addons/secret-sync.yaml",
}

func loadRenovate(t *testing.T) (renovateConfig, []string) {
	t.Helper()
	c, err := readRenovate("../..")
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("git", "-C", "../..", "ls-files", "-z").Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	return c, strings.Split(strings.TrimRight(string(out), "\x00"), "\x00")
}

// ignored reports whether Renovate's ignorePaths cover file. Only exact
// paths and a trailing /** are used in renovate.json, and only those are
// understood here.
func ignored(c renovateConfig, file string) bool {
	for _, p := range c.IgnorePaths {
		if dir, ok := strings.CutSuffix(p, "/**"); ok && strings.HasPrefix(file, dir+"/") || p == file {
			return true
		}
	}
	return false
}

func readRepoFile(t *testing.T, file string) string {
	t.Helper()
	b, err := os.ReadFile("../../" + file)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestRenovateHelperImagesListed: helper images, those of Managed
// containers that only carry traffic or run helpers, are matched by file
// path, not by name (#896), so a file that gains one of their tags must be
// added to its manager's list, or Renovate leaves it behind on the next
// update.
func TestRenovateHelperImagesListed(t *testing.T) {
	c, files := loadRenovate(t)
	managers := 0
	var resolved []string
	for _, m := range c.CustomManagers {
		if m.DatasourceTemplate != "docker" {
			continue
		}
		managers++
		image := m.DepNameTemplate
		// Every listed file holds the image at one tag: the one the
		// manager's regex reads.
		for _, f := range m.ManagerFilePatterns {
			if !slices.Contains(files, f) {
				t.Errorf("%s: managerFilePatterns entry %q is not a tracked file; list exact paths", image, f)
			}
		}
		tags, empty, err := m.tags("../..")
		if err != nil {
			t.Error(err)
			continue
		}
		for _, f := range empty {
			t.Errorf("%s: %s is listed but holds no %s tag", image, f, image)
		}
		if len(tags) != 1 {
			t.Errorf("%s: listed files disagree on the tag: %v", image, tags)
			continue
		}
		var ref string
		for v := range tags {
			ref = image + ":" + v
		}
		resolved = append(resolved, ref)
		for _, f := range files {
			if slices.Contains(m.ManagerFilePatterns, f) || ignored(c, f) || strings.HasPrefix(f, "docs/releases/") {
				continue
			}
			if strings.Contains(readRepoFile(t, f), ref) {
				t.Errorf("%s holds %s but is not in that manager's managerFilePatterns (renovate.json)", f, ref)
			}
		}
	}
	if managers == 0 {
		t.Fatal("renovate.json has no docker custom managers")
	}
	// helper-images, which the weekly scan reads (ADR 0090), lists what
	// this test resolves.
	listed, err := helperImageList("../..")
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(resolved)
	if resolved = slices.Compact(resolved); !slices.Equal(listed, resolved) {
		t.Errorf("helper-images lists %q; renovate.json's docker managers resolve to %q", listed, resolved)
	}
}

// TestRenovateNeverTouchesProductImages: a Product image refresh is a
// deliberate change (ADR 0084), so every file that sets one is in
// ignorePaths, which binds default and custom managers alike, and no
// custom manager lists one.
func TestRenovateNeverTouchesProductImages(t *testing.T) {
	c, files := loadRenovate(t)
	for _, f := range productImageFiles {
		if !slices.Contains(files, f) {
			t.Errorf("%s is not a tracked file; update productImageFiles", f)
		}
		if !ignored(c, f) {
			t.Errorf("%s sets a Product image but is not in renovate.json's ignorePaths", f)
		}
	}
	for _, m := range c.CustomManagers {
		for _, f := range m.ManagerFilePatterns {
			if ignored(c, f) {
				t.Errorf("custom manager %q lists %s, which ignorePaths covers", m.Description, f)
			}
		}
	}
}
