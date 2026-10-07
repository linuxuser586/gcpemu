package sql

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"

	sqladmin "google.golang.org/api/sqladmin/v1beta4"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// Backups, restore and clone (FR-SQL-008), implemented with pg_dumpall:
// a backup run stores a logical dump of the whole cluster (roles and all
// databases) under the service's data directory together with the API's
// users and databases; restore re-initialises the target's volume and
// replays the dump; clone creates a new instance and loads a dump of the
// source into it.

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

// reinitialize replaces the instance's volume with a fresh cluster.
func (s *Service) reinitialize(ctx context.Context, project, name string) error {
	rt, _, err := s.plane(ctx)
	if err != nil {
		return err
	}
	if err := s.stopContainer(ctx, project, name); err != nil {
		return err
	}
	if err := rt.RemoveVolume(ctx, volumeName(rt, project, name)); err != nil {
		return err
	}
	if err := s.updateRecord(project, name, func(r *instanceRecord) error {
		r.AppliedFlags = flagMap(r.Instance.Settings)
		return nil
	}); err != nil {
		return err
	}
	return s.startContainer(ctx, project, name)
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
	project, name := rec.Instance.Project, rec.Instance.Name
	id := s.env.Clock.Now().UnixMilli()
	run := &sqladmin.BackupRun{
		Kind:            "sql#backupRun",
		Id:              id,
		Instance:        name,
		Description:     req.Description,
		Type:            "ON_DEMAND",
		BackupKind:      "SNAPSHOT",
		Status:          "RUNNING",
		DatabaseVersion: rec.Instance.DatabaseVersion,
		EnqueuedTime:    s.now(),
		WindowStartTime: s.now(),
		Location:        rec.Instance.Region,
		SelfLink:        instanceLink(project, name) + "/backupRuns/" + strconv.FormatInt(id, 10),
	}
	op := s.newOp(r.Context(), project, name, "BACKUP_VOLUME")
	op.BackupContext = &sqladmin.BackupContext{Kind: "sql#backupContext", BackupId: id}
	writeJSON(w, r, s.runOp(op, true, func(ctx context.Context) error {
		run.StartTime = s.now()
		dump, err := s.dumpAll(ctx, project, name)
		if err != nil {
			return err
		}
		dir, err := s.backupDir(project, name)
		if err != nil {
			return err
		}
		file := filepath.Join(dir, strconv.FormatInt(id, 10)+".sql")
		if err := writeFileAtomic(file, []byte(dump)); err != nil {
			return err
		}
		dbs, users := s.snapshotAPIState(project, name)
		run.Status, run.EndTime = "SUCCESSFUL", s.now()
		return s.env.Store.Update(func(tx store.Tx) error {
			return store.PutJSON(tx, nsBackups, childKey(project, name, backupKey(id)), &backupRecord{Run: run, File: file, Databases: dbs, Users: users})
		})
	}))
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
	project, name := rec.Instance.Project, rec.Instance.Name
	b, ok := s.loadBackup(project, name, r.PathValue("id"))
	if !ok {
		apierr.Write(w, errBackupNotFound())
		return
	}
	op := s.newOp(r.Context(), project, name, "DELETE_BACKUP")
	writeJSON(w, r, s.runOp(op, false, func(context.Context) error {
		_ = os.Remove(b.File)
		return s.env.Store.Update(func(tx store.Tx) error {
			return tx.Delete(nsBackups, childKey(project, name, backupKey(b.Run.Id)))
		})
	}))
}

// restoreBackup restores a backup run of this or another instance
// (restoreBackupContext.instanceId) into the instance named in the path.
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
	if rc == nil || rc.BackupRunId == 0 {
		if req.Backup != "" {
			apierr.Write(w, errUnsupported("Restoring from the backups API (backup names)"))
			return
		}
		apierr.Write(w, errInvalid("restoreBackupContext.backupRunId is required."))
		return
	}
	if err := requireRunnable(rec); err != nil {
		apierr.Write(w, err)
		return
	}
	project, name := rec.Instance.Project, rec.Instance.Name
	srcProject, srcName := orDefault(rc.Project, project), orDefault(rc.InstanceId, name)
	b, ok := s.loadBackup(srcProject, srcName, strconv.FormatInt(rc.BackupRunId, 10))
	if !ok {
		apierr.Write(w, errBackupNotFound())
		return
	}
	op := s.newOp(r.Context(), project, name, "RESTORE_VOLUME")
	writeJSON(w, r, s.runOp(op, true, func(ctx context.Context) error {
		dump, err := os.ReadFile(b.File)
		if err != nil {
			return newOpError("INTERNAL_ERROR", "read backup: %v", err)
		}
		if err := s.reinitialize(ctx, project, name); err != nil {
			return err
		}
		if err := s.loadDump(ctx, project, name, string(dump)); err != nil {
			return err
		}
		return s.replaceAPIState(project, name, b.Databases, b.Users)
	}))
}

// cloneInstance creates destinationInstanceName from the source's
// settings and current data.
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
	if cc.PointInTime != "" || cc.PitrTimestampMs != 0 {
		apierr.Write(w, errUnsupported("Point-in-time clone (FR-SQL-010)"))
		return
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
	op, err := s.createInstance(ctx, project, raw, "CLONE", func(ctx context.Context) error {
		dump, err := s.dumpAll(ctx, project, src)
		if err != nil {
			return err
		}
		if err := s.loadDump(ctx, project, dest, dump); err != nil {
			return err
		}
		dbs, users := s.snapshotAPIState(project, src)
		if len(cc.DatabaseNames) > 0 {
			keep := map[string]bool{"postgres": true}
			for _, n := range cc.DatabaseNames {
				keep[n] = true
			}
			var kept []*sqladmin.Database
			for _, d := range dbs {
				if keep[d.Name] {
					kept = append(kept, d)
				} else if _, err := s.execSQL(ctx, project, dest, "postgres", "", "DROP DATABASE IF EXISTS "+quoteIdent(d.Name)+" WITH (FORCE);"); err != nil {
					return err
				}
			}
			dbs = kept
		}
		return s.replaceAPIState(project, dest, dbs, users)
	})
	if err != nil {
		apierr.Write(w, err)
		return
	}
	writeJSON(w, r, op)
}
