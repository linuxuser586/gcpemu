package emu

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLinkAndCopyTree(t *testing.T) {
	src := t.TempDir()
	if err := os.MkdirAll(filepath.Join(src, "a", "b"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "a", "b", "f"), []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, tree := range map[string]func(string, string) error{"LinkTree": LinkTree, "CopyTree": CopyTree} {
		dst := filepath.Join(t.TempDir(), "out")
		if err := tree(src, dst); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if b, err := os.ReadFile(filepath.Join(dst, "a", "b", "f")); err != nil || string(b) != "data" {
			t.Errorf("%s: copied %q, %v", name, b, err)
		}
		if err := tree(filepath.Join(src, "missing"), dst); err != nil {
			t.Errorf("%s of a missing tree: %v", name, err)
		}
	}
	// A copy is independent of its source; CopyTree is for mutable files.
	dst := t.TempDir()
	if err := CopyTree(src, dst); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "a", "b", "f"), []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "a", "b", "f")); string(b) != "data" {
		t.Errorf("CopyTree copy changed with its source: %q", b)
	}
}
