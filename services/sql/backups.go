package sql

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	sqladmin "google.golang.org/api/sqladmin/v1beta4"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/runtime"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// Backups, restore and clone (FR-SQL-008), implemented with pg_dumpall:
// a backup run stores a logical dump of the whole cluster (roles and all
// databases) under the service's data directory together with the API's
// users and databases; restore re-initialises the target's volume and
// replays the dump; clone creates a new instance and loads a dump of the
// source into it. The backups API (backupsapi.go) serves the same stored
// backups by name. Point-in-time clones replay archived WAL (pitr.go).

// backupDir returns the directory holding an instance's backup files.
func (s *Service) backupDir(project, name string) (string, error) {
	d, err := s.env.ServiceDir("sql")
	if err != nil {
		return "", err
	}
	d = filepath.Join(d, "backups", project, name)
	return d, os.MkdirAll(d, 0o700)
}

func (s *Service) removeBackupFiles(project, name string) {
	if d, err := s.env.ServiceDir("sql"); err == nil {
		_ = os.RemoveAll(filepath.Join(d, "backups", project, name))
	}
}

func (s *Service) removeAllBackupFiles() {
	if d, err := s.env.ServiceDir("sql"); err == nil {
		_ = os.RemoveAll(filepath.Join(d, "backups"))
	}
}

// dumpAll returns a pg_dumpall of the instance.
func (s *Service) dumpAll(ctx context.Context, project, name string) (string, error) {
	return s.execIn(ctx, project, name, "", "pg_dumpall", "-h", "/var/run/postgresql", "-p", "5433", "-U", superuser, "--no-password")
}

// writeFileAtomic writes temp + fsync + rename (NFR-REL-001).
func writeFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// snapshotAPIState returns the stored databases and users of an instance.
func (s *Service) snapshotAPIState(project, name string) ([]*sqladmin.Database, []*userRecord) {
	var dbs []*sqladmin.Database
	var users []*userRecord
	_ = s.env.Store.View(func(tx store.Tx) error {
		prefix := project + "/" + name + "/"
		scanJSON(tx, nsDatabases, prefix, func(_ string, d *sqladmin.Database) { dbs = append(dbs, d) })
		scanJSON(tx, nsUsers, prefix, func(_ string, u *userRecord) { users = append(users, u) })
		return nil
	})
	return dbs, users
}

// replaceAPIState makes the target's users and databases those of a
// backup/source, rewritten for the target instance.
func (s *Service) replaceAPIState(project, name string, dbs []*sqladmin.Database, users []*userRecord) error {
	return s.env.Store.Update(func(tx store.Tx) error {
		prefix := project + "/" + name + "/"
		for _, ns := range []string{nsDatabases, nsUsers} {
			var keys []string
			tx.Scan(ns, prefix, func(k string, _ []byte) bool { keys = append(keys, k); return true })
			for _, k := range keys {
				if err := tx.Delete(ns, k); err != nil {
					return err
				}
			}
		}
		for _, d := range dbs {
			nd := newDatabase(project, name, d.Name, d.Charset, d.Collation)
			if err := store.PutJSON(tx, nsDatabases, childKey(project, name, d.Name), nd); err != nil {
				return err
			}
		}
		rec, ok := getInstance(tx, project, name)
		for _, u := range users {
			nu := &userRecord{User: newUser(project, name, u.User.Name, orDefault(u.User.Type, "BUILT_IN")), Password: u.Password}
			if err := store.PutJSON(tx, nsUsers, childKey(project, name, u.User.Name), nu); err != nil {
				return err
			}
			if ok && u.User.Name == "postgres" {
				rec.RootPassword = u.Password
			}
		}
		if ok {
			return store.PutJSON(tx, nsInstances, instKey(project, name), rec)
		}
		return nil
	})
}

