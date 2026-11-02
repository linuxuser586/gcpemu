// Package filelock takes exclusive OS locks on files, which the OS drops
// when the holding process dies. Test processes of every emutest version
// and the gcpemu CLI share them, so the locking scheme is a contract
// (ADR 0047).
package filelock

import (
	"errors"
	"fmt"
	"os"
	"time"
)

// ErrHeld is a lock that another holder has.
var ErrHeld = errors.New("still held")

// Acquire takes an exclusive OS lock on whichever of paths is free first,
// so that len(paths) holders get in at once. The OS releases it if the
// process dies. It polls until deadline (zero: forever), then fails with
// ErrHeld. The caller keeps release reachable until it calls it: a
// dropped release lets the collector close the file, dropping the lock.
func Acquire(paths []string, deadline time.Time) (release func(), err error) {
	for {
		for _, p := range paths {
			release, err := TryLock(p, os.O_CREATE)
			if !errors.Is(err, ErrHeld) {
				return release, err
			}
		}
		if !deadline.IsZero() && time.Now().After(deadline) {
			return nil, ErrHeld
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// TryLock takes an exclusive OS lock on path without waiting, opening it
// with the extra flags (os.O_CREATE to create it). It fails with ErrHeld
// while another holder has it, and when path was removed or replaced
// before the lock was taken, since a lock on the old file guards nothing.
func TryLock(path string, flag int) (release func(), err error) {
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
		return nil, ErrHeld
	}
	locked, err := f.Stat()
	now, serr := os.Stat(path)
	if err != nil || serr != nil || !os.SameFile(locked, now) {
		release()
		return nil, ErrHeld
	}
	return release, nil
}
