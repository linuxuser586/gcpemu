package emutest

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/linuxuser586/gcpemu/internal/loopback"
)

// hostLockFile is shared by every test binary of the user (it lives in
// their temporary directory): without loopback aliases, the Cloud SQL
// instances of all of them serve host clients on the one address
// 127.0.0.1 (ADR 0005, #316).
var hostLockFile = filepath.Join(os.TempDir(), "gcpemu-test-host-loopback.lock")

// holdHostLoopback takes the host's loopback lock for the rest of the
// test when the host has no loopback aliases, waiting for other tests
// (in any process) that hold it, up to the test's deadline.
func holdHostLoopback(t testing.TB) {
	t.Helper()
	if loopback.Aliases() {
		return
	}
	var deadline time.Time
	if d, ok := t.(interface{ Deadline() (time.Time, bool) }); ok {
		deadline, _ = d.Deadline()
	}
	release, err := acquireHostLock(hostLockFile, deadline)
	if err != nil {
		t.Fatalf("emutest: %v", err)
	}
	t.Cleanup(release)
}

// acquireHostLock takes an exclusive OS lock on path, which the OS
// releases if the process dies, polling until deadline (zero: forever).
func acquireHostLock(path string, deadline time.Time) (release func(), err error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	for {
		ok, err := tryLock(f)
		if err != nil {
			f.Close()
			return nil, fmt.Errorf("locking %s: %w", path, err)
		}
		if ok {
			return func() { _ = unlock(f); _ = f.Close() }, nil
		}
		if !deadline.IsZero() && time.Now().After(deadline) {
			f.Close()
			return nil, fmt.Errorf("another test still serves Cloud SQL host clients on 127.0.0.1 (lock %s); run `%s` to give each its own address", path, loopback.InstallHint)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