// loadDump replays a pg_dumpall into the instance; "already exists"
// errors for the bootstrap roles are expected and ignored.
func (s *Service) loadDump(ctx context.Context, project, name, dump string) error {
	_, err := s.execIn(ctx, project, name, dump, "psql", "-X", "-q", "-h", "/var/run/postgresql", "-p", "5433", "-U", superuser, "-d", "postgres", "-f", "-")
	return err
}

// reinitialize replaces the instance's volumes with a fresh cluster. The
// WAL archive goes too: the new cluster cannot replay the old one's WAL.
func (s *Service) reinitialize(ctx context.Context, project, name string) error {
	rt, _, err := s.plane(ctx)
	if err != nil {
		return err
	}
	if err := s.stopContainer(ctx, project, name); err != nil {
		return err
	}
	for _, v := range []string{volumeName(rt, project, name), archiveVolumeName(rt, project, name)} {
		if err := rt.RemoveVolume(ctx, v); err != nil {
			return err
		}
	}
	if err := s.updateRecord(project, name, func(r *instanceRecord) error {
		r.AppliedFlags = serverFlags(r.Instance.Settings)
		return nil
	}); err != nil {
		return err
	}
	return s.startContainer(ctx, project, name)
}

// backupName is a backup's name in the backups API. A backup is the same
// stored record as the backup run of the same ID; IDs are unique within a
// project.
func backupName(project string, id int64) string {
	return "projects/" + project + "/backups/" + strconv.FormatInt(id, 10)
}

var backupNameRe = regexp.MustCompile(`^projects/([^/]+)/backups/([0-9]+)$`)

// parseBackupName splits "projects/P/backups/ID".
func parseBackupName(name string) (project string, id int64, err error) {
	m := backupNameRe.FindStringSubmatch(name)
	if m == nil {
		return "", 0, errInvalid("Invalid backup name %q: expected projects/{project}/backups/{backup}.", name)
	}
	id, err = strconv.ParseInt(m[2], 10, 64)
	if err != nil {
		return "", 0, errInvalid("Invalid backup name %q: expected projects/{project}/backups/{backup}.", name)
	}
	return m[1], id, nil
}

// findBackup looks a backup up by project and ID, whatever its instance.
func (s *Service) findBackup(project string, id int64) (*backupRecord, bool) {
	var found *backupRecord
	_ = s.env.Store.View(func(tx store.Tx) error {
		scanJSON(tx, nsBackups, project+"/", func(k string, b *backupRecord) {
			if found == nil && strings.HasSuffix(k, "/"+backupKey(id)) {
				found = b
			}
		})
		return nil
	})
	return found, found != nil
}

// backupIDTaken reports whether a backup of the project has the ID.
func backupIDTaken(tx store.Tx, project string, id int64) bool {
	taken := false
	tx.Scan(nsBackups, project+"/", func(k string, _ []byte) bool {
		taken = strings.HasSuffix(k, "/"+backupKey(id))
		return !taken
	})
	return taken
}

// updateBackupRecord changes a stored backup in place.
func (s *Service) updateBackupRecord(project, name string, id int64, fn func(b *backupRecord)) error {
	return s.env.Store.Update(func(tx store.Tx) error {
		var b backupRecord
		k := childKey(project, name, backupKey(id))
		if err := store.GetJSON(tx, nsBackups, k, &b); err != nil {
			return errBackupMissing()
		}
		fn(&b)
		return store.PutJSON(tx, nsBackups, k, &b)
	})
}

// newBackupRun returns a RUNNING backup run of the instance, not yet
// stored.
func (s *Service) newBackupRun(rec *instanceRecord, typ, description string) *sqladmin.BackupRun {
	return &sqladmin.BackupRun{
		Kind:            "sql#backupRun",
		Instance:        rec.Instance.Name,
		Description:     description,
		Type:            typ,
		BackupKind:      "SNAPSHOT",
		Status:          "RUNNING",
		DatabaseVersion: rec.Instance.DatabaseVersion,
		EnqueuedTime:    s.now(),
		WindowStartTime: s.now(),
		Location:        rec.Instance.Region,
	}
}

