package sql

import (
	"context"
	"net/url"
	"strconv"
	"time"

	sqladmin "google.golang.org/api/sqladmin/v1beta4"
)

// Final backups (FR-SQL-008, #407): instances.delete can take a backup of
// the Cloud SQL instance it deletes, asked for by enableFinalBackup or by
// settings.finalBackupConfig. It is a pg_dumpall like an on-demand backup,
// taken before the runtime objects go, and outlives the instance. Any
// instance not running has its container started for it: a stopped one,
// and a FAILED or SUSPENDED one too, since its data volume is kept and
// the start often recovers it. A dump that fails leaves a FAILED Final
// backup and the instance is deleted anyway, as in Cloud SQL.
//
// A Final backup is stored under "P/I@final/backupKey" and its dump under
// backups/P/I@final: instance names cannot contain "@", so neither the
// instance's teardown nor a later Cloud SQL instance of the same name sees
// it as one of its backup runs. Its permissions are still checked on the
// instance's resource name, so a later instance's IAM policy governs it,
// as IAM conditions on resource.name would in Cloud SQL.

// finalSuffix marks the key segment of a Final backup's deleted instance.
const finalSuffix = "@final"

// defaultFinalRetention is how long a Final backup is kept when neither
// the delete nor settings.finalBackupConfig says, as in Cloud SQL.
const defaultFinalRetention = 30 * 24 * time.Hour

// finalBackupRequest is what a delete asks of its Final backup.
type finalBackupRequest struct {
	Description string
	ExpiryTime  string
	// DeletionTime is the instance's deletion time.
	DeletionTime string
}

// owner is the key segment the backup is stored under: its instance's
// name, or for a Final backup the deleted instance's.
func (b *backupRecord) owner() string {
	if b.InstanceDeletionTime != "" {
		return b.Run.Instance + finalSuffix
	}
	return b.Run.Instance
}

// deletedAt is a Final backup's instanceDeletionTime; zero for another
// backup.
func (b *backupRecord) deletedAt() (time.Time, error) {
	if b.InstanceDeletionTime == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339Nano, b.InstanceDeletionTime)
	if err != nil {
		return time.Time{}, newOpError("INTERNAL_ERROR", "backup %d: invalid instanceDeletionTime %q", b.Run.Id, b.InstanceDeletionTime)
	}
	return t, nil
}

// finalBackupOf validates the Final backup parameters of a delete of rec
// at now and returns the Final backup to take, or nil for none.
func finalBackupOf(q url.Values, rec *instanceRecord, now time.Time) (*finalBackupRequest, error) {
	var cfg *sqladmin.FinalBackupConfig
	if rec.Instance.Settings != nil {
		cfg = rec.Instance.Settings.FinalBackupConfig
	}
	configured := cfg != nil && cfg.Enabled
	take := configured
	if v := q.Get("enableFinalBackup"); v != "" {
		enable, err := strconv.ParseBool(v)
		if err != nil {
			return nil, errInvalid("Invalid value for enableFinalBackup: %q is not a boolean.", v)
		}
		if !enable && configured {
			return nil, errInvalid("enableFinalBackup is false, but the instance's finalBackupConfig takes a final backup. Omit enableFinalBackup or set it to true.")
		}
		take = enable
	}
	ttl, expiry, desc := q.Get("finalBackupTtlDays"), q.Get("finalBackupExpiryTime"), q.Get("finalBackupDescription")
	if !take {
		if ttl != "" || expiry != "" || desc != "" {
			return nil, errInvalid("finalBackupTtlDays, finalBackupExpiryTime and finalBackupDescription need a final backup: set enableFinalBackup or enable the instance's finalBackupConfig.")
		}
		return nil, nil
	}
	if ttl != "" && expiry != "" {
		return nil, errInvalid("Only one of finalBackupTtlDays and finalBackupExpiryTime can be set.")
	}
	ask := expiryAsk{ExpiryTime: expiry, ttlName: "finalBackupTtlDays", atName: "finalBackupExpiryTime"}
	if ttl != "" {
		// An explicit 0 is out of range, unlike an absent ttlDays.
		n, err := strconv.ParseInt(ttl, 10, 64)
		if err != nil || n < 1 {
			n = -1
		}
		ask.TTLDays = n
	}
	at, err := ask.resolve(now, now, "from now")
	if err != nil {
		return nil, err
	}
	fb := &finalBackupRequest{Description: desc, ExpiryTime: at, DeletionTime: now.Format(time.RFC3339Nano)}
	if at == "" {
		// retentionDays 0 is an unset retentionDays: the Go client and
		// gcloud omit it.
		retention := defaultFinalRetention
		if cfg != nil && cfg.RetentionDays > 0 {
			retention = days(cfg.RetentionDays)
		}
		fb.ExpiryTime = now.Add(retention).Format(time.RFC3339Nano)
	}
	return fb, nil
}

// takeFinalBackup stores the Final backup of the Cloud SQL instance being
// deleted and fills it with a dump. A failed dump leaves the backup
// FAILED; only failing to store it fails the delete.
func (s *Service) takeFinalBackup(ctx context.Context, rec *instanceRecord, fb *finalBackupRequest) error {
	project, name := rec.Instance.Project, rec.Instance.Name
	run := s.newBackupRun(rec, "FINAL", fb.Description)
	run.StartTime = s.now()
	b := &backupRecord{Run: run, ExpiryTime: fb.ExpiryTime, InstanceDeletionTime: fb.DeletionTime, InstanceSettings: rec.Instance}
	if err := s.storeNewBackup(project, b); err != nil {
		return err
	}
	file, err := s.dumpFinal(ctx, project, name, b)
	dbs, users := s.snapshotAPIState(project, name)
	return s.updateBackupRecord(project, b.owner(), run.Id, func(b *backupRecord) {
		if err != nil {
			s.fail(b, err)
			return
		}
		b.Run.Status, b.Run.EndTime = "SUCCESSFUL", s.now()
		b.File, b.Databases, b.Users = file, dbs, users
	})
}

// dumpFinal writes a pg_dumpall of the Cloud SQL instance to its Final
// backup b's file, starting a stopped instance's container for it.
func (s *Service) dumpFinal(ctx context.Context, project, name string, b *backupRecord) (string, error) {
	if !s.running(ctx, project, name) {
		if err := s.startContainer(ctx, project, name); err != nil {
			return "", err
		}
	}
	return s.writeDump(ctx, project, name, b.owner(), b.Run.Id)
}
