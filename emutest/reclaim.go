package emutest

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/instance"
	"github.com/linuxuser586/gcpemu/internal/runtime"
)

// ownerLockFile is the owner lock file in a Test data dir. The test that
// owns the dir holds an exclusive OS lock on it from before its Instance
// is created until the Instance's containers, networks and volumes are
// removed; the OS drops the lock if the test's process dies. So a dir
// whose lock can be taken belongs to a test that has ended, and the sweep
// reclaims it (ADR 0047). The name and the locking are a contract between
// the test processes of every emutest version.
const ownerLockFile = "gcpemu-test-owner.lock"

// ownerLockWait bounds how long holdOwnerLock waits for a sweep that is
// reclaiming the dir to let go of its lock.
const ownerLockWait = 30 * time.Second

// owners holds the owner locks this process has taken, by dir, so that
// the tests of one process that share a dir (a test and its subtests)
// share the lock too.
var (
	ownersMu sync.Mutex
	owners   = map[string]*owner{}
)

// owner is an owner lock this process holds, with the number of holders
// in the process.
type owner struct {
	refs    int
	release func()
}

// holdOwnerLock takes the owner lock of the Test data dir dir, an
// absolute path, creating dir if need be, or another reference to it when
// this process holds it already. It waits up to ownerLockWait for a sweep
// that holds it, then fails: another test process owns dir. release drops
// the reference, and the lock with the last one.
func holdOwnerLock(dir string) (release func(), err error) {
	ownersMu.Lock()
	defer ownersMu.Unlock()
	o := owners[dir]
	if o == nil {
		lock, err := lockOwnerFile(dir, time.Now().Add(ownerLockWait))
		if err != nil {
			return nil, err
		}
		o = &owner{release: lock}
		owners[dir] = o
	}
	o.refs++
	var once sync.Once
	return func() {
		once.Do(func() {
			ownersMu.Lock()
			defer ownersMu.Unlock()
			if o.refs--; o.refs == 0 {
				delete(owners, dir)
				o.release()
			}
		})
	}, nil
}

// lockOwnerFile takes the lock on dir's owner lock file, polling until
// deadline. A sweep may remove dir while this waits, so dir is made again
// on each try.
func lockOwnerFile(dir string, deadline time.Time) (release func(), err error) {
	for {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
		release, err := lockNow(filepath.Join(dir, ownerLockFile), os.O_CREATE)
		if !errors.Is(err, errLockHeld) {
			return release, err
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("Test data dir %s is owned by another test process", dir)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// logFunc reports a sweep's failures, as testing.TB's Logf does.
type logFunc func(format string, args ...any)

// sweeper reclaims, once, the Instances of test processes that died.
type sweeper struct {
	once   sync.Once
	detect func() (*runtime.Client, error)
}

// processSweep runs before the first Instance this process starts.
var processSweep = &sweeper{detect: runtime.Detect}

// run sweeps the first time it is called, if there is a runtime, logging
// failures with logf.
func (s *sweeper) run(logf logFunc) {
	s.once.Do(func() {
		cl, err := s.detect()
		if err != nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		sweep(ctx, cl, logf)
	})
}

// sweep reclaims the Instances of tests that have ended: those whose Test
// data dir holds their ID and has an owner lock file it can lock, and the
// Abandoned Instance IDs whose data dirs were all in the OS temp dir. It
// logs each failure with logf.
func sweep(ctx context.Context, cl *runtime.Client, logf logFunc) {
	tagged, err := instance.FindTagged(ctx, cl)
	if err != nil {
		logf("emutest: reclaiming the Instances of ended tests: %v", err)
		return
	}
	for _, a := range tagged {
		for _, dir := range a.Dirs {
			if err := reclaimEnded(ctx, cl, a, dir); err != nil {
				logf("emutest: reclaiming Instance %s of an ended test: %v", a.ID, err)
			}
		}
	}
	abandoned, err := instance.FindAbandoned(ctx, cl, config.StateHome())
	if err != nil {
		logf("emutest: reclaiming the Instances of ended tests: %v", err)
		return
	}
	for _, a := range abandoned {
		if a.Unverified || !allInTempDir(a.Dirs) {
			continue
		}
		if err := a.Prune(ctx, cl); err != nil {
			logf("emutest: reclaiming Abandoned Instance %s: %v", a.ID, err)
		}
	}
}

// reclaimEnded prunes a and removes dir if dir is a Test data dir holding
// a's ID whose test has ended, which taking its owner lock proves. Any
// other dir is left alone.
func reclaimEnded(ctx context.Context, cl *runtime.Client, a instance.Tagged, dir string) error {
	if heldID(dir) != a.ID {
		return nil
	}
	release, err := lockNow(filepath.Join(dir, ownerLockFile), 0)
	if errors.Is(err, errLockHeld) || errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	release = sync.OnceFunc(release)
	defer release()
	// Another sweep may have reclaimed it before this one took the lock.
	if heldID(dir) != a.ID {
		return nil
	}
	if err := a.Prune(ctx, cl); err != nil {
		return err // the dir still holds the ID, so a later sweep tries again
	}
	// The ID goes first, so that no other sweep takes the dir for this
	// Instance's. Windows cannot remove the open lock file, so the dir is
	// removed once more after the release.
	if err := os.Remove(filepath.Join(dir, instance.IDFile)); err != nil {
		return err
	}
	err = os.RemoveAll(dir)
	release()
	if err != nil {
		return os.RemoveAll(dir)
	}
	return nil
}

// heldID returns the Instance ID that dir holds, or "".
func heldID(dir string) string {
	b, _ := os.ReadFile(filepath.Join(dir, instance.IDFile))
	return string(b)
}

// allInTempDir reports whether dirs, which may no longer exist, are all
// inside the OS temp dir, symlinks resolved (on macOS /var is
// /private/var).
func allInTempDir(dirs []string) bool {
	tmp := resolveExisting(os.TempDir())
	for _, d := range dirs {
		rel, err := filepath.Rel(tmp, resolveExisting(d))
		if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return false
		}
	}
	return len(dirs) > 0
}

// resolveExisting returns the absolute path p with the symlinks of its
// longest existing ancestor resolved.
func resolveExisting(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	rest := ""
	for q := p; ; q = filepath.Dir(q) {
		if r, err := filepath.EvalSymlinks(q); err == nil {
			return filepath.Join(r, rest)
		}
		if filepath.Dir(q) == q {
			return p
		}
		rest = filepath.Join(filepath.Base(q), rest)
	}
}