// storeNewBackup gives b's run an ID unique in the project and stores it.
func (s *Service) storeNewBackup(project string, b *backupRecord) error {
	return s.env.Store.Update(func(tx store.Tx) error {
		id := s.env.Clock.Now().UnixMilli()
		for backupIDTaken(tx, project, id) {
			id++
		}
		b.Run.Id = id
		b.Run.SelfLink = instanceLink(project, b.Run.Instance) + "/backupRuns/" + strconv.FormatInt(id, 10)
		return store.PutJSON(tx, nsBackups, b.key(project), b)
	})
}

// fail marks the backup FAILED with err.
func (s *Service) fail(b *backupRecord, err error) {
	b.Run.Status, b.Run.EndTime = "FAILED", s.now()
	b.Run.Error = &sqladmin.OperationError{Kind: "sql#operationError", Code: "INTERNAL_ERROR", Message: err.Error()}
}

// writeDump writes a pg_dumpall of the instance to the file of the backup
// with the ID stored under owner (backupRecord.owner) and returns its path.
func (s *Service) writeDump(ctx context.Context, project, name, owner string, id int64) (string, error) {
	dump, err := s.dumpAll(ctx, project, name)
	if err != nil {
		return "", err
	}
	dir, err := s.backupDir(project, owner)
	if err != nil {
		return "", err
	}
	file := filepath.Join(dir, strconv.FormatInt(id, 10)+".sql")
	return file, writeFileAtomic(file, []byte(dump))
}

// startBackup stores a RUNNING on-demand backup of the instance and starts
// the BACKUP_VOLUME operation that fills it: a pg_dumpall with the API's
// users and databases and, with point-in-time recovery enabled, a base
// backup that later recoveries can start from.
func (s *Service) startBackup(ctx context.Context, rec *instanceRecord, description, expiry string) (*sqladmin.Operation, error) {
	project, name := rec.Instance.Project, rec.Instance.Name
	b := &backupRecord{Run: s.newBackupRun(rec, "ON_DEMAND", description), ExpiryTime: expiry}
	if err := s.storeNewBackup(project, b); err != nil {
		return nil, err
	}
	id := b.Run.Id
	op := s.newOp(ctx, project, name, "BACKUP_VOLUME")
	op.BackupContext = &sqladmin.BackupContext{Kind: "sql#backupContext", BackupId: id, Name: backupName(project, id)}
	return s.runOp(op, true, func(ctx context.Context) error {
		err := s.fillBackup(ctx, project, name, id)
		if err != nil && !wasReset(ctx) {
			_ = s.updateBackupRecord(project, name, id, func(b *backupRecord) { s.fail(b, err) })
		}
		return err
	}), nil
}

func (s *Service) fillBackup(ctx context.Context, project, name string, id int64) error {
	start := s.now()
	if err := s.updateBackupRecord(project, name, id, func(b *backupRecord) { b.Run.StartTime = start }); err != nil {
		return err
	}
	file, err := s.writeDump(ctx, project, name, name, id)
	if err != nil {
		return err
	}
	if rec, err := s.loadRecord(project, name); err == nil && pitrEnabled(rec.Instance) {
		if err := s.takeBaseBackup(ctx, project, name); err != nil {
			return err
		}
	}
	dbs, users := s.snapshotAPIState(project, name)
	return s.updateBackupRecord(project, name, id, func(b *backupRecord) {
		b.Run.Status, b.Run.EndTime = "SUCCESSFUL", s.now()
		b.File, b.Databases, b.Users = file, dbs, users
	})
}

