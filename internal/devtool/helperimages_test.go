package main

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// writeFixture writes files under a new directory and returns it.
func writeFixture(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, body := range files {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

const fixtureManagers = `{"customManagers": [
  {"managerFilePatterns": ["a.go"], "matchStrings": ["busybox:(?<currentValue>\\d+\\.\\d+)"], "depNameTemplate": "busybox", "datasourceTemplate": "docker"},
  {"managerFilePatterns": ["tool.sh"], "matchStrings": ["version=(?<currentValue>\\S+)"], "depNameTemplate": "helm/helm", "datasourceTemplate": "github-releases"}
  %s
]}`

// TestHelperImageListAddsDockerManager: every docker custom manager is a
// Helper image, and nothing else is.
func TestHelperImageListAddsDockerManager(t *testing.T) {
	files := map[string]string{
		"a.go":    `img := "busybox:1.36"`,
		"b.go":    `img := "alpine/git:v2.49.1" // and again: alpine/git:v2.49.1`,
		"tool.sh": "version=v3.0.0\n",
	}
	files["renovate.json"] = fmt.Sprintf(fixtureManagers, "")
	got, err := helperImageList(writeFixture(t, files))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"busybox:1.36"}; !slices.Equal(got, want) {
		t.Errorf("before: got %q, want %q", got, want)
	}

	files["renovate.json"] = fmt.Sprintf(fixtureManagers, `,
  {"managerFilePatterns": ["b.go"], "matchStrings": ["alpine/git:(?<currentValue>v[\\d.]+)"], "depNameTemplate": "alpine/git", "datasourceTemplate": "docker"}`)
	got, err = helperImageList(writeFixture(t, files))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"alpine/git:v2.49.1", "busybox:1.36"}; !slices.Equal(got, want) {
		t.Errorf("after: got %q, want %q", got, want)
	}
}
