//go:build unix

package main

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// TestHoldIgnoresSignals is #1035: ^C, and Playwright's graceful
// shutdown on Linux, signal the whole process group, hold included, while
// the harness is still stopping its Instance. The lock must outlast them,
// until the harness closes hold's stdin.
func TestHoldIgnoresSignals(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	cmd, stdin := startHold(t, dir)
	for _, sig := range []os.Signal{syscall.SIGINT, syscall.SIGTERM} {
		if err := cmd.Process.Signal(sig); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(500 * time.Millisecond)
	if lockFree(t, dir) {
		t.Fatal("the owner lock is free after SIGINT and SIGTERM")
	}
	stdin.Close()
	if err := waitExit(t, cmd); err != nil {
		t.Fatalf("hold after its stdin closed: %v", err)
	}
}
