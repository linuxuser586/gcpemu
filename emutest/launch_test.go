package emutest

import (
	"context"
	"os"
	"testing"
)

// TestLaunchFailureCleansUp: an Instance that fails to start drops the
// host loopback lock and removes the temp data dir emutest made, and
// returns the start error rather than panicking (#865).
func TestLaunchFailureCleansUp(t *testing.T) {
	cfg := testConfig([]string{"no-such-service"}, nil)
	released := 0
	i, err := launch(context.Background(), &cfg, func() { released++ })
	if err == nil {
		t.Fatal("launch with an unknown service succeeded")
	}
	if i != nil {
		t.Errorf("launch returned an Instance with error %v", err)
	}
	if released != 1 {
		t.Errorf("host loopback lock released %d times, want 1", released)
	}
	if cfg.DataDir == "" {
		t.Fatal("launch made no temp data dir")
	}
	if _, err := os.Stat(cfg.DataDir); !os.IsNotExist(err) {
		t.Errorf("temp data dir %s left after a failed start: %v", cfg.DataDir, err)
	}
}
