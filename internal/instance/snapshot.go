package instance

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/linuxuser586/gcpemu/internal/emu"
)

// State snapshots (FR-CORE-012) live under <instance dir>/snapshots/NAME:
// snapshot.json describes the snapshot, store.json holds the Store's
// contents, and services/<service> what each emu.Snapshotter saved. They
// sit beside the data directory, so an Ephemeral Instance's snapshots
// outlive it and a later Instance of the same name can restore them.

// SnapshotsDir is the directory under the instance dir holding State snapshots.
const SnapshotsDir = "snapshots"

const (
	snapshotManifest = "snapshot.json"
	snapshotStore    = "store.json"
	snapshotServices = "services"
	snapshotFormat   = 1
	// snapshotAttempts bounds how often SaveSnapshot retries when the
	// Store changed while services saved their state.
	snapshotAttempts = 3
)

var (
	// ErrSnapshotNotFound is returned for a State snapshot name that was never saved.
	ErrSnapshotNotFound = errors.New("State snapshot not found")
	// ErrSnapshotBusy is returned when the Instance cannot be snapshotted
	// right now: it is not ready, an Operation is running, or its state
	// kept changing while it was saved.
	ErrSnapshotBusy = errors.New("Instance busy")
	// ErrSnapshotName is returned for a name that is not a valid State snapshot name.
	ErrSnapshotName = errors.New("invalid State snapshot name")
)

var snapshotNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)

// SnapshotInfo describes a saved State snapshot.
type SnapshotInfo struct {
	Name string `json:"name"`
	// CreateTime is the host's wall-clock time of the save, not the Emulator clock.
	CreateTime time.Time `json:"createTime"`
	// Services are the Services the Instance ran when it was saved.
	Services []string `json:"services"`
	// SizeBytes is the size of the snapshot's files on disk.
	SizeBytes int64 `json:"sizeBytes"`
}

type snapshotManifestFile struct {
	Format     int       `json:"format"`
	Name       string    `json:"name"`
	CreateTime time.Time `json:"createTime"`
	Services   []string  `json:"services"`
}

func (in *Instance) snapshotsDir() string { return filepath.Join(in.Dir, SnapshotsDir) }

func checkSnapshotName(name string) error {
	if !snapshotNameRE.MatchString(name) {
		return fmt.Errorf("%w %q: use up to 63 letters, digits, '.', '_' or '-', starting with a letter or digit", ErrSnapshotName, name)
	}
	return nil
}

