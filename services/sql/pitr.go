package sql

import (
	"context"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	sqladmin "google.golang.org/api/sqladmin/v1beta4"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/runtime"
	"github.com/linuxuser586/gcpemu/services/sql/proxy"
)

// Point-in-time recovery (FR-SQL-008, FR-SQL-010). With
// backupConfiguration.pointInTimeRecoveryEnabled PostgreSQL archives every
// completed WAL segment into the instance's archive volume (archiveCommand)
// and the emulator keeps physical base backups there too, under
// base/<end>, where <end> is PostgreSQL's clock (Unix microseconds) once
// the backup was complete: one when the instance is created or PITR is
// turned on (the earliest recovery time), and one with every backup.
//
// A clone with pointInTime archives the source's current WAL segment, packs
// the newest base backup that ended at or before that time with the
// archived WAL into a restore bundle, and creates the clone with the bundle
// in its container: the agent lays the base backup out as the data
// directory and PostgreSQL replays the WAL up to the time and promotes
// (proxy/restore.go). The clone then gets its own settings and starts its
// own WAL history with a base backup of its own.

const (
	archiveWAL  = proxy.ArchiveDir + "/wal"
	archiveBase = proxy.ArchiveDir + "/base"
	// archiveTimeout bounds the wait for a switched WAL segment to be archived.
	archiveTimeout = time.Minute
	// recoveryTimeout bounds the wait for a clone's replay and promotion.
	recoveryTimeout = 5 * time.Minute
)

func pitrEnabled(in *sqladmin.DatabaseInstance) bool { return pitrOn(in.Settings) }

func pitrOn(st *sqladmin.Settings) bool {
	return st != nil && st.BackupConfiguration != nil && st.BackupConfiguration.PointInTimeRecoveryEnabled
}

func errPITRDisabled(name string) error {
	return errInvalid("Point-in-time recovery is not enabled for instance %s.", name)
}

// pgClockSQL is PostgreSQL's clock in Unix microseconds.
const pgClockSQL = "SELECT (extract(epoch FROM clock_timestamp()) * 1000000)::bigint"

// takeBaseBackup stores a base backup in the archive volume.
func (s *Service) takeBaseBackup(ctx context.Context, project, name string) error {
	psql := "psql -X -At -h " + proxy.SocketDir + " -p " + strconv.Itoa(proxy.InternalPort) + " -U " + superuser + " -d postgres"
	script := "set -e\n" +
		"tmp=" + archiveBase + "/.partial\n" +
		"rm -rf \"$tmp\"\n" +
		"pg_basebackup -h " + proxy.SocketDir + " -p " + strconv.Itoa(proxy.InternalPort) + " -U " + superuser +
		" --no-password -D \"$tmp\" -Ft -z -X fetch --checkpoint=fast\n" +
		"end=$(" + psql + " -c \"" + pgClockSQL + "\")\n" +
		"mv \"$tmp\" " + archiveBase + "/\"$end\"\n"
	if _, err := s.execIn(ctx, project, name, "", "sh", "-c", script); err != nil {
		return newOpError("INTERNAL_ERROR", "base backup: %v", err)
	}
	return nil
}

// ensureBaseBackup takes a base backup when PITR is enabled and the
// archive has none: an instance started on a fresh archive volume (after a
// State snapshot or backup restore, or the first start of an instance
// created stopped).
func (s *Service) ensureBaseBackup(ctx context.Context, project, name string) error {
	rec, err := s.loadRecord(project, name)
	if err != nil || !pitrEnabled(rec.Instance) {
		return err
	}
	ends, err := s.baseBackups(ctx, project, name)
	if err != nil || len(ends) > 0 {
		return err
	}
	return s.takeBaseBackup(ctx, project, name)
}

// clearArchive removes the archived WAL and base backups.
func (s *Service) clearArchive(ctx context.Context, project, name string) error {
	_, err := s.execIn(ctx, project, name, "", "sh", "-c", "rm -rf "+archiveWAL+"/* "+archiveBase+"/*")
	return err
}

