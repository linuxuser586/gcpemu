package sql

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"

	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// State snapshots (FR-CORE-012) keep each instance's data as a pg_dumpall,
// like a backup run, under dumps/<project>/<name>.sql, and the backup
// files under backups/. Restoring starts each instance on a fresh volume
// and replays its dump, so a snapshot does not depend on the runtime's
// volumes and restores into another Instance too.

var _ emu.Snapshotter = (*Service)(nil)

func (s *Service) instanceRecords() []*instanceRecord {
	var recs []*instanceRecord
	_ = s.env.Store.View(func(tx store.Tx) error {
		scanJSON(tx, nsInstances, "", func(_ string, rec *instanceRecord) {
			if rec.Instance != nil {
				recs = append(recs, rec)
			}
		})
		return nil
	})
	return recs
}

// hasData reports whether an instance's container was ever started, so
// that it has a Postgres cluster worth keeping.
func hasData(rec *instanceRecord) bool {
	in := rec.Instance
	return in.State != "PENDING_CREATE" && rec.HostPort != 0
}

func stopped(rec *instanceRecord) bool {
	in := rec.Instance
	return in.Settings != nil && in.Settings.ActivationPolicy == "NEVER"
}

func dumpPath(dir, project, name string) string {
	return filepath.Join(dir, "dumps", project, name+".sql")
}

// Snapshot implements emu.Snapshotter. A stopped instance's data cannot be
// dumped, so it must be started or deleted first.
func (s *Service) Snapshot(ctx context.Context, dir string) error {
	for _, rec := range s.instanceRecords() {
		if !hasData(rec) {
			continue
		}
		p, n := rec.Instance.Project, rec.Instance.Name
		if stopped(rec) {
			return fmt.Errorf("Cloud SQL instance %s:%s is stopped; start it (activationPolicy ALWAYS) or delete it to save a State snapshot", p, n)
		}
		mu := s.lock(p, n)
		dump, err := s.dumpAll(ctx, p, n)
		mu.Unlock()
		if err != nil {
			return fmt.Errorf("dump %s:%s: %w", p, n, err)
		}
		f := dumpPath(dir, p, n)
		if err := os.MkdirAll(filepath.Dir(f), 0o700); err != nil {
			return err
		}
		if err := writeFileAtomic(f, []byte(dump)); err != nil {
			return err
		}
	}
	d, err := s.env.ServiceDir("sql")
	if err != nil {
		return err
	}
	return emu.LinkTree(filepath.Join(d, "backups"), filepath.Join(dir, "backups"))
}

// Restore implements emu.Snapshotter: after Reset removed every container
// and volume, each instance with data is started on a fresh volume and its
// dump replayed, in parallel.
func (s *Service) Restore(ctx context.Context, dir string) error {
	d, err := s.env.ServiceDir("sql")
	if err != nil {
		return err
	}
	if err := emu.LinkTree(filepath.Join(dir, "backups"), filepath.Join(d, "backups")); err != nil {
		return err
	}
	s.failInterruptedOps()
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
	)
	for _, rec := range s.instanceRecords() {
		if !hasData(rec) || stopped(rec) {
			continue
		}
		p, n := rec.Instance.Project, rec.Instance.Name
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.restoreInstance(ctx, dir, p, n); err != nil {
				_ = s.setState(p, n, "FAILED")
				mu.Lock()
				errs = append(errs, fmt.Errorf("%s:%s: %w", p, n, err))
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	return errors.Join(errs...)
}

func (s *Service) restoreInstance(ctx context.Context, dir, project, name string) error {
	mu := s.lock(project, name)
	defer mu.Unlock()
	state := "RUNNABLE"
	if rec, err := s.loadRecord(project, name); err == nil {
		state = rec.Instance.State
	}
	_ = s.setState(project, name, "MAINTENANCE")
	if err := s.startContainer(ctx, project, name); err != nil {
		return err
	}
	dump, err := os.ReadFile(dumpPath(dir, project, name))
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return err
	default:
		if err := s.loadDump(ctx, project, name, string(dump)); err != nil {
			return err
		}
	}
	// The fresh volume was bootstrapped with the record's settings.
	if err := s.updateRecord(project, name, func(r *instanceRecord) error {
		r.AppliedFlags = serverFlags(r.Instance.Settings)
		return nil
	}); err != nil {
		return err
	}
	if err := s.ensureBaseBackup(ctx, project, name); err != nil {
		return err
	}
	return s.setState(project, name, state)
}
