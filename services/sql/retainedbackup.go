package sql

import (
	"errors"
	"os"
	"strings"

	sqladmin "google.golang.org/api/sqladmin/v1beta4"

	"github.com/linuxuser586/gcpemu/internal/store"
)

// Backups of deleted Cloud SQL instances (FR-SQL-008): Final backups
// (finalbackup.go, #407) and Retained backups (#366). With
// settings.retainBackupsOnDelete, instances.delete keeps every on-demand
// backup of the instance, whatever its state, as a Retained backup: it
// keeps its type, ID, name, description, state and expiry, and gains the
// instanceDeletionTime and instanceSettings a Final backup has.
//
// A backup of a deleted instance is stored under "P/I@deleted/backupKey"
// and its dump under backups/P/I@deleted. Instance names cannot contain
// "@", so neither the instance's teardown nor a later Cloud SQL instance
// of the same name sees it as one of its backup runs, and backup IDs are
// unique in the project, so the backups of successive instances of one
// name share the segment without colliding. Its permissions are still
// checked on the instance's resource name.

// deletedSuffix marks the key segment of a deleted instance's backups.
const deletedSuffix = "@deleted"

// legacyFinalSuffix is the segment v0.9.0 stored Final backups under;
// migrateDeletedOwners moves them to deletedSuffix.
const legacyFinalSuffix = "@final"

// owner is the key segment the backup is stored under: its instance's
// name, or for a backup of a deleted instance that name + deletedSuffix.
func (b *backupRecord) owner() string {
	if b.InstanceDeletionTime != "" {
		return b.Run.Instance + deletedSuffix
	}
	return b.Run.Instance
}

// errDeletedMidBackup fails an on-demand backup still running when its
// instance's delete keeps it.
var errDeletedMidBackup = errors.New("the Cloud SQL instance was deleted before the backup finished")

// retainBackups makes every on-demand backup of the Cloud SQL instance a
// Retained backup of in, deleted at deletedAt, and moves its dump out of
// the instance's backup directory. The delete runs after the instance's
// other operations, so a backup run is normally finished; one that is not
// yet is marked FAILED.
func (s *Service) retainBackups(in *sqladmin.DatabaseInstance, deletedAt string) error {
	project, name := in.Project, in.Name
	var recs []*backupRecord
	_ = s.env.Store.View(func(tx store.Tx) error {
		scanJSON(tx, nsBackups, project+"/"+name+"/", func(_ string, b *backupRecord) { recs = append(recs, b) })
		return nil
	})
	// The dumps move first: a crash before the records do leaves a file
	// nothing names, not a backup without its dump.
	for _, b := range recs {
		if b.Run.Status == "RUNNING" || b.Run.Status == "ENQUEUED" {
			s.fail(b, errDeletedMidBackup)
		}
		b.InstanceDeletionTime, b.InstanceSettings = deletedAt, in
		if b.File == "" {
			continue
		}
		file, err := s.backupFile(project, b.owner(), b.Run.Id)
		if err != nil {
			return err
		}
		if err := os.Rename(b.File, file); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		b.File = file
	}
	return s.env.Store.Update(func(tx store.Tx) error {
		for _, b := range recs {
			if err := tx.Delete(nsBackups, childKey(project, name, backupKey(b.Run.Id))); err != nil {
				return err
			}
			if err := store.PutJSON(tx, nsBackups, b.key(project), b); err != nil {
				return err
			}
		}
		return nil
	})
}

// migrateDeletedOwners moves Final backups stored by v0.9.0 under
// legacyFinalSuffix to deletedSuffix. Their dumps stay where their records
// say.
func (s *Service) migrateDeletedOwners() error {
	return s.env.Store.Update(func(tx store.Tx) error {
		moved := map[string]*backupRecord{}
		scanJSON(tx, nsBackups, "", func(k string, b *backupRecord) {
			if _, rest := splitKey(k); strings.Contains(rest, legacyFinalSuffix+"/") {
				moved[k] = b
			}
		})
		for k, b := range moved {
			project, _ := splitKey(k)
			if err := tx.Delete(nsBackups, k); err != nil {
				return err
			}
			if err := store.PutJSON(tx, nsBackups, b.key(project), b); err != nil {
				return err
			}
		}
		return nil
	})
}