// baseBackups lists the instance's base backups by end time, oldest first.
func (s *Service) baseBackups(ctx context.Context, project, name string) ([]int64, error) {
	out, err := s.execIn(ctx, project, name, "", "ls", "-1", archiveBase)
	if err != nil {
		return nil, err
	}
	var ends []int64
	for _, f := range strings.Fields(out) {
		if n, err := strconv.ParseInt(f, 10, 64); err == nil {
			ends = append(ends, n)
		}
	}
	sort.Slice(ends, func(i, j int) bool { return ends[i] < ends[j] })
	return ends, nil
}

// recoveryWindow returns the earliest and latest times an instance can be
// recovered to: the end of its oldest base backup and now.
func (s *Service) recoveryWindow(ctx context.Context, project, name string) (earliest, latest time.Time, err error) {
	ends, err := s.baseBackups(ctx, project, name)
	if err != nil {
		return
	}
	if len(ends) == 0 {
		err = errInvalid("Instance %s has no base backup to recover from yet.", name)
		return
	}
	now, err := s.execSQL(ctx, project, name, "postgres", "", pgClockSQL+";")
	if err != nil {
		return
	}
	n, perr := strconv.ParseInt(strings.TrimSpace(now), 10, 64)
	if perr != nil {
		err = newOpError("INTERNAL_ERROR", "read PostgreSQL's clock: %q", now)
		return
	}
	return time.UnixMicro(ends[0]).UTC(), time.UnixMicro(n).UTC(), nil
}

