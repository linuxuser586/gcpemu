package emutest

import (
	"context"
	"errors"
	"os"
	"testing"
)

// TestLaunchFailureCleansUp: an Instance that fails to launch reports the
// error, calls release once, and removes the data dir it created (#863).
func TestLaunchFailureCleansUp(t *testing.T) {
	cfg := testConfig([]string{"no-such-service"}, nil)
	released := 0
	i, err := launch(context.Background(), &cfg, func() { released++ })
	if err == nil {
		i.Close(context.Background())
		t.Fatal("launch with an unknown service succeeded")
	}
	if released != 1 {
		t.Errorf("release called %d times, want 1", released)
	}
	if cfg.DataDir == "" {
		t.Fatal("launch created no data dir")
	}
	if _, err := os.Stat(cfg.DataDir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("data dir %s left behind: %v", cfg.DataDir, err)
	}
}

// TestNewReportsLaunchError: New returns a failed launch's error rather
// than panicking (#863).
func TestNewReportsLaunchError(t *testing.T) {
	i, err := New(context.Background(), []string{"no-such-service"})
	if err == nil {
		i.Close(context.Background())
		t.Fatal("New with an unknown service succeeded")
	}
}
