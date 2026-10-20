package sql_test

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	sqladmin "google.golang.org/api/sqladmin/v1beta4"

	"github.com/linuxuser586/gcpemu/emutest"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/services/sql"
	"github.com/linuxuser586/gcpemu/services/sql/proxy"
)

// TestBackupsAPIAndPointInTimeClone covers FR-SQL-008: the backups API
// (create, list, get, update, delete), restoring a backup by name, and a
// clone of an instance as of a point in time, replayed from archived WAL,
// and transaction log retention pruning the WAL archive (#365).
func TestBackupsAPIAndPointInTimeClone(t *testing.T) {
	emutest.RequireRuntime(t)
	t.Parallel()
	inst := emutest.Start(t, []string{"sql"})
	svc := adminClient(t, inst)
	ctx := context.Background()
	open := []*sqladmin.AclEntry{{Value: "0.0.0.0/0"}}
	settings := func() *sqladmin.Settings {
		return &sqladmin.Settings{
			IpConfiguration:     &sqladmin.IpConfiguration{Ipv4Enabled: true, AuthorizedNetworks: open},
			BackupConfiguration: &sqladmin.BackupConfiguration{Enabled: true, PointInTimeRecoveryEnabled: true},
		}
	}
	createInstance(t, svc, &sqladmin.DatabaseInstance{
		Name: "pitr", DatabaseVersion: "POSTGRES_18", RootPassword: "rootpw", Settings: settings(),
	})
	waitOp(t, svc, must(svc.Databases.Insert(testProject, "pitr", &sqladmin.Database{Name: "app"}).Do()))
	exec := func(instance, q string) {
		t.Helper()
		c := connectHost(t, inst, instance, "postgres", "rootpw", "app")
		defer c.Close(ctx)
		if _, err := c.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	rows := func(instance string) int {
		t.Helper()
		c := connectHost(t, inst, instance, "postgres", "rootpw", "app")
		defer c.Close(ctx)
		return count(t, c, "SELECT count(*) FROM items")
	}
	exec("pitr", "CREATE TABLE items (id int PRIMARY KEY); INSERT INTO items SELECT generate_series(1, 5)")

	// Backups API.
	parent := "projects/" + testProject
	op := waitOp(t, svc, must(svc.Backups.CreateBackup(parent, &sqladmin.Backup{Instance: "pitr", Description: "five rows"}).Do()))
	if op.BackupContext == nil || op.BackupContext.Name == "" {
		t.Fatalf("operation names no backup: %+v", op.BackupContext)
	}
	name := op.BackupContext.Name
	list := must(svc.Backups.ListBackups(parent).Do())
	if len(list.Backups) != 1 || list.Backups[0].Name != name || list.Backups[0].State != "SUCCESSFUL" || list.Backups[0].Instance != "pitr" {
		t.Fatalf("backups = %+v", list.Backups)
	}
	if runs := must(svc.BackupRuns.List(testProject, "pitr").Do()); len(runs.Items) != 1 || name != parent+"/backups/"+strconv.FormatInt(runs.Items[0].Id, 10) {
		t.Fatalf("backup runs = %+v, backup %s", runs.Items, name)
	}
	waitOp(t, svc, must(svc.Backups.UpdateBackup(name, &sqladmin.Backup{Description: "renamed"}).UpdateMask("description").Do()))
	if b := must(svc.Backups.GetBackup(name).Do()); b.Description != "renamed" || b.BackupInterval == nil || b.BackupInterval.EndTime == "" {
		t.Fatalf("backup = %+v", b)
	}

	// Transaction log retention (7 days by default). The Base backups taken
	// on create and with the backup are back-dated to 10 and 8 days ago;
	// the next Base backup prunes the first and the WAL before the second,
	// the anchor (ADR 0022: the WAL archive is aged directly).
	sqlSvc := serviceNamed(t, inst, "sql")
	bases, wal, err := sql.WALArchive(ctx, sqlSvc, testProject, "pitr")
	if err != nil || len(bases) != 2 || bases[0].Start == "" || bases[1].Start == "" {
		t.Fatalf("WAL archive before back-dating = %+v, %v", bases, err)
	}
	anchor := bases[1]
	if !hasSegmentBefore(wal, anchor.Start) {
		t.Fatalf("no archived WAL before the anchor's start %s: %v", anchor.Start, wal)
	}
	now := time.Now()
	backdate := func(b sql.BaseBackup, days int) int64 {
		t.Helper()
		end := now.AddDate(0, 0, -days).UnixMicro()
		if _, err := sql.ExecIn(ctx, sqlSvc, testProject, "pitr", "mv",
			proxy.ArchiveDir+"/base/"+strconv.FormatInt(b.End, 10), proxy.ArchiveDir+"/base/"+strconv.FormatInt(end, 10)); err != nil {
			t.Fatal(err)
		}
		return end
	}
	backdate(bases[0], 10)
	anchor.End = backdate(anchor, 8)
	second := waitOp(t, svc, must(svc.Backups.CreateBackup(parent, &sqladmin.Backup{Instance: "pitr"}).Do())).BackupContext.Name
	bases, wal, err = sql.WALArchive(ctx, sqlSvc, testProject, "pitr")
	if err != nil || len(bases) != 2 || bases[0] != anchor {
		t.Fatalf("Base backups after pruning = %+v, %v; want the anchor %+v and the new one", bases, err, anchor)
	}
	if hasSegmentBefore(wal, anchor.Start) {
		t.Fatalf("WAL before the anchor's start %s survived pruning: %v", anchor.Start, wal)
	}
	if rt := must(svc.Projects.Instances.GetLatestRecoveryTime(testProject, "pitr").Do()); !parseTime(t, rt.EarliestRecoveryTime).Equal(time.UnixMicro(anchor.End)) {
		t.Fatalf("earliestRecoveryTime = %s, want the anchor's end %s", rt.EarliestRecoveryTime, time.UnixMicro(anchor.End).UTC().Format(time.RFC3339Nano))
	}

	// Point in time: five rows, then five more.
	exec("pitr", "SELECT 1") // a separate commit before the target
	rt := must(svc.Projects.Instances.GetLatestRecoveryTime(testProject, "pitr").Do())
	if !parseTime(t, rt.LatestRecoveryTime).After(parseTime(t, rt.EarliestRecoveryTime)) {
		t.Fatalf("recovery window = %+v", rt)
	}
	target := rt.LatestRecoveryTime
	time.Sleep(50 * time.Millisecond)
	exec("pitr", "INSERT INTO items SELECT generate_series(6, 10)")
	if n := rows("pitr"); n != 10 {
		t.Fatalf("source rows = %d", n)
	}
	waitOp(t, svc, must(svc.Instances.Clone(testProject, "pitr", &sqladmin.InstancesCloneRequest{CloneContext: &sqladmin.CloneContext{
		DestinationInstanceName: "pitr-clone", PointInTime: target,
	}}).Do()))
	if n := rows("pitr-clone"); n != 5 {
		t.Fatalf("rows in the point-in-time clone = %d, want 5", n)
	}
	// The bundle held the WAL from the newest Base backup's start on, and
	// its temporary file is gone.
	bases, _, err = sql.WALArchive(ctx, sqlSvc, testProject, "pitr")
	if err != nil || len(bases) != 2 {
		t.Fatalf("source Base backups = %+v, %v", bases, err)
	}
	out, err := sql.ExecIn(ctx, sqlSvc, testProject, "pitr-clone", "ls", "-1", proxy.Dir+"/restore/wal")
	if err != nil || strings.TrimSpace(out) == "" {
		t.Fatalf("WAL in the clone's restore bundle: %q, %v", out, err)
	}
	if got := strings.Fields(out); hasSegmentBefore(got, bases[1].Start) {
		t.Fatalf("restore bundle WAL %v holds segments before the Base backup's start %s", got, bases[1].Start)
	}
	if left, _ := filepath.Glob(filepath.Join(os.TempDir(), "gcpemu-sql-restore-*")); len(left) != 0 {
		t.Fatalf("restore bundle temporary files left: %v", left)
	}
	if dbs := must(svc.Databases.List(testProject, "pitr-clone").Do()); len(dbs.Items) != 2 {
		t.Fatalf("clone databases = %+v", dbs.Items)
	}
	// The clone is writable and keeps its own recovery window.
	exec("pitr-clone", "INSERT INTO items VALUES (100)")
	if rt := must(svc.Projects.Instances.GetLatestRecoveryTime(testProject, "pitr-clone").Do()); rt.EarliestRecoveryTime == "" {
		t.Fatalf("clone recovery window = %+v", rt)
	}

	// Point-in-time recovery turned off while the clone is stopped takes
	// effect when it starts again.
	patch := func(st *sqladmin.Settings) {
		t.Helper()
		waitOp(t, svc, must(svc.Instances.Patch(testProject, "pitr-clone", &sqladmin.DatabaseInstance{Settings: st}).Do()))
	}
	patch(&sqladmin.Settings{ActivationPolicy: "NEVER"})
	patch(&sqladmin.Settings{BackupConfiguration: &sqladmin.BackupConfiguration{
		Enabled: true, PointInTimeRecoveryEnabled: false, ForceSendFields: []string{"PointInTimeRecoveryEnabled"},
	}})
	patch(&sqladmin.Settings{ActivationPolicy: "ALWAYS"})
	c := connectHost(t, inst, "pitr-clone", "postgres", "rootpw", "app")
	var mode string
	if err := c.QueryRow(ctx, "SHOW archive_mode").Scan(&mode); err != nil || mode != "off" {
		t.Fatalf("archive_mode after turning PITR off while stopped = %q, %v", mode, err)
	}
	c.Close(ctx)
	if _, err := svc.Projects.Instances.GetLatestRecoveryTime(testProject, "pitr-clone").Do(); err == nil {
		t.Fatal("recovery window reported with PITR off")
	}

	// Restore by backup name.
	exec("pitr", "DELETE FROM items")
	waitOp(t, svc, must(svc.Instances.RestoreBackup(testProject, "pitr", &sqladmin.InstancesRestoreBackupRequest{Backup: name}).Do()))
	if n := rows("pitr"); n != 5 {
		t.Fatalf("rows after restoring %s = %d, want 5", name, n)
	}
	// The restored cluster starts a new recovery window.
	if rt := must(svc.Projects.Instances.GetLatestRecoveryTime(testProject, "pitr").Do()); !parseTime(t, rt.EarliestRecoveryTime).After(parseTime(t, target)) {
		t.Fatalf("recovery window after restore = %+v, want it to start after %s", rt, target)
	}

	waitOp(t, svc, must(svc.Backups.DeleteBackup(name).Do()))
	waitOp(t, svc, must(svc.Backups.DeleteBackup(second).Do()))
	if list := must(svc.Backups.ListBackups(parent).Do()); len(list.Backups) != 0 {
		t.Fatalf("backups after delete = %+v", list.Backups)
	}
}

func parseTime(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// hasSegmentBefore reports whether wal holds a segment before start.
func hasSegmentBefore(wal []string, start string) bool {
	for _, f := range wal {
		if sql.SegmentBefore(f, start) {
			return true
		}
	}
	return false
}

func serviceNamed(t *testing.T, inst *emutest.Instance, name string) emu.Service {
	t.Helper()
	for _, s := range inst.Internal().Services() {
		if s.Name() == name {
			return s
		}
	}
	t.Fatalf("no %s service", name)
	return nil
}
