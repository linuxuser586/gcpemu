package instance

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linuxuser586/gcpemu/internal/ca"
	"github.com/linuxuser586/gcpemu/internal/config"
)

// failingConfig is a config New fails on late, after taking the state
// store (and, for an Ephemeral Instance, its temp dir): its explicitly
// requested Service has no factory (#872).
func failingConfig(t *testing.T, dir string, ephemeral bool) *config.Config {
	t.Helper()
	cfg := config.Defaults()
	cfg.Ephemeral = ephemeral
	cfg.DataDir = dir
	cfg.Services = []string{"dns"}
	return &cfg
}

// newFails runs New with no factories and checks it fails with the
// missing factory's error, unchanged by the rollback.
func newFails(t *testing.T, cfg *config.Config) {
	t.Helper()
	in, err := New(cfg, map[string]Factory{}, io.Discard)
	if err == nil {
		t.Fatalf("New succeeded: %v", in)
	}
	if err.Error() != `service "dns" is not implemented in this build` {
		t.Fatalf("New err = %v, want the missing factory", err)
	}
}

// TestFailedNewRemovesEphemeralTempDir is #872: a failed New leaves no
// gcpemu-* temp dir behind.
func TestFailedNewRemovesEphemeralTempDir(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	newFails(t, failingConfig(t, t.TempDir(), true))
	entries, err := os.ReadDir(tmp)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "gcpemu-") {
			t.Errorf("temp dir %s left behind", e.Name())
		}
	}
}

// TestFailedNewUnlocksPersistentDir is #872: a failed New closes the state
// store, so the next New in the instance dir reports its own error, not
// "is another instance using".
func TestFailedNewUnlocksPersistentDir(t *testing.T) {
	cfg := failingConfig(t, t.TempDir(), false)
	newFails(t, cfg)
	newFails(t, cfg)
}

// TestFailedNewRemovesAdminTokenKeepsCA is #872: on a non-loopback bind a
// failed New removes the Admin token file it wrote, and keeps the CA.
func TestFailedNewRemovesAdminTokenKeepsCA(t *testing.T) {
	dir := t.TempDir()
	cfg := failingConfig(t, dir, false)
	cfg.Bind = "0.0.0.0"
	newFails(t, cfg)
	if _, err := os.Stat(filepath.Join(dir, AdminTokenFile)); !os.IsNotExist(err) {
		t.Errorf("Admin token file left behind: %v", err)
	}
	for _, f := range []string{ca.CertFile, ca.KeyFile} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("CA file %s: %v", f, err)
		}
	}
}
