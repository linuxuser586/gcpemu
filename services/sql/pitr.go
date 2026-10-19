package sql

import (
	"archive/tar"
	"context"
	"io"
	"net/http"
	"os"
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
// Transaction log retention (retention.go) prunes the WAL archive after
// every base backup and before the recovery window is read or a restore
// bundle is built, always under the instance's lock.
//
// A clone with pointInTime archives the source's current WAL segment, packs
// the newest base backup that ended at or before that time with the
// archived WAL from its start segment on into a restore bundle, and creates
// the clone with the bundle in its container: the agent lays the base
// backup out as the data directory and PostgreSQL replays the WAL up to the
// time and promotes (proxy/restore.go). The bundle is packed inside the
// source's container and streamed to a temporary file on the emulator's
// host, removed once the clone's container has it. The clone then gets its
// own settings and starts its own WAL history with a base backup of its
// own.

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

// takeBaseBackup stores a base backup in the archive volume and prunes the
// WAL archive. The caller holds the instance's lock.
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
	_, err := s.pruneArchive(ctx, project, name)
	return err
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

// walArchive is what an instance's WAL archive holds as of Now, on
// PostgreSQL's clock.
type walArchive struct {
	Bases []baseBackup // oldest first
	WAL   []string
	Now   time.Time
}

// listArchiveScript prints each base backup's directory name with the
// WAL-Ranges section of its backup_manifest, then the archived WAL.
const listArchiveScript = "cd " + proxy.ArchiveDir + "\n" +
	"for d in base/*/; do\n" +
	"  e=${d#base/}; e=${e%/}\n" +
	"  echo \"@base $e\"\n" +
	"  [ -f \"base/$e/backup_manifest\" ] && sed -n '/\"WAL-Ranges\"/,$p' \"base/$e/backup_manifest\"\n" +
	"done\n" +
	"echo @wal\n" +
	"ls -1 wal\n"

// readArchive lists the instance's WAL archive.
func (s *Service) readArchive(ctx context.Context, project, name string) (walArchive, error) {
	var a walArchive
	out, err := s.execIn(ctx, project, name, "", "sh", "-c", listArchiveScript)
	if err != nil {
		return a, err
	}
	clock, err := s.execSQL(ctx, project, name, "postgres", "",
		pgClockSQL+", setting FROM pg_settings WHERE name = 'wal_segment_size';")
	if err != nil {
		return a, err
	}
	now, segSize, ok := strings.Cut(strings.TrimSpace(clock), "|")
	n, err1 := strconv.ParseInt(now, 10, 64)
	seg, err2 := strconv.ParseUint(segSize, 10, 64)
	if !ok || err1 != nil || err2 != nil {
		return a, newOpError("INTERNAL_ERROR", "read PostgreSQL's clock: %q", clock)
	}
	a.Now = time.UnixMicro(n).UTC()
	var cur *baseBackup
	var ranges strings.Builder
	flush := func() {
		if cur != nil {
			cur.Start = manifestStart(ranges.String(), seg)
			a.Bases = append(a.Bases, *cur)
		}
		cur = nil
		ranges.Reset()
	}
	inWAL := false
	for _, l := range strings.Split(out, "\n") {
		switch {
		case inWAL:
			if l = strings.TrimSpace(l); l != "" {
				a.WAL = append(a.WAL, l)
			}
		case strings.HasPrefix(l, "@base "):
			flush()
			if e, err := strconv.ParseInt(strings.TrimPrefix(l, "@base "), 10, 64); err == nil {
				cur = &baseBackup{End: e}
			}
		case l == "@wal":
			flush()
			inWAL = true
		default:
			ranges.WriteString(l + "\n")
		}
	}
	sort.Slice(a.Bases, func(i, j int) bool { return a.Bases[i].End < a.Bases[j].End })
	return a, nil
}

// pruneArchive applies the instance's transaction log retention to its
// WAL archive and returns what is left. The caller holds the instance's
// lock.
func (s *Service) pruneArchive(ctx context.Context, project, name string) (walArchive, error) {
	rec, err := s.loadRecord(project, name)
	if err != nil {
		return walArchive{}, err
	}
	days := int64(7)
	if bc := rec.Instance.Settings.BackupConfiguration; bc != nil && bc.TransactionLogRetentionDays > 0 {
		days = bc.TransactionLogRetentionDays
	}
	a, err := s.readArchive(ctx, project, name)
	if err != nil {
		return a, err
	}
	p := planRetention(a.Bases, a.WAL, a.Now, days)
	if len(p.DropBase) == 0 && len(p.DropWAL) == 0 {
		return a, nil
	}
	var paths strings.Builder
	for _, b := range p.DropBase {
		paths.WriteString("base/" + strconv.FormatInt(b.End, 10) + "\n")
	}
	for _, f := range p.DropWAL {
		paths.WriteString("wal/" + f + "\n")
	}
	if _, err := s.execIn(ctx, project, name, paths.String(), "sh", "-c", "cd "+proxy.ArchiveDir+" && xargs rm -rf --"); err != nil {
		return a, newOpError("INTERNAL_ERROR", "prune WAL archive: %v", err)
	}
	a.Bases = p.Keep
	a.WAL, _ = walFrom(p.Keep[0].Start, a.WAL)
	return a, nil
}

// recoveryWindow prunes the WAL archive and returns the earliest and
// latest times the instance can be recovered to: the end of its oldest
// base backup and now.
func (s *Service) recoveryWindow(ctx context.Context, project, name string) (earliest, latest time.Time, err error) {
	mu := s.lock(project, name)
	defer mu.Unlock()
	a, err := s.pruneArchive(ctx, project, name)
	if err != nil {
		return
	}
	if len(a.Bases) == 0 {
		err = errInvalid("Instance %s has no base backup to recover from yet.", name)
		return
	}
	return time.UnixMicro(a.Bases[0].End).UTC(), a.Now, nil
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
// source's data as of target (proxy/restore.go), and a function that
// removes the bundle's temporary file once the container has them. It
// holds the source's lock, so that no prune removes what the bundle is
// built from.
func (s *Service) restoreBundle(ctx context.Context, project, src string, target time.Time) ([]runtime.File, func(), error) {
	mu := s.lock(project, src)
	defer mu.Unlock()
	if err := s.archiveNow(ctx, project, src); err != nil {
		return nil, nil, err
	}
	a, err := s.pruneArchive(ctx, project, src)
	if err != nil {
		return nil, nil, err
	}
	var base *baseBackup
	for i, b := range a.Bases {
		if b.End <= target.UnixMicro() {
			base = &a.Bases[i]
		}
	}
	if base == nil {
		return nil, nil, newOpError("INVALID_REQUEST", "No base backup of %s ends at or before %s.", src, target.Format(time.RFC3339Nano))
	}
	wal, _ := walFrom(base.Start, a.WAL)
	file, err := s.stageBundle(ctx, project, src, *base, wal)
	if err != nil {
		return nil, nil, err
	}
	return []runtime.File{
		{Name: strings.TrimPrefix(proxy.Dir, "/") + "/" + proxy.RestoreBundle, Path: file, Mode: 0o600},
		{Name: strings.TrimPrefix(proxy.Dir, "/") + "/" + proxy.RestoreTarget, Data: []byte(pgTimestamp(target)), Mode: 0o600},
	}, func() { _ = os.Remove(file) }, nil
}

// stagedBundle is where a restore bundle is packed in the source's archive
// volume before it is copied out.
const stagedBundle = proxy.ArchiveDir + "/.bundle.tar.gz"

// stageBundle packs a base backup and WAL files of the source into a
// gzipped tar inside its container and streams it to a temporary file on
// the emulator's host, whose path it returns.
func (s *Service) stageBundle(ctx context.Context, project, src string, base baseBackup, wal []string) (string, error) {
	rt, _, err := s.plane(ctx)
	if err != nil {
		return "", err
	}
	paths := "base/" + strconv.FormatInt(base.End, 10) + "\n"
	for _, f := range wal {
		paths += "wal/" + f + "\n"
	}
	if _, err := s.execIn(ctx, project, src, paths, "sh", "-c", "cd "+proxy.ArchiveDir+" && rm -f "+stagedBundle+" && tar -czf "+stagedBundle+" -T -"); err != nil {
		return "", newOpError("INTERNAL_ERROR", "pack WAL archive: %v", err)
	}
	defer func() { _, _ = s.execIn(context.WithoutCancel(ctx), project, src, "", "rm", "-f", stagedBundle) }()
	f, err := os.CreateTemp("", "gcpemu-sql-restore-*.tar.gz")
	if err != nil {
		return "", newOpError("INTERNAL_ERROR", "stage restore bundle: %v", err)
	}
	err = copyArchived(ctx, rt, containerName(rt, project, src), stagedBundle, f)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(f.Name())
		return "", newOpError("INTERNAL_ERROR", "copy restore bundle: %v", err)
	}
	return f.Name(), nil
}

// copyArchived streams the single file p in container id to w.
func copyArchived(ctx context.Context, rt *runtime.Manager, id, p string, w io.Writer) error {
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(rt.ArchiveFrom(ctx, id, p, pw)) }()
	defer pr.Close()
	tr := tar.NewReader(pr)
	h, err := tr.Next()
	if err != nil {
		return err
	}
	if h.Typeflag != tar.TypeReg {
		return os.ErrInvalid
	}
	if _, err := io.Copy(w, tr); err != nil {
		return err
	}
	_, err = io.Copy(io.Discard, pr)
	return err
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