func (s *Service) insertBackupRun(w http.ResponseWriter, r *http.Request) {
	rec, err := s.instanceFor(r, "cloudsql.backupRuns.create")
	if err != nil {
		apierr.Write(w, err)
		return
	}
	var req sqladmin.BackupRun
	if err := decode(r, &req, true); err != nil {
		apierr.Write(w, err)
		return
	}
	if err := requireRunnable(rec); err != nil {
		apierr.Write(w, err)
		return
	}
	op, err := s.startBackup(r.Context(), rec, req.Description, "")
	if err != nil {
		apierr.Write(w, err)
		return
	}
	writeJSON(w, r, op)
}

// backupKey sorts newest first.
func backupKey(id int64) string { return fmt.Sprintf("%019d", math.MaxInt64-id) }

func (s *Service) loadBackup(project, name, id string) (*backupRecord, bool) {
	n, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		return nil, false
	}
	var b backupRecord
	var ok bool
	_ = s.env.Store.View(func(tx store.Tx) error {
		ok = store.GetJSON(tx, nsBackups, childKey(project, name, backupKey(n)), &b) == nil
		return nil
	})
	return &b, ok
}

func errBackupNotFound() error {
	return apierr.NotFound("The backup run does not exist.").WithLegacy("backupRunDoesNotExist")
}

func errBackupMissing() error {
	return apierr.NotFound("The backup does not exist.").WithLegacy("notFound")
}

func (s *Service) listBackupRuns(w http.ResponseWriter, r *http.Request) {
	rec, err := s.instanceFor(r, "cloudsql.backupRuns.list")
	if err != nil {
		apierr.Write(w, err)
		return
	}
	items := map[string]*sqladmin.BackupRun{}
	_ = s.env.Store.View(func(tx store.Tx) error {
		scanJSON(tx, nsBackups, rec.Instance.Project+"/"+rec.Instance.Name+"/", func(k string, b *backupRecord) {
			items[k] = b.Run
		})
		return nil
	})
	page, next, err := paginate(r, sortedKeys(items), items)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	writeJSON(w, r, &sqladmin.BackupRunsListResponse{Kind: "sql#backupRunsList", Items: page, NextPageToken: next})
}

func (s *Service) getBackupRun(w http.ResponseWriter, r *http.Request) {
	rec, err := s.instanceFor(r, "cloudsql.backupRuns.get")
	if err != nil {
		apierr.Write(w, err)
		return
	}
	b, ok := s.loadBackup(rec.Instance.Project, rec.Instance.Name, r.PathValue("id"))
	if !ok {
		apierr.Write(w, errBackupNotFound())
		return
	}
	writeJSON(w, r, b.Run)
}

func (s *Service) deleteBackupRun(w http.ResponseWriter, r *http.Request) {
	rec, err := s.instanceFor(r, "cloudsql.backupRuns.delete")
	if err != nil {
		apierr.Write(w, err)
		return
	}
	b, ok := s.loadBackup(rec.Instance.Project, rec.Instance.Name, r.PathValue("id"))
	if !ok {
		apierr.Write(w, errBackupNotFound())
		return
	}
	writeJSON(w, r, s.deleteBackupOp(r.Context(), rec.Instance.Project, b))
}

// deleteBackupOp removes a backup and its dump.
func (s *Service) deleteBackupOp(ctx context.Context, project string, b *backupRecord) *sqladmin.Operation {
	name := b.Run.Instance
	op := s.newOp(ctx, project, name, "DELETE_BACKUP")
	return s.runOp(op, false, func(context.Context) error {
		return s.removeBackup(project, b)
	})
}

// removeBackup deletes a stored backup and its dump.
func (s *Service) removeBackup(project string, b *backupRecord) error {
	if b.File != "" {
		_ = os.Remove(b.File)
	}
	return s.env.Store.Update(func(tx store.Tx) error {
		return tx.Delete(nsBackups, b.key(project))
	})
}

