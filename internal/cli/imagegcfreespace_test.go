package cli

import (
	"path/filepath"
	"strings"
	"testing"
)

// --gke-image-gc-free-space sets GKE node
// image GC's free-space floor over the environment, which sets it over the
// config file (FR-CORE-010, #1227).
func TestGKEImageGCFreeSpaceFlag(t *testing.T) {
	t.Setenv("GCPEMU_GKE_IMAGE_GC_FREE_SPACE", "")
	if got := loadUnder(t, "", nil, nil, "").GKEImageGCFreeSpace; got != "" {
		t.Errorf("unset: %q, want empty (the default)", got)
	}
	if got := loadUnder(t, "", nil, nil, "gkeImageGcFreeSpace: 2GiB\n").GKEImageGCFreeSpace; got != "2GiB" {
		t.Errorf("file: %q, want 2GiB", got)
	}
	t.Setenv("GCPEMU_GKE_IMAGE_GC_FREE_SPACE", "4GiB")
	if got := loadUnder(t, "", nil, nil, "gkeImageGcFreeSpace: 2GiB\n").GKEImageGCFreeSpace; got != "4GiB" {
		t.Errorf("env over file: %q, want 4GiB", got)
	}
	if got := loadUnder(t, "", []string{"--gke-image-gc-free-space", "0"}, nil, "gkeImageGcFreeSpace: 2GiB\n").GKEImageGCFreeSpace; got != "0" {
		t.Errorf("flag over env: %q, want 0", got)
	}
}

// An unusable free-space floor stops `gcpemu start` with an error naming
// where it came from, rather than becoming the default (#1227).
func TestStartRefusesUnusableGKEImageGCFreeSpace(t *testing.T) {
	t.Setenv("GCPEMU_GKE_IMAGE_GC_FREE_SPACE", "")
	dir := t.TempDir()
	root := New(nil)
	root.SetArgs([]string{"start", "--config", filepath.Join(dir, "none.yaml"), "--data-dir", dir, "--services", "gke", "--gke-image-gc-free-space", "lots"})
	root.SilenceUsage = true
	err := root.Execute()
	if err == nil {
		t.Fatal("start succeeded")
	}
	for _, w := range []string{"--gke-image-gc-free-space", "GCPEMU_GKE_IMAGE_GC_FREE_SPACE", "gkeImageGcFreeSpace", "lots"} {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("error %q does not mention %q", err, w)
		}
	}
}
