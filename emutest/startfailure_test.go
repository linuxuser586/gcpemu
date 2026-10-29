package emutest_test

import (
	"context"
	"testing"
	"time"

	"github.com/linuxuser586/gcpemu/emutest"
)

// TestNewWhilePersistentInstanceRuns: a second Instance in the dir of a
// running Persistent Instance gets the start error from New, not a panic
// (#865).
func TestNewWhilePersistentInstanceRuns(t *testing.T) {
	dir := t.TempDir()
	emutest.Start(t, []string{"iam"}, emutest.WithTestDataDir(t, dir))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	inst, err := emutest.New(ctx, []string{"iam"}, emutest.WithDataDir(dir))
	if err == nil {
		_ = inst.Close(ctx)
		t.Fatal("second Instance started in a dir whose Instance is running")
	}
	if inst != nil {
		t.Errorf("New returned an Instance with error %v", err)
	}
}
