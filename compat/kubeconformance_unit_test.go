//go:build compat

package compat

import (
	"slices"
	"testing"
)

func TestNewestPatches(t *testing.T) {
	got := newestPatches([]string{
		"1.36.4-gke.100", "1.37.1-gke.100", "1.35.8-gke.100",
		"1.37.0-gke.100", "1.35.9-gke.100", "1.36.5-gke.100", "1.36.10-gke.100",
	})
	want := []string{"1.35.9-gke.100", "1.36.10-gke.100", "1.37.1-gke.100"}
	if !slices.Equal(got, want) {
		t.Errorf("newestPatches = %v, want %v", got, want)
	}
}
