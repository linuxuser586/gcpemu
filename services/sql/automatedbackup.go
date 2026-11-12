package sql

import (
	"context"
	"errors"
	"os"
	"time"

	sqladmin "google.golang.org/api/sqladmin/v1beta4"

	"github.com/linuxuser586/gcpemu/internal/store"
)

// Automated backups (FR-SQL-008, ADR 0069, #1038). A Cloud SQL instance
// that is running with backupConfiguration.enabled takes an Automated
// backup as soon as the emulator clock passes its startTime ("HH:MM", UTC)
// each day: the same dump, and Base backup with point-in-time recovery, as
// an on-demand backup, run as a BACKUP_VOLUME Operation. The backup sweep
// and ClockAdvanced look for due slots.
//
// Which daily slot was last handled is recorded on the instance
// (instanceRecord.BackupSlot), never worked out from the backups that
// exist, which retention and users delete. A slot is handled once its
// backup was attempted. An instance that finds its latest slot unhandled,
// however many slots it missed, takes one backup for it. One stopped, not
// RUNNABLE or in another Operation leaves the slot for a later
// sweep. Creating an instance, enabling its backups or changing their
// start time marks the current slot handled, and so does the first sweep
// that finds a record stored before #1038.

// lastBackupSlot is the latest daily Automated backup slot at or before now:
// startTime ("HH:MM", UTC) on now's UTC day, or the day before. ok is false
// for a startTime that is not one.
func lastBackupSlot(now time.Time, startTime string) (slot time.Time, ok bool) {
	t, err := time.Parse("15:04", startTime)
	if err != nil {
		return time.Time{}, false
	}
	now = now.UTC()
	slot = time.Date(now.Year(), now.Month(), now.Day(), t.Hour(), t.Minute(), 0, 0, time.UTC)
	if slot.After(now) {
		slot = slot.AddDate(0, 0, -1)
	}
	return slot, true
}

// backupStartTime is the instance's backupConfiguration.startTime.
func backupStartTime(st *sqladmin.Settings) string {
	if st == nil || st.BackupConfiguration == nil {
		return ""
	}
	return st.BackupConfiguration.StartTime
}

// backupsEnabled reports backupConfiguration.enabled.
func backupsEnabled(st *sqladmin.Settings) bool {
	return st != nil && st.BackupConfiguration != nil && st.BackupConfiguration.Enabled
}

// currentSlot is the BackupSlot that marks the slot current at now handled
// for settings st; "" for a startTime that is not one.
func currentSlot(st *sqladmin.Settings, now time.Time) string {
	slot, ok := lastBackupSlot(now, backupStartTime(st))
	if !ok {
		return ""
	}
	return slot.Format(time.RFC3339)
}

// dueSlot is the slot the instance's Automated backup is due for at now.
// ok is false when none is: backups are off, the instance is not running,
// or its latest slot is handled.
func dueSlot(rec *instanceRecord, now time.Time) (slot time.Time, ok bool) {
	in := rec.Instance
	st := in.Settings
	if !backupsEnabled(st) || st.ActivationPolicy == "NEVER" || in.State != "RUNNABLE" {
		return time.Time{}, false
	}
	slot, ok = lastBackupSlot(now, backupStartTime(st))
	if !ok {
		return time.Time{}, false
	}
	handled, err := time.Parse(time.RFC3339, rec.BackupSlot)
	if err != nil || !handled.Before(slot) {
		return time.Time{}, false
	}
	return slot, true
}

// startDueAutomatedBackups starts the Automated backups due now on the
// background generation whose context is gen. It starts none once gen is
// done: Reset or Stop then owns the state the sweep read.
func (s *Service) startDueAutomatedBackups(gen context.Context) {
	now := s.env.Clock.Now()
	var due, legacy []string
	_ = s.env.Store.View(func(tx store.Tx) error {
		scanJSON(tx, nsInstances, "", func(k string, rec *instanceRecord) {
			switch {
			case rec.Instance == nil:
			case rec.BackupSlot == "":
				legacy = append(legacy, k)
			default:
				if _, ok := dueSlot(rec, now); ok {
					due = append(due, k)
				}
			}
		})
		return nil
	})
	for _, k := range legacy {
		project, name := splitKey(k)
		_ = s.updateRecord(project, name, func(rec *instanceRecord) error {
			if rec.BackupSlot == "" {
				rec.BackupSlot = currentSlot(rec.Instance.Settings, now)
			}
			return nil
		})
	}
	for _, k := range due {
		project, name := splitKey(k)
		s.startAutomatedBackup(gen, project, name, now)
	}
}

// errNotDue ends a transaction that finds no Automated backup due.
var errNotDue = errors.New("no Automated backup due")

