//go:build !windows

package cdn

import "os"

// createSpill creates the temp file a fill spills to. It is renamed into
// the disk tier while still open.
func createSpill(dir string) (*os.File, error) {
	return os.CreateTemp(dir, "fill-*.tmp")
}

// openBody opens a cached body for reading. The entry may be evicted and
// its file removed while the handle is open.
func openBody(name string) (*os.File, error) {
	return os.Open(name)
}
