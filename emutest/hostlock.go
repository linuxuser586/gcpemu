package emutest

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/linuxuser586/gcpemu/internal/filelock"
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

// acquireHostLock takes an exclusive OS lock on path, which the OS
// releases if the process dies, polling until deadline (zero: forever).
func acquireHostLock(path string, deadline time.Time) (release func(), err error) {
	release, err = filelock.Acquire([]string{path}, deadline)
	if errors.Is(err, filelock.ErrHeld) {
		return nil, fmt.Errorf("another test still serves Cloud SQL host clients on 127.0.0.1 (lock %s); run `%s` to give each its own address", path, loopback.InstallHint)
	}
	return release, err
}
