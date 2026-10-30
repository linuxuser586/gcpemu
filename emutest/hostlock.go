package emutest

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/linuxuser586/gcpemu/internal/loopback"
)

// hostLockFile is shared by every test binary of the user (it lives in
// their temporary directory): without loopback aliases, the Cloud SQL
// instances of all of them serve host clients on the one address
// 127.0.0.1 (ADR 0005, #316).
var hostLockFile = filepath.Join(os.TempDir(), "gcpemu-test-host-loopback.lock")

// holdHostLoopback takes the host's loopback lock when the host has no
// loopback aliases, waiting for other tests (in any process) that hold it
// until deadline (zero: forever). release drops it; it is nil when no lock
// was needed.
func holdHostLoopback(deadline time.Time) (release func(), err error) {
	if loopback.Aliases() {
		return nil, nil
	}
	return acquireHostLock(hostLockFile, deadline)
}

// errLockHeld is a lock that another holder has.
var errLockHeld = errors.New("still held")

// acquireHostLock takes an exclusive OS lock on path, which the OS
// releases if the process dies, polling until deadline (zero: forever).
func acquireHostLock(path string, deadline time.Time) (release func(), err error) {
	release, err = acquireLock([]string{path}, deadline)
	if errors.Is(err, errLockHeld) {
		return nil, fmt.Errorf("another test still serves Cloud SQL host clients on 127.0.0.1 (lock %s); run `%s` to give each its own address", path, loopback.InstallHint)
	}
	return release, err
}

// acquireLock takes an exclusive OS lock on whichever of paths is free
// first, so that len(paths) holders get in at once. The OS releases it if
// the process dies. It polls until deadline (zero: forever), then fails
// with errLockHeld. The caller keeps release reachable until it calls it:
// a dropped release lets the collector close the file, dropping the lock.
func acquireLock(paths []string, deadline time.Time) (release func(), err error) {
	for {
		for _, p := range paths {
			release, err := lockNow(p, os.O_CREATE)
			if !errors.Is(err, errLockHeld) {
				return release, err
			}
		}
		if !deadline.IsZero() && time.Now().After(deadline) {
			return nil, errLockHeld
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// lockNow takes an exclusive OS lock on path without waiting, opening it
// with the extra flags (os.O_CREATE to create it). It fails with
// errLockHeld while another holder has it, and when path was removed or
// replaced before the lock was taken, since a lock on the old file guards
// nothing.
func lockNow(path string, flag int) (release func(), err error) {
	f, err := os.OpenFile(path, os.O_RDWR|flag, 0o600)
	if err != nil {
		return nil, err
	}
	ok, err := tryLock(f)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("locking %s: %w", path, err)
	}
	release = func() { _ = unlock(f); _ = f.Close() }
	if !ok {
		f.Close()
		return nil, errLockHeld
	}
	locked, err := f.Stat()
	now, serr := os.Stat(path)
	if err != nil || serr != nil || !os.SameFile(locked, now) {
		release()
		return nil, errLockHeld
	}
	return release, nil
}
