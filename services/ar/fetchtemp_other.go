//go:build !windows

package ar

import "os"

// createFetchTemp creates the temp file an upstream blob fetch writes to.
// Readers follow it while it grows and it is renamed into the blob store
// while still open.
func createFetchTemp(dir string) (*os.File, error) {
	return os.CreateTemp(dir, "fetch-")
}