// archiveNow makes sure the WAL up to now is archived: it writes a commit
// record (so that replay finds one past any earlier target time), switches
// to a new segment and waits until the old one is archived.
func (s *Service) archiveNow(ctx context.Context, project, name string) error {
	out, err := s.execSQL(ctx, project, name, "postgres", "", "SELECT txid_current();\nSELECT pg_walfile_name(pg_switch_wal());")
	if err != nil {
		return err
	}
	lines := strings.Fields(out)
	if len(lines) == 0 {
		return newOpError("INTERNAL_ERROR", "pg_switch_wal returned nothing")
	}
	seg := lines[len(lines)-1]
	ctx, cancel := context.WithTimeout(ctx, archiveTimeout)
	defer cancel()
	for {
		last, err := s.execSQL(ctx, project, name, "postgres", "", "SELECT coalesce(last_archived_wal, '') FROM pg_stat_archiver;")
		if err != nil {
			return err
		}
		// Segment names of one timeline sort by position.
		if last = strings.TrimSpace(last); len(last) >= 24 && last[:24] >= seg {
			return nil
		}
		select {
		case <-ctx.Done():
			return newOpError("INTERNAL_ERROR", "WAL segment %s was not archived in time", seg)
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// restoreBundle returns the files that make a new container recover the
// source's data as of target (proxy/restore.go).
func (s *Service) restoreBundle(ctx context.Context, project, src string, target time.Time) ([]runtime.File, error) {
	if err := s.archiveNow(ctx, project, src); err != nil {
		return nil, err
	}
	ends, err := s.baseBackups(ctx, project, src)
	if err != nil {
		return nil, err
	}
	var base int64
	for _, e := range ends {
		if e <= target.UnixMicro() {
			base = e
		}
	}
	if base == 0 {
		return nil, newOpError("INVALID_REQUEST", "No base backup of %s ends at or before %s.", src, target.Format(time.RFC3339Nano))
	}
	tgz, err := s.execIn(ctx, project, src, "", "tar", "-czf", "-", "-C", proxy.ArchiveDir, "base/"+strconv.FormatInt(base, 10), "wal")
	if err != nil {
		return nil, newOpError("INTERNAL_ERROR", "pack WAL archive: %v", err)
	}
	return []runtime.File{
		{Name: strings.TrimPrefix(proxy.Dir, "/") + "/" + proxy.RestoreBundle, Data: []byte(tgz), Mode: 0o600},
		{Name: strings.TrimPrefix(proxy.Dir, "/") + "/" + proxy.RestoreTarget, Data: []byte(pgTimestamp(target)), Mode: 0o600},
	}, nil
}

// pgTimestamp renders t for recovery_target_time.
func pgTimestamp(t time.Time) string {
	return t.UTC().Format("2006-01-02 15:04:05.999999") + "+00"
}

// finishRecovery waits until a recovered clone has promoted, then replaces
// the recovery settings with the clone's own.
func (s *Service) finishRecovery(ctx context.Context, project, name string) error {
	wctx, cancel := context.WithTimeout(ctx, recoveryTimeout)
	defer cancel()
	for {
		out, err := s.execSQL(wctx, project, name, "postgres", "", "SELECT pg_is_in_recovery();")
		if err == nil && strings.TrimSpace(out) == "f" {
			break
		}
		select {
		case <-wctx.Done():
			return newOpError("INTERNAL_ERROR", "point-in-time recovery did not finish: %v", err)
		case <-time.After(200 * time.Millisecond):
		}
	}
	rec, err := s.loadRecord(project, name)
	if err != nil {
		return err
	}
	want := serverFlags(rec.Instance.Settings)
	q := "ALTER SYSTEM RESET restore_command;\nALTER SYSTEM RESET recovery_target_time;\nALTER SYSTEM RESET recovery_target_action;\n" +
		alterSystemSQL(nil, pgSettings(want))
	if _, err := s.execSQL(ctx, project, name, "postgres", "", q); err != nil {
		return err
	}
	if needsRestart(nil, want) {
		return s.restartContainer(ctx, project, name)
	}
	_, err = s.execSQL(ctx, project, name, "postgres", "", "SELECT pg_reload_conf();")
	return err
}

// recoveredAPIState is the source's users and databases that exist in the
// recovered clone, plus the clone's other databases.
func (s *Service) recoveredAPIState(ctx context.Context, project, src, dest string) ([]*sqladmin.Database, []*userRecord, error) {
	dbOut, err := s.execSQL(ctx, project, dest, "postgres", "", "SELECT datname FROM pg_database WHERE NOT datistemplate;")
	if err != nil {
		return nil, nil, err
	}
	roleOut, err := s.execSQL(ctx, project, dest, "postgres", "", "SELECT rolname FROM pg_roles;")
	if err != nil {
		return nil, nil, err
	}
	present := func(out string) map[string]bool {
		m := map[string]bool{}
		for _, l := range strings.Split(out, "\n") {
			if l = strings.TrimSpace(l); l != "" {
				m[l] = true
			}
		}
		return m
	}
	haveDB, haveRole := present(dbOut), present(roleOut)
	srcDBs, srcUsers := s.snapshotAPIState(project, src)
	var dbs []*sqladmin.Database
	for _, d := range srcDBs {
		if haveDB[d.Name] {
			dbs = append(dbs, d)
			delete(haveDB, d.Name)
		}
	}
	for n := range haveDB {
		dbs = append(dbs, &sqladmin.Database{Name: n})
	}
	sort.Slice(dbs, func(i, j int) bool { return dbs[i].Name < dbs[j].Name })
	var users []*userRecord
	for _, u := range srcUsers {
		if haveRole[u.User.Name] {
			users = append(users, u)
		}
	}
	return dbs, users, nil
}

// pointInTime parses a clone's target time.
func pointInTime(cc *sqladmin.CloneContext) (time.Time, error) {
	if cc.PointInTime == "" {
		return time.UnixMilli(cc.PitrTimestampMs).UTC(), nil
	}
	t, err := time.Parse(time.RFC3339Nano, cc.PointInTime)
	if err != nil {
		return time.Time{}, errInvalid("Invalid value for cloneContext.pointInTime: %q is not an RFC 3339 timestamp.", cc.PointInTime)
	}
	return t.UTC(), nil
}

func (s *Service) getLatestRecoveryTime(w http.ResponseWriter, r *http.Request) {
	rec, err := s.instanceFor(r, "cloudsql.instances.get")
	if err != nil {
		apierr.Write(w, err)
		return
	}
	if !pitrEnabled(rec.Instance) {
		apierr.Write(w, errPITRDisabled(rec.Instance.Name))
		return
	}
	if err := requireRunnable(rec); err != nil {
		apierr.Write(w, err)
		return
	}
	earliest, latest, err := s.recoveryWindow(r.Context(), rec.Instance.Project, rec.Instance.Name)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	writeJSON(w, r, &sqladmin.SqlInstancesGetLatestRecoveryTimeResponse{
		Kind:                 "sql#getLatestRecoveryTime",
		EarliestRecoveryTime: earliest.Format(time.RFC3339Nano),
		LatestRecoveryTime:   latest.Format(time.RFC3339Nano),
	})
}
