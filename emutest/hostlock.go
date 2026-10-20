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

// errLockHeld is acquireLock reaching its deadline with every file held.
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
// with errLockHeld.
func acquireLock(paths []string, deadline time.Time) (release func(), err error) {
	files := make([]*os.File, 0, len(paths))
	defer func() {
		for _, f := range files {
			f.Close()
		}
	}()
	for _, p := range paths {
		f, err := os.OpenFile(p, os.O_RDWR|os.O_CREATE, 0o600)
		if err != nil {
			return nil, err
		}
		files = append(files, f)
	}
	for {
		for i, f := range files {
			ok, err := tryLock(f)
			if err != nil {
				return nil, fmt.Errorf("locking %s: %w", paths[i], err)
			}
			if ok {
				files = append(files[:i], files[i+1:]...) // kept open: closing drops the lock
				return func() { _ = unlock(f); _ = f.Close() }, nil
			}
		}
		if !deadline.IsZero() && time.Now().After(deadline) {
			return nil, errLockHeld
		}
		time.Sleep(100 * time.Millisecond)
	}
}
