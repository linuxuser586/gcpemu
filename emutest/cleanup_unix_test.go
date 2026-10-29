//go:build unix

package emutest_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/linuxuser586/gcpemu/emutest"
)

// TestStartReportsDataDirLeft: when Start's cleanup cannot remove the
// Instance's temporary data dir, the test is told, as with t.TempDir.
func TestStartReportsDataDirLeft(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root removes entries of read-only directories")
	}
	rec := &cleanupTB{TB: t}
	inst := emutest.Start(rec, []string{"gcs"})
	dir := inst.Internal().Dir
	stuck := filepath.Join(dir, "stuck")
	if err := os.MkdirAll(filepath.Join(stuck, "entry"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(stuck, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(stuck, 0o700)
		_ = os.RemoveAll(dir)
	})
	rec.runCleanups()
	if len(rec.errors) != 1 {
		t.Errorf("Start's cleanup reported %q, want the data dir it could not remove", rec.errors)
	}
}
