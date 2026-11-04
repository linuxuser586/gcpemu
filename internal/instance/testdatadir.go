package instance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/filelock"
	"github.com/linuxuser586/gcpemu/internal/gateway"
	"github.com/linuxuser586/gcpemu/internal/runtime"
)

// OwnerLockFile is the owner lock file in a Test data dir. The test that
// owns the dir holds an exclusive OS lock on it from before its Instance
// is created until the Instance's containers, networks and volumes are
// removed; the OS drops the lock if the test's process dies. So a dir
// whose lock can be taken belongs to a test that has ended, and
// ReclaimEnded reclaims it (ADRs 0047, 0057). The name and the locking are
// a contract between the test processes of every emutest version.
const OwnerLockFile = "gcpemu-test-owner.lock"

// ownerLockWait bounds how long HoldOwnerLock waits for a reclaim that is
// removing the dir to let go of its lock.
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

// HoldOwnerLock takes the owner lock of the Test data dir dir, an
// absolute path, creating dir if need be, or another reference to it when
// this process holds it already. It waits up to ownerLockWait for a
// reclaim that holds it, then fails: another test process owns dir.
// release drops the reference, and the lock with the last one.
func HoldOwnerLock(dir string) (release func(), err error) {
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
// deadline. A reclaim may remove dir while this waits, so dir is made
// again on each try.
func lockOwnerFile(dir string, deadline time.Time) (release func(), err error) {
	for {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
		release, err := filelock.TryLock(filepath.Join(dir, OwnerLockFile), os.O_CREATE)
		if !errors.Is(err, filelock.ErrHeld) {
			return release, err
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("Test data dir %s is owned by another test process", dir)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// lockOwnerNow takes the lock on dir's existing owner lock file without
// waiting, failing with filelock.ErrHeld while a test holds it.
func lockOwnerNow(dir string) (release func(), err error) {
	return filelock.TryLock(filepath.Join(dir, OwnerLockFile), 0)
}

// testEnded reports whether dir is a Test data dir whose test has ended:
// its owner lock file exists and nothing holds the lock.
func testEnded(dir string) bool {
	release, err := lockOwnerNow(dir)
	if err != nil {
		return false
	}
	release()
	return true
}

// Reclaim removes a's containers, networks and volumes, and its ended
// Test data dirs as ReclaimEnded does. It reports false when it removed
// nothing because a test owns the dirs again or another reclaim has them.
func (a Abandoned) Reclaim(ctx context.Context, cl *runtime.Client) (reclaimed bool, err error) {
	if !a.EndedTest() {
		return true, a.Prune(ctx, cl)
	}
	var errs []error
	for _, dir := range a.EndedTestDirs {
		ok, err := ReclaimEnded(ctx, cl, a.Tagged, dir)
		reclaimed = reclaimed || ok
		errs = append(errs, err)
	}
	return reclaimed, errors.Join(errs...)
}

// ReclaimEnded prunes a and removes dir if dir is a Test data dir holding
// a's ID whose test has ended, which taking its owner lock proves. An
// Instance process that still serves dir is stopped first. Any other dir
// is left alone, and reclaimed is false.
func ReclaimEnded(ctx context.Context, cl *runtime.Client, a Tagged, dir string) (reclaimed bool, err error) {
	if ReadID(dir) != a.ID {
		return false, nil
	}
	release, err := lockOwnerNow(dir)
	if errors.Is(err, filelock.ErrHeld) || errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	release = sync.OnceFunc(release)
	defer release()
	// Another reclaim may have reclaimed it before this one took the lock.
	if ReadID(dir) != a.ID {
		return false, nil
	}
	if stopProcess(ctx, dir, a.ID) {
		// The Instance may have changed its objects before it stopped.
		if a, err = taggedByID(ctx, cl, a.ID); err != nil {
			return false, err
		}
	}
	if err := a.Prune(ctx, cl); err != nil {
		return false, err // the dir still holds the ID, so a later reclaim tries again
	}
	// The ID goes first, so that no other reclaim takes the dir for this
	// Instance's. Windows cannot remove the open lock file, so the dir is
	// removed once more after the release.
	if err := os.Remove(filepath.Join(dir, IDFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	err = os.RemoveAll(dir)
	release()
	if err != nil {
		return true, os.RemoveAll(dir)
	}
	return true, nil
}

// ReclaimEndedDir reclaims dir as ReclaimEnded does, for the Instance ID
// that dir holds and whatever it tags on the runtime, so it also serves a
// dir that no object records, such as one whose Instance made none
// (#921). A dir that holds no ID is left alone.
func ReclaimEndedDir(ctx context.Context, cl *runtime.Client, dir string) (reclaimed bool, err error) {
	id := ReadID(dir)
	if id == "" {
		return false, nil
	}
	a, err := taggedByID(ctx, cl, id)
	if err != nil {
		return false, err
	}
	return ReclaimEnded(ctx, cl, a, dir)
}

// How long stopProcess waits for a process to exit once asked to stop,
// and once killed.
const (
	stopWait = 30 * time.Second
	killWait = 5 * time.Second
)

// stopProcess ends a process that still runs the Instance with ID id
// whose data dir is dir, and reports whether there was one. It asks the
// process to stop through dir's endpoints, as `gcpemu stop` does, once
// they serve that Instance. If it still runs, it kills it, but only if
// its executable is named gcpemu: a pid file that a crashed Instance left
// behind may name a reused pid (ADR 0057).
func stopProcess(ctx context.Context, dir, id string) bool {
	b, err := os.ReadFile(filepath.Join(dir, PIDFile))
	if err != nil {
		return false
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	if pid <= 0 || pid == os.Getpid() || !ProcessAlive(pid) {
		return false
	}
	if askDirShutdown(dir, id, pid) && exited(ctx, pid, stopWait) {
		return true
	}
	if exe, err := exePath(pid); err != nil || !isGcpemu(exe) {
		return true
	}
	if p, err := os.FindProcess(pid); err == nil {
		_ = p.Kill()
	}
	exited(ctx, pid, killWait)
	return true
}

// askDirShutdown asks the Instance on the gateway in dir's endpoints to shut
// down, with dir's Admin token, if it is the Instance id in process pid.
// It reports whether the Instance accepted.
func askDirShutdown(dir, id string, pid int) bool {
	var eps map[string]string
	b, err := os.ReadFile(filepath.Join(dir, EndpointsFile))
	if err != nil || json.Unmarshal(b, &eps) != nil || eps["gateway"] == "" {
		return false
	}
	token, _ := os.ReadFile(filepath.Join(dir, AdminTokenFile))
	c := &http.Client{Timeout: 10 * time.Second}
	_, err = AskShutdown(func(method, path string) (int, []byte, error) {
		req, err := http.NewRequest(method, "http://"+emu.Dialable(eps["gateway"])+path, nil)
		if err != nil {
			return 0, nil, err
		}
		if t := strings.TrimSpace(string(token)); t != "" {
			req.Header.Set(gateway.AdminTokenHeader, t)
		}
		resp, err := c.Do(req)
		if err != nil {
			return 0, nil, err
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		return resp.StatusCode, body, err
	}, id, pid)
	return err == nil
}

// exited waits up to wait for the process pid to exit, and reports
// whether it did.
func exited(ctx context.Context, pid int, wait time.Duration) bool {
	for deadline := time.Now().Add(wait); ProcessAlive(pid); time.Sleep(100 * time.Millisecond) {
		if ctx.Err() != nil || time.Now().After(deadline) {
			return false
		}
	}
	return true
}

// isGcpemu reports whether exe, an executable's path, is named gcpemu, in
// any case, with or without .exe.
func isGcpemu(exe string) bool {
	name := strings.ToLower(filepath.Base(filepath.FromSlash(exe)))
	return strings.TrimSuffix(name, ".exe") == "gcpemu"
}
