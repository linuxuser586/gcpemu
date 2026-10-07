package admin

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestSpecCoversRoutes: every /_emu/v1/ route registered in the source
// ("METHOD /_emu/v1/...") is described in openapi.yaml, and the document
// describes no route that doesn't exist.
func TestSpecCoversRoutes(t *testing.T) {
	var doc struct {
		OpenAPI string                               `yaml:"openapi"`
		Paths   map[string]map[string]map[string]any `yaml:"paths"`
	}
	if err := yaml.Unmarshal(OpenAPI, &doc); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(doc.OpenAPI, "3.") {
		t.Fatalf("openapi = %q", doc.OpenAPI)
	}
	spec := map[string]bool{}
	for path, ops := range doc.Paths {
		for method, op := range ops {
			if op["operationId"] == nil || op["responses"] == nil {
				t.Errorf("%s %s lacks operationId or responses", method, path)
			}
			spec[strings.ToUpper(method)+" "+path] = true
		}
	}

	root, _ := filepath.Abs("../..")
	route := regexp.MustCompile(`"(GET|POST|PUT|PATCH|DELETE) (/_emu/v1/[^"]*)"`)
	src := map[string]bool{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == "e2e" || strings.HasPrefix(d.Name(), ".")) {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		for _, m := range route.FindAllStringSubmatch(string(b), -1) {
			src[m[1]+" "+m[2]] = true
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(src) < 10 {
		t.Fatalf("found only %d routes in %s", len(src), root)
	}
	var missing, extra []string
	for r := range src {
		if !spec[r] {
			missing = append(missing, r)
		}
	}
	for r := range spec {
		if !src[r] {
			extra = append(extra, r)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	if len(missing) > 0 {
		t.Errorf("routes missing from openapi.yaml: %v", missing)
	}
	if len(extra) > 0 {
		t.Errorf("openapi.yaml describes routes that don't exist: %v", extra)
	}
}
