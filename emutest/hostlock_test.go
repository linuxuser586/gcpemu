package emutest

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestHostLockExcludes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lock")
	release, err := acquireHostLock(path, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := acquireHostLock(path, time.Now().Add(300*time.Millisecond)); err == nil {
		t.Fatal("second holder acquired a held lock")
	}
	got := make(chan error, 1)
	go func() {
		r, err := acquireHostLock(path, time.Now().Add(10*time.Second))
		if err == nil {
			r()
		}
		got <- err
	}()
	select {
	case err := <-got:
		t.Fatalf("waiter returned while the lock was held: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	release()
	select {
	case err := <-got:
		if err != nil {
			t.Fatalf("waiter after release: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waiter did not get the lock after release")
	}
}

// TestHostLockHolderDies is the lock passing on when the process holding
// it is killed.
func TestHostLockHolderDies(t *testing.T) {
	if p := os.Getenv("GCPEMU_TEST_HOSTLOCK_HOLD"); p != "" {
		if _, err := acquireHostLock(p, time.Time{}); err != nil {
			os.Exit(2)
		}
		_ = os.WriteFile(p+".held", nil, 0o600)
		time.Sleep(time.Minute)
		os.Exit(0)
	}
	path := filepath.Join(t.TempDir(), "lock")
	cmd := exec.Command(os.Args[0], "-test.run=^TestHostLockHolderDies$")
	cmd.Env = append(os.Environ(), "GCPEMU_TEST_HOSTLOCK_HOLD="+path)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		if _, err := os.Stat(path + ".held"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("holder process did not take the lock")
		}
	}
	if _, err := acquireHostLock(path, time.Now().Add(300*time.Millisecond)); err == nil {
		t.Fatal("acquired a lock another process holds")
	}
	_ = cmd.Process.Kill()
	release, err := acquireHostLock(path, time.Now().Add(10*time.Second))
	if err != nil {
		t.Fatalf("after the holder died: %v", err)
	}
	release()
}

// TestLockSlots is a lock over n files letting n holders in at once and
// the next one in when any of them releases.
func TestLockSlots(t *testing.T) {
	dir := t.TempDir()
	paths := []string{filepath.Join(dir, "0"), filepath.Join(dir, "1")}
	first, err := acquireLock(paths, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	second, err := acquireLock(paths, time.Now().Add(time.Second))
	if err != nil {
		t.Fatalf("second holder of two slots: %v", err)
	}
	defer second()
	if _, err := acquireLock(paths, time.Now().Add(300*time.Millisecond)); !errors.Is(err, errLockHeld) {
		t.Fatalf("third holder of two slots: err = %v, want errLockHeld", err)
	}
	got := make(chan error, 1)
	go func() {
		r, err := acquireLock(paths, time.Now().Add(10*time.Second))
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
