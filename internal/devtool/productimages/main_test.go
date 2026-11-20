package main

import (
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// sources are the files that set Product images (CONTEXT.md), the same
// list devtool's TestRenovateNeverTouchesProductImages keeps out of
// Renovate's reach (#896): Go files whose quoted image references the
// emulator runs, and the GKE add-on manifests, whose image: lines a
// cluster runs.
var (
	goSources = []string{
		"services/sql/images.go",
		"services/gke/versions.go",
		"internal/instance/managed.go",
	}
	manifestDir = "services/gke/addons"
)

var (
	quotedImage   = regexp.MustCompile(`"((?:[a-z0-9.-]+/)*[a-z0-9._-]+:[A-Za-z0-9._-]+)"`)
	manifestImage = regexp.MustCompile(`(?m)^\s*-?\s*image:\s*"?([^\s"]+)"?\s*$`)
)

// TestEveryProductImageListed: an image reference in a file that sets
// Product images must be in the output, so the weekly scan (#1165) never
// silently stops scanning one. A refresh that changes a tag changes both.
func TestEveryProductImageListed(t *testing.T) {
	got, err := productImages()
	if err != nil {
		t.Fatal(err)
	}
	var want []string
	for _, f := range goSources {
		for _, m := range quotedImage.FindAllStringSubmatch(read(t, f), -1) {
			want = append(want, m[1])
		}
	}
	entries, err := os.ReadDir(filepath.Join("..", "..", "..", manifestDir))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		for _, m := range manifestImage.FindAllStringSubmatch(read(t, filepath.Join(manifestDir, e.Name())), -1) {
			want = append(want, m[1])
		}
	}
	// systemImages builds CoreDNS's tag from an argument.
	want = append(want, "rancher/mirrored-coredns-coredns:1.14.7")
	if len(want) < 10 {
		t.Fatalf("found only %d image references in the sources: %v; has a file moved?", len(want), want)
	}
	for _, w := range want {
		if !slices.Contains(got, w) {
			t.Errorf("%s sets a Product image but product-images does not print it", w)
		}
	}
	if !slices.IsSorted(got) || len(slices.Compact(slices.Clone(got))) != len(got) {
		t.Errorf("output is not sorted and unique: %v", got)
	}
}

// read returns a repository file, gunzipped if it ends in .gz.
func read(t *testing.T, file string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "..", file))
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasSuffix(file, ".gz") {
		zr, err := gzip.NewReader(bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		if b, err = io.ReadAll(zr); err != nil {
			t.Fatal(err)
		}
	}
	return string(b)
}
