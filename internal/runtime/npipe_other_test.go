//go:build !windows

package runtime

import (
	"strings"
	"testing"
)

// TestNamedPipeRefused: npipe:// endpoints exist only on Windows, and the
// error says which scheme was refused (#374).
func TestNamedPipeRefused(t *testing.T) {
	_, err := New("npipe:////./pipe/docker_engine")
	if err == nil || !strings.Contains(err.Error(), "npipe://") {
		t.Errorf("New = %v, want an error naming npipe://", err)
	}
}