// restoreBackup restores a backup into the instance named in the path: a
// backup run of this or another instance (restoreBackupContext) or a
// backup of the backups API (backup, by name).
func (s *Service) restoreBackup(w http.ResponseWriter, r *http.Request) {
	rec, err := s.instanceFor(r, "cloudsql.instances.restoreBackup")
	if err != nil {
		apierr.Write(w, err)
		return
	}
	var req sqladmin.InstancesRestoreBackupRequest
	if err := decode(r, &req, false); err != nil {
		apierr.Write(w, err)
		return
	}
	rc := req.RestoreBackupContext
	byRun := rc != nil && rc.BackupRunId != 0
	sources := 0
	for _, set := range []bool{byRun, req.Backup != "", req.BackupdrBackup != ""} {
		if set {
			sources++
		}
	}
	switch {
	case sources > 1:
		apierr.Write(w, errInvalid("Only one of restoreBackupContext, backup and backupdrBackup can be set."))
		return
	case req.BackupdrBackup != "":
		apierr.Write(w, errUnsupported("Restoring from Backup and DR (backupdrBackup)"))
		return
	case sources == 0:
		apierr.Write(w, errInvalid("restoreBackupContext.backupRunId is required."))
		return
	}
	if err := requireRunnable(rec); err != nil {
		apierr.Write(w, err)
		return
	}
	project, name := rec.Instance.Project, rec.Instance.Name
	var b *backupRecord
	var srcProject string
	if byRun {
		var srcName string
		srcProject, srcName = orDefault(rc.Project, project), orDefault(rc.InstanceId, name)
		var ok bool
		if b, ok = s.loadBackup(srcProject, srcName, strconv.FormatInt(rc.BackupRunId, 10)); !ok {
			apierr.Write(w, errBackupNotFound())
			return
		}
	} else {
		var id int64
		var err error
		if srcProject, id, err = parseBackupName(req.Backup); err != nil {
			apierr.Write(w, err)
			return
		}
		var ok bool
		if b, ok = s.findBackup(srcProject, id); !ok {
			apierr.Write(w, errBackupMissing())
			return
		}
	}
	if b.Run.Status != "SUCCESSFUL" {
		apierr.Write(w, errInvalid("Backup %d is %s; only a successful backup can be restored.", b.Run.Id, b.Run.Status))
		return
	}
	// The backup must outlive its expiry until the dump has been read.
	release, ok := s.holdBackup(srcProject, b)
	if !ok {
		apierr.Write(w, errBackupMissing())
		return
	}
	op := s.newOp(r.Context(), project, name, "RESTORE_VOLUME")
	writeJSON(w, r, s.runOp(op, true, func(ctx context.Context) error {
		dump, err := os.ReadFile(b.File)
		release()
		if err != nil {
			return newOpError("INTERNAL_ERROR", "read backup: %v", err)
		}
		if err := s.reinitialize(ctx, project, name); err != nil {
			return err
		}
		if err := s.loadDump(ctx, project, name, string(dump)); err != nil {
			return err
		}
		if err := s.replaceAPIState(project, name, b.Databases, b.Users); err != nil {
			return err
		}
		return s.ensureBaseBackup(ctx, project, name)
	}))
}

