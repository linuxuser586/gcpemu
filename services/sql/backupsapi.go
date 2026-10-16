package sql

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	sqladmin "google.golang.org/api/sqladmin/v1beta4"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// The backups API (FR-SQL-008): projects/{project}/backups/{backup} is the
// project-level view of the stored backup runs, with the run's ID as the
// backup ID. Permissions are the backupRuns ones on the backup's instance,
// as in Cloud SQL. A backup past its expiryTime (set by expiryTime or
// ttlDays) is deleted on the emulator clock. Unlike Cloud SQL, which only
// lets FINAL backups expire, any backup can: the emulator has no final
// backups yet (#407).

// toBackup renders a stored backup as a backups API resource.
func toBackup(project string, b *backupRecord) *sqladmin.Backup {
	run := b.Run
	name := backupName(project, run.Id)
	out := &sqladmin.Backup{
		Kind:            "sql#backup",
		Name:            name,
		SelfLink:        linkBase + name,
		Instance:        run.Instance,
		Description:     run.Description,
		Type:            run.Type,
		State:           run.Status,
		BackupKind:      run.BackupKind,
		DatabaseVersion: run.DatabaseVersion,
		Location:        run.Location,
		BackupRun:       run.SelfLink,
		ExpiryTime:      b.ExpiryTime,
		Error:           run.Error,
	}
	if run.StartTime != "" || run.EndTime != "" {
		out.BackupInterval = &sqladmin.Interval{StartTime: run.StartTime, EndTime: run.EndTime}
	}
	return out
}

// backupFor loads the backup named in the path and checks perm on its
// instance.
func (s *Service) backupFor(r *http.Request, perm string) (*backupRecord, error) {
	project := r.PathValue("project")
	id, err := strconv.ParseInt(r.PathValue("backup"), 10, 64)
	if err != nil {
		return nil, errBackupMissing()
	}
	b, ok := s.findBackup(project, id)
	if !ok {
		return nil, errBackupMissing()
	}
	if err := s.check(r.Context(), perm, instanceResource(project, b.Run.Instance)); err != nil {
		return nil, err
	}
	return b, nil
}

// maxTTLDays is how far ahead, in days, a backup's expiry can be, as in
// Cloud SQL.
const maxTTLDays = 365

// expiryOf returns the expiry time a request asks for: ttlDays from now,
// or expiryTime. ttlDays 0 cannot be told from an absent ttlDays and
// means none; an expiryTime in the past is accepted, and the next sweep
// deletes the backup.
func (s *Service) expiryOf(b *sqladmin.Backup) (string, error) {
	now := s.env.Clock.Now().UTC()
	switch {
	case b.TtlDays < 0 || b.TtlDays > maxTTLDays:
		return "", errInvalid("ttlDays must be between 1 and %d.", maxTTLDays)
	case b.TtlDays > 0:
		return now.Add(time.Duration(b.TtlDays) * 24 * time.Hour).Format(time.RFC3339Nano), nil
	case b.ExpiryTime != "":
		t, err := time.Parse(time.RFC3339Nano, b.ExpiryTime)
		if err != nil {
			return "", errInvalid("Invalid value for expiryTime: %q is not an RFC 3339 timestamp.", b.ExpiryTime)
		}
		if t.Sub(now) > maxTTLDays*24*time.Hour {
			return "", errInvalid("expiryTime must be at most %d days from now.", maxTTLDays)
		}
		return b.ExpiryTime, nil
	}
	return "", nil
}

func (s *Service) createBackup(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("project")
	var req sqladmin.Backup
	if err := decode(r, &req, false); err != nil {
		apierr.Write(w, err)
		return
	}
	if req.Instance == "" {
		apierr.Write(w, errInvalid("backup.instance is required."))
		return
	}
	if err := s.check(r.Context(), "cloudsql.backupRuns.create", instanceResource(project, req.Instance)); err != nil {
		apierr.Write(w, err)
		return
	}
	rec, err := s.loadRecord(project, req.Instance)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	expiry, err := s.expiryOf(&req)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	if err := requireRunnable(rec); err != nil {
		apierr.Write(w, err)
		return
	}
	op, err := s.startBackup(r.Context(), rec, req.Description, expiry)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	writeJSON(w, r, op)
}

func (s *Service) listBackups(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("project")
	if err := s.env.EnsureProject(project); err != nil {
		apierr.Write(w, err)
		return
	}
	if err := s.check(r.Context(), "cloudsql.backupRuns.list", projectResource(project)); err != nil {
		apierr.Write(w, err)
		return
	}
	match, err := compileFilter(r.URL.Query().Get("filter"), backupFilterFields)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	// Newest first across the project's instances.
	items := map[string]*sqladmin.Backup{}
	_ = s.env.Store.View(func(tx store.Tx) error {
		scanJSON(tx, nsBackups, project+"/", func(k string, b *backupRecord) {
			// k is P/I/backupKey; order by backupKey, then instance.
			_, rest := splitKey(k)
			inst, idKey, _ := strings.Cut(rest, "/")
			if out := toBackup(project, b); match(out) {
				items[idKey+"/"+inst] = out
			}
		})
		return nil
	})
	page, next, err := paginateBy(r, "pageSize", sortedKeys(items), items)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	writeJSON(w, r, &sqladmin.ListBackupsResponse{Backups: page, NextPageToken: next})
}

func (s *Service) getBackup(w http.ResponseWriter, r *http.Request) {
	b, err := s.backupFor(r, "cloudsql.backupRuns.get")
	if err != nil {
		apierr.Write(w, err)
		return
	}
	writeJSON(w, r, toBackup(r.PathValue("project"), b))
}

// updateBackup changes description and expiry (updateMask: description,
// expiryTime, ttlDays).
func (s *Service) updateBackup(w http.ResponseWriter, r *http.Request) {
	mask := r.URL.Query().Get("updateMask")
	if mask == "" {
		apierr.Write(w, errInvalid("updateMask is required."))
		return
	}
	var req sqladmin.Backup
	if err := decode(r, &req, false); err != nil {
		apierr.Write(w, err)
		return
	}
	var setDesc, setExpiry bool
	for _, f := range strings.Split(mask, ",") {
		switch strings.TrimSpace(f) {
		case "description":
			setDesc = true
		case "expiryTime", "expiry_time", "ttlDays", "ttl_days":
			setExpiry = true
		default:
			apierr.Write(w, errInvalid("Field %q in updateMask cannot be updated.", f))
			return
		}
	}
	b, err := s.backupFor(r, "cloudsql.backupRuns.update")
	if err != nil {
		apierr.Write(w, err)
		return
	}
	expiry, err := s.expiryOf(&req)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	if setExpiry && expiry == "" {
		apierr.Write(w, errInvalid("updateMask names expiryTime or ttlDays, but the backup sets neither."))
		return
	}
	project, inst := r.PathValue("project"), b.Run.Instance
	op := s.newOp(r.Context(), project, inst, "UPDATE_BACKUP")
	writeJSON(w, r, s.runOp(op, false, func(_ context.Context) error {
		return s.updateBackupRecord(project, inst, b.Run.Id, func(b *backupRecord) {
			if setDesc {
				b.Run.Description = req.Description
			}
			if setExpiry {
				b.ExpiryTime = expiry
			}
		})
	}))
}

func (s *Service) deleteBackup(w http.ResponseWriter, r *http.Request) {
	b, err := s.backupFor(r, "cloudsql.backupRuns.delete")
	if err != nil {
		apierr.Write(w, err)
		return
	}
	writeJSON(w, r, s.deleteBackupOp(r.Context(), r.PathValue("project"), b))
}
