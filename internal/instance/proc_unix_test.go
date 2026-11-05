//go:build unix

package instance

import (
	"os"
	"testing"
)

// TestProcessAliveWithoutPermission is #998: a process this user may not
// signal exists, as on Windows. Only root may signal pid 1, and to root
// it is alive anyway.
func TestProcessAliveWithoutPermission(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root may signal every process")
	}
	if !ProcessAlive(1) {
		t.Error("ProcessAlive(1) = false; want true for a process this user may not signal")
	}
}