// cloneInstance creates destinationInstanceName from the source's
// settings and its current data or, with pointInTime, its data as of that
// time (pitr.go).
func (s *Service) cloneInstance(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rec, err := s.instanceFor(r, "cloudsql.instances.clone")
	if err != nil {
		apierr.Write(w, err)
		return
	}
	var req sqladmin.InstancesCloneRequest
	if err := decode(r, &req, false); err != nil {
		apierr.Write(w, err)
		return
	}
	cc := req.CloneContext
	if cc == nil || cc.DestinationInstanceName == "" {
		apierr.Write(w, errInvalid("cloneContext.destinationInstanceName is required."))
		return
	}
	pitr := cc.PointInTime != "" || cc.PitrTimestampMs != 0
	var target time.Time
	if pitr {
		if !pitrEnabled(rec.Instance) {
			apierr.Write(w, errPITRDisabled(rec.Instance.Name))
			return
		}
		if target, err = pointInTime(cc); err != nil {
			apierr.Write(w, err)
			return
		}
		// WAL times are PostgreSQL's, which shares the host's clock.
		if target.After(time.Now()) {
			apierr.Write(w, errInvalid("The point in time %s is in the future.", target.Format(time.RFC3339Nano)))
			return
		}
	}
	if err := requireRunnable(rec); err != nil {
		apierr.Write(w, err)
		return
	}
	project, src := rec.Instance.Project, rec.Instance.Name
	if err := s.check(ctx, "cloudsql.instances.create", projectResource(project)); err != nil {
		apierr.Write(w, err)
		return
	}
	if pitr {
		earliest, _, err := s.recoveryWindow(ctx, project, src)
		if err != nil {
			apierr.Write(w, err)
			return
		}
		if target.Before(earliest) {
			apierr.Write(w, errInvalid("The point in time %s is before the earliest recovery time %s.", target.Format(time.RFC3339Nano), earliest.Format(time.RFC3339Nano)))
			return
		}
	}
	raw := map[string]any{
		"name":            cc.DestinationInstanceName,
		"databaseVersion": rec.Instance.DatabaseVersion,
		"region":          rec.Instance.Region,
		"rootPassword":    rec.RootPassword,
		"settings":        toMap(rec.Instance.Settings),
	}
	st := child(raw, "settings")
	delete(st, "settingsVersion")
	if cc.DestinationNetwork != "" {
		ip := child(st, "ipConfiguration")
		ip["privateNetwork"] = cc.DestinationNetwork
	}
	if cc.PreferredZone != "" {
		st["locationPreference"] = map[string]any{"zone": cc.PreferredZone}
	}
	dest := cc.DestinationInstanceName
	hooks := createHooks{after: func(ctx context.Context) error {
		dump, err := s.dumpAll(ctx, project, src)
		if err != nil {
			return err
		}
		if err := s.loadDump(ctx, project, dest, dump); err != nil {
			return err
		}
		dbs, users := s.snapshotAPIState(project, src)
		if dbs, err = s.keepDatabases(ctx, project, dest, dbs, cc.DatabaseNames); err != nil {
			return err
		}
		return s.replaceAPIState(project, dest, dbs, users)
	}}
	if pitr {
		hooks.files = func(ctx context.Context) ([]runtime.File, error) {
			return s.restoreBundle(ctx, project, src, target)
		}
		hooks.after = func(ctx context.Context) error {
			if err := s.finishRecovery(ctx, project, dest); err != nil {
				return err
			}
			dbs, users, err := s.recoveredAPIState(ctx, project, src, dest)
			if err != nil {
				return err
			}
			if dbs, err = s.keepDatabases(ctx, project, dest, dbs, cc.DatabaseNames); err != nil {
				return err
			}
			return s.replaceAPIState(project, dest, dbs, users)
		}
	}
	op, err := s.createInstance(ctx, project, raw, "CLONE", hooks)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	writeJSON(w, r, op)
}

// keepDatabases drops from a clone the databases not in names (all are
// kept when names is empty) and returns those kept.
func (s *Service) keepDatabases(ctx context.Context, project, dest string, dbs []*sqladmin.Database, names []string) ([]*sqladmin.Database, error) {
	if len(names) == 0 {
		return dbs, nil
	}
	keep := map[string]bool{"postgres": true}
	for _, n := range names {
		keep[n] = true
	}
	var kept []*sqladmin.Database
	for _, d := range dbs {
		if keep[d.Name] {
			kept = append(kept, d)
		} else if _, err := s.execSQL(ctx, project, dest, "postgres", "", "DROP DATABASE IF EXISTS "+quoteIdent(d.Name)+" WITH (FORCE);"); err != nil {
			return nil, err
		}
	}
	return kept, nil
}
