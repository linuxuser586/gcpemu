package filelock

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// TestAcquireSlots is a lock over n files letting n holders in at once and
// the next one in when any of them releases.
func TestAcquireSlots(t *testing.T) {
	dir := t.TempDir()
	paths := []string{filepath.Join(dir, "0"), filepath.Join(dir, "1")}
	first, err := Acquire(paths, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	second, err := Acquire(paths, time.Now().Add(time.Second))
	if err != nil {
		t.Fatalf("second holder of two slots: %v", err)
	}
	defer second()
	if _, err := Acquire(paths, time.Now().Add(300*time.Millisecond)); !errors.Is(err, ErrHeld) {
		t.Fatalf("third holder of two slots: err = %v, want ErrHeld", err)
	}
	got := make(chan error, 1)
	go func() {
		r, err := Acquire(paths, time.Now().Add(10*time.Second))
		if err == nil {
			r()
		}
		got <- err
	}()
	select {
	case err := <-got:
		t.Fatalf("waiter returned while both slots were held: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	first()
	select {
	case err := <-got:
		if err != nil {
			t.Fatalf("waiter after a release: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waiter did not get the freed slot")
	}
}