// SaveSnapshot saves all of the Instance's state as the State snapshot
// name, replacing one saved under that name before. The Instance must be
// ready with no Operation running, so that no resource is half made.
func (in *Instance) SaveSnapshot(ctx context.Context, name string) (*SnapshotInfo, error) {
	if err := checkSnapshotName(name); err != nil {
		return nil, err
	}
	in.stateMu.Lock()
	defer in.stateMu.Unlock()
	if err := in.quiescent(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(in.snapshotsDir(), 0o700); err != nil {
		return nil, err
	}
	tmp, err := os.MkdirTemp(in.snapshotsDir(), ".save-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	if err := in.saveInto(ctx, tmp); err != nil {
		return nil, err
	}
	mf := snapshotManifestFile{Format: snapshotFormat, Name: name, CreateTime: time.Now().UTC(), Services: in.serviceNames()}
	if err := writeJSONFile(filepath.Join(tmp, snapshotManifest), mf); err != nil {
		return nil, err
	}
	// Swap the new snapshot in, keeping the old one until it is in place.
	dst := filepath.Join(in.snapshotsDir(), name)
	old := ""
	if _, err := os.Stat(dst); err == nil {
		old = tmp + ".old"
		if err := os.Rename(dst, old); err != nil {
			return nil, err
		}
	}
	if err := os.Rename(tmp, dst); err != nil {
		if old != "" {
			_ = os.Rename(old, dst)
		}
		return nil, err
	}
	if old != "" {
		_ = os.RemoveAll(old)
	}
	return snapshotInfo(dst, mf), nil
}

// quiescent reports why the Instance cannot be snapshotted now, if it can't.
func (in *Instance) quiescent() error {
	var waiting []string
	for n, err := range in.Readiness() {
		if err != nil {
			waiting = append(waiting, fmt.Sprintf("%s: %v", n, err))
		}
	}
	if len(waiting) > 0 {
		sort.Strings(waiting)
		return fmt.Errorf("%w: not ready (%s)", ErrSnapshotBusy, strings.Join(waiting, "; "))
	}
	for _, op := range in.Operations("", "") {
		if !op.Done {
			return fmt.Errorf("%w: Operation %s of %s is running; wait for it to finish", ErrSnapshotBusy, op.Name, op.Service)
		}
	}
	return nil
}

// saveInto writes the Store and every Snapshotter's state into dir. The
// Store is dumped before and after the services save theirs; when the two
// differ a client changed something meanwhile, and the save starts again.
// Entries an emu.StoreCacher declares cache may change meanwhile.
func (in *Instance) saveInto(ctx context.Context, dir string) error {
	svcDir := filepath.Join(dir, snapshotServices)
	for range snapshotAttempts {
		before, err := in.Env.Store.Dump()
		if err != nil {
			return err
		}
		if err := os.RemoveAll(svcDir); err != nil {
			return err
		}
		for _, s := range in.services {
			sn, ok := s.(emu.Snapshotter)
			if !ok {
				continue
			}
			d := filepath.Join(svcDir, s.Name())
			if err := os.MkdirAll(d, 0o700); err != nil {
				return err
			}
			if err := sn.Snapshot(ctx, d); err != nil {
				return fmt.Errorf("%s: %w", s.Name(), err)
			}
		}
		after, err := in.Env.Store.Dump()
		if err != nil {
			return err
		}
		if sameDump(before, after, in.isStoreCache) {
			return writeJSONFile(filepath.Join(dir, snapshotStore), before)
		}
	}
	return fmt.Errorf("%w: its state kept changing while the State snapshot was saved; pause its clients and try again", ErrSnapshotBusy)
}

// isStoreCache reports whether a service declares key of ns a cache entry.
func (in *Instance) isStoreCache(ns, key string) bool {
	for _, s := range in.services {
		if c, ok := s.(emu.StoreCacher); ok && c.IsStoreCache(ns, key) {
			return true
		}
	}
	return false
}

// sameDump reports whether two Store dumps hold the same entries, leaving
// out the cache entries.
func sameDump(a, b map[string]map[string][]byte, isCache func(ns, key string) bool) bool {
	// An empty namespace and a missing one are the same state.
	for ns, kv := range a {
		for k := range kv {
			if _, ok := b[ns][k]; !ok && !isCache(ns, k) {
				return false
			}
		}
	}
	for ns, kv := range b {
		for k, v := range kv {
			if w, ok := a[ns][k]; (!ok || !bytes.Equal(v, w)) && !isCache(ns, k) {
				return false
			}
		}
	}
	return true
}

// RestoreSnapshot replaces all of the Instance's state with the State
// snapshot name: it resets the Instance, loads the snapshot's Store and
// lets each Snapshotter restore what it saved. Configuration, Fault rules
// and the Emulator clock, which never goes back, are kept.
func (in *Instance) RestoreSnapshot(ctx context.Context, name string) error {
	if err := checkSnapshotName(name); err != nil {
		return err
	}
	in.stateMu.Lock()
	defer in.stateMu.Unlock()
	dir := filepath.Join(in.snapshotsDir(), name)
	var mf snapshotManifestFile
	if err := readJSONFile(filepath.Join(dir, snapshotManifest), &mf); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("%w: %s", ErrSnapshotNotFound, name)
		}
		return err
	}
	if mf.Format != snapshotFormat {
		return fmt.Errorf("State snapshot %s has format %d; this gcpemu reads format %d", name, mf.Format, snapshotFormat)
	}
	var data map[string]map[string][]byte
	if err := readJSONFile(filepath.Join(dir, snapshotStore), &data); err != nil {
		return err
	}
	return in.gated(ctx, "restore", "restoring State snapshot "+name, func() error {
		if err := in.reset(ctx); err != nil {
			return fmt.Errorf("reset: %w", err)
		}
		if err := in.Env.Store.Load(data); err != nil {
			return err
		}
		var errs []error
		for _, s := range in.services {
			if sn, ok := s.(emu.Snapshotter); ok {
				if err := sn.Restore(ctx, filepath.Join(dir, snapshotServices, s.Name())); err != nil {
					errs = append(errs, fmt.Errorf("%s: %w", s.Name(), err))
				}
			}
		}
		return errors.Join(errs...)
	})
}

// Snapshots lists the saved State snapshots by name.
func (in *Instance) Snapshots() ([]SnapshotInfo, error) {
	ents, err := os.ReadDir(in.snapshotsDir())
	if errors.Is(err, fs.ErrNotExist) {
		return []SnapshotInfo{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := []SnapshotInfo{}
	for _, e := range ents {
		if !e.IsDir() || checkSnapshotName(e.Name()) != nil {
			continue
		}
		dir := filepath.Join(in.snapshotsDir(), e.Name())
		var mf snapshotManifestFile
		if readJSONFile(filepath.Join(dir, snapshotManifest), &mf) != nil {
			continue
		}
		mf.Name = e.Name()
		out = append(out, *snapshotInfo(dir, mf))
	}
	return out, nil
}

// DeleteSnapshot removes the State snapshot name.
func (in *Instance) DeleteSnapshot(name string) error {
	if err := checkSnapshotName(name); err != nil {
		return err
	}
	in.stateMu.Lock()
	defer in.stateMu.Unlock()
	dir := filepath.Join(in.snapshotsDir(), name)
	if _, err := os.Stat(filepath.Join(dir, snapshotManifest)); errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%w: %s", ErrSnapshotNotFound, name)
	}
	return os.RemoveAll(dir)
}

func (in *Instance) serviceNames() []string {
	out := make([]string, 0, len(in.services))
	for _, s := range in.services {
		out = append(out, s.Name())
	}
	sort.Strings(out)
	return out
}

func snapshotInfo(dir string, mf snapshotManifestFile) *SnapshotInfo {
	var size int64
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			if fi, err := d.Info(); err == nil {
				size += fi.Size()
			}
		}
		return nil
	})
	return &SnapshotInfo{Name: mf.Name, CreateTime: mf.CreateTime, Services: mf.Services, SizeBytes: size}
}

func writeJSONFile(path string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

func readJSONFile(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// snapshotStatus is the admin API's HTTP status for a State snapshot error.
func snapshotStatus(err error) int {
	switch {
	case errors.Is(err, ErrSnapshotName):
		return http.StatusBadRequest
	case errors.Is(err, ErrSnapshotNotFound):
		return http.StatusNotFound
	case errors.Is(err, ErrSnapshotBusy):
		return http.StatusConflict
	}
	return http.StatusInternalServerError
}