// startAutomatedBackup takes the instance's Operation lock, marks its due
// slot handled, stores a RUNNING Automated backup and starts the Operation
// that fills it. It does nothing when another Operation holds the
// Operation lock, no backup is due any more or gen is done.
func (s *Service) startAutomatedBackup(gen context.Context, project, name string, now time.Time) {
	op := s.newOp(system(context.Background()), project, name, "BACKUP_VOLUME")
	if !s.tryAcquire(instKey(project, name), op.Name) {
		return
	}
	b := &backupRecord{}
	err := s.env.Store.Update(func(tx store.Tx) error {
		rec, ok := getInstance(tx, project, name)
		if !ok || gen.Err() != nil {
			return errNotDue
		}
		slot, ok := dueSlot(rec, now)
		if !ok {
			return errNotDue
		}
		rec.BackupSlot = slot.Format(time.RFC3339)
		b.Run = s.newBackupRun(rec, "AUTOMATED", "")
		b.Run.WindowStartTime = slot.Format(time.RFC3339Nano)
		if err := s.putNewBackup(tx, project, b); err != nil {
			return err
		}
		return store.PutJSON(tx, nsInstances, instKey(project, name), rec)
	})
	if err != nil {
		s.releaseOp(op)
		if !errors.Is(err, errNotDue) {
			s.env.Log.Warn("sql: starting an Automated backup failed", "instance", project+":"+name, "err", err)
		}
		return
	}
	if gen.Err() != nil {
		// Stopped or reset since the write: the backup will not run.
		s.releaseOp(op)
		_ = s.updateBackupRecord(project, name, b.Run.Id, func(b *backupRecord) { s.fail(b, errStopped) })
		return
	}
	s.runBackup(op, project, name, b.Run.Id, true)
}

// runningAutomated returns the Operations of the Automated backups still
// being taken, whichever sweep started them.
func (s *Service) runningAutomated() []*sqladmin.Operation {
	var ops []*sqladmin.Operation
	_ = s.env.Store.View(func(tx store.Tx) error {
		scanJSON(tx, nsOps, "", func(_ string, op *sqladmin.Operation) {
			if op.Status == "DONE" || op.OperationType != "BACKUP_VOLUME" || op.BackupContext == nil {
				return
			}
			if b, ok := s.findBackupIn(tx, op.TargetProject, op.BackupContext.BackupId); ok && b.Run.Type == "AUTOMATED" {
				ops = append(ops, op)
			}
		})
		return nil
	})
	return ops
}

// awaitOps waits until every op is DONE or gone (a Reset clears the store
// after stopping its work), or ctx ends.
func (s *Service) awaitOps(ctx context.Context, ops []*sqladmin.Operation) error {
	for _, op := range ops {
		for {
			cur, ok := s.loadOp(op.TargetProject, op.Name)
			if !ok || cur.Status == "DONE" {
				break
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(10 * time.Millisecond):
			}
		}
	}
	return nil
}

// retainedBackups is backupRetentionSettings.retainedBackups, 0 when unset.
func retainedBackups(st *sqladmin.Settings) int64 {
	if st == nil || st.BackupConfiguration == nil || st.BackupConfiguration.BackupRetentionSettings == nil {
		return 0
	}
	return st.BackupConfiguration.BackupRetentionSettings.RetainedBackups
}

// pruneAutomated applies the instance's backup retention: it keeps the
// newest retainedBackups successful Automated backups and deletes, with
// their dumps, the older ones and the FAILED Automated backup runs older
// than the oldest kept. Other backups, and the Base backups that
// transaction log retention keeps, are not touched. A backup a restore is
// about to read is kept until the next pruning.
func (s *Service) pruneAutomated(project, name string) error {
	rec, err := s.loadRecord(project, name)
	if err != nil {
		return err
	}
	keep := retainedBackups(rec.Instance.Settings)
	if keep <= 0 {
		return nil
	}
	// Keys sort newest first (backupKey).
	var auto []*backupRecord
	_ = s.env.Store.View(func(tx store.Tx) error {
		scanJSON(tx, nsBackups, project+"/"+name+"/", func(_ string, b *backupRecord) {
			if b.Run.Type == "AUTOMATED" {
				auto = append(auto, b)
			}
		})
		return nil
	})
	var drop []*backupRecord
	kept, oldestKept := int64(0), int64(-1)
	for _, b := range auto {
		if b.Run.Status != "SUCCESSFUL" {
			continue
		}
		if kept < keep {
			kept++
			oldestKept = b.Run.Id
		} else {
			drop = append(drop, b)
		}
	}
	for _, b := range auto {
		if b.Run.Status == "FAILED" && oldestKept >= 0 && b.Run.Id < oldestKept {
			drop = append(drop, b)
		}
	}
	for _, b := range drop {
		k, deleted := b.key(project), false
		if err := s.env.Store.Update(func(tx store.Tx) error {
			if s.held(k) {
				return nil
			}
			deleted = true
			return tx.Delete(nsBackups, k)
		}); err != nil {
			return err
		}
		if deleted && b.File != "" {
			_ = os.Remove(b.File)
		}
	}
	return nil
}
