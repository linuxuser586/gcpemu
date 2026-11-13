package sql

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// A restore bundle left by a process that died mid-clone is removed when
// the service starts again (#1110).
func TestStartRemovesLeftoverRestoreBundles(t *testing.T) {
	s, _ := fileService(t)
	dir := filepath.Join(s.env.DataDir, "sql", restoreStagingName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	left := filepath.Join(dir, "gcpemu-sql-restore-123.tar.gz")
	if err := os.WriteFile(left, []byte("bundle"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Stop(context.Background()) })

	if _, err := os.Stat(left); !os.IsNotExist(err) {
		t.Errorf("leftover restore bundle after Start: %v, want it removed", err)
	}
}
