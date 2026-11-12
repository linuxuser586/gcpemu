package sql_test

import (
	"compress/gzip"
	"context"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	sqladmin "google.golang.org/api/sqladmin/v1beta4"

	"github.com/linuxuser586/gcpemu/emutest"
	"github.com/linuxuser586/gcpemu/internal/instance"
	"github.com/linuxuser586/gcpemu/services/sql"
)

// requireGzip fails unless file starts with the gzip magic bytes.
func requireGzip(t *testing.T, file string) {
	t.Helper()
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) < 2 || b[0] != 0x1f || b[1] != 0x8b {
		t.Fatalf("%s is not gzipped", file)
	}
}

// gunzipInPlace replaces a gzipped file with its plain content under the
// name without .gz.
func gunzipInPlace(t *testing.T, file string) {
	t.Helper()
	f, err := os.Open(file)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	out, err := os.Create(strings.TrimSuffix(file, ".gz"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, zr); err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
}

// TestGzippedDumps covers #1043: backups and State snapshots are written
// as gzipped dumps that restore into the same and another Cloud SQL
// instance, and the plain dumps written before still restore, a Retained
// backup's too.
func TestGzippedDumps(t *testing.T) {
	emutest.RequireRuntime(t)
	t.Parallel()
	inst := emutest.Start(t, []string{"sql"})
	svc := adminClient(t, inst)
	sqlSvc := serviceNamed(t, inst, "sql")
	ctx := context.Background()
	ipc := func() *sqladmin.IpConfiguration {
		return &sqladmin.IpConfiguration{Ipv4Enabled: true, AuthorizedNetworks: []*sqladmin.AclEntry{{Value: "0.0.0.0/0"}}}
	}
	createInstance(t, svc, &sqladmin.DatabaseInstance{
		Name: "gz", DatabaseVersion: "POSTGRES_18", RootPassword: "rootpw",
		Settings: &sqladmin.Settings{IpConfiguration: ipc(), RetainBackupsOnDelete: true},
	})
	createInstance(t, svc, &sqladmin.DatabaseInstance{
		Name: "other", DatabaseVersion: "POSTGRES_18", RootPassword: "rootpw", Settings: &sqladmin.Settings{IpConfiguration: ipc()},
	})
	waitOp(t, svc, must(svc.Databases.Insert(testProject, "gz", &sqladmin.Database{Name: "app"}).Do()))
	exec := func(name, q string) {
		t.Helper()
		c := connectHost(t, inst, name, "postgres", "rootpw", "app")
		defer c.Close(ctx)
		if _, err := c.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	rows := func(name string) int {
		t.Helper()
		c := connectHost(t, inst, name, "postgres", "rootpw", "app")
		defer c.Close(ctx)
		return count(t, c, "SELECT count(*) FROM items")
	}
	restore := func(into string, req *sqladmin.InstancesRestoreBackupRequest) {
		t.Helper()
		waitOp(t, svc, must(svc.Instances.RestoreBackup(testProject, into, req).Do()))
	}
	exec("gz", "CREATE TABLE items (id int PRIMARY KEY); INSERT INTO items SELECT generate_series(1, 5)")

	waitOp(t, svc, must(svc.BackupRuns.Insert(testProject, "gz", &sqladmin.BackupRun{}).Do()))
	runs := must(svc.BackupRuns.List(testProject, "gz").Do()).Items
	if len(runs) != 1 {
		t.Fatalf("backup runs: %+v, want 1", runs)
	}
	id := runs[0].Id
	file := sql.BackupFile(sqlSvc, testProject, id)
	if filepath.Base(file) != strconv.FormatInt(id, 10)+".sql.gz" {
		t.Errorf("backup file %s, want <id>.sql.gz", file)
	}
	requireGzip(t, file)

	t.Run("gzipped On-demand backup into the same Cloud SQL instance", func(t *testing.T) {
		exec("gz", "INSERT INTO items SELECT generate_series(6, 8)")
		restore("gz", &sqladmin.InstancesRestoreBackupRequest{RestoreBackupContext: &sqladmin.RestoreBackupContext{BackupRunId: id}})
		if n := rows("gz"); n != 5 {
			t.Errorf("rows after the restore: %d, want 5", n)
		}
	})
	t.Run("gzipped On-demand backup into another Cloud SQL instance", func(t *testing.T) {
		restore("other", &sqladmin.InstancesRestoreBackupRequest{RestoreBackupContext: &sqladmin.RestoreBackupContext{BackupRunId: id, InstanceId: "gz"}})
		if n := rows("other"); n != 5 {
			t.Errorf("rows after the restore: %d, want 5", n)
		}
	})

	t.Run("State snapshot", func(t *testing.T) {
		if _, err := inst.Internal().SaveSnapshot(ctx, "gz"); err != nil {
			t.Fatal(err)
		}
		dumps := filepath.Join(inst.Internal().Dir, instance.SnapshotsDir, "gz", "services", "sql", "dumps", testProject)
		requireGzip(t, filepath.Join(dumps, "gz.sql.gz"))
		exec("gz", "INSERT INTO items SELECT generate_series(6, 8)")
		if err := inst.Internal().RestoreSnapshot(ctx, "gz"); err != nil {
			t.Fatal(err)
		}
		if n := rows("gz"); n != 5 {
			t.Errorf("rows after restoring the State snapshot: %d, want 5", n)
		}

		// A State snapshot from before #1043 holds plain <name>.sql dumps.
		gunzipInPlace(t, filepath.Join(dumps, "gz.sql.gz"))
		exec("gz", "INSERT INTO items SELECT generate_series(6, 8)")
		if err := inst.Internal().RestoreSnapshot(ctx, "gz"); err != nil {
			t.Fatal(err)
		}
		if n := rows("gz"); n != 5 {
			t.Errorf("rows after restoring the plain State snapshot: %d, want 5", n)
		}
	})

	// A backup from before #1043 has a plain <id>.sql dump.
	if err := sql.MakePlainBackup(sqlSvc, testProject, id); err != nil {
		t.Fatal(err)
	}
	t.Run("plain On-demand backup", func(t *testing.T) {
		exec("other", "INSERT INTO items SELECT generate_series(6, 8)")
		restore("other", &sqladmin.InstancesRestoreBackupRequest{RestoreBackupContext: &sqladmin.RestoreBackupContext{BackupRunId: id, InstanceId: "gz"}})
		if n := rows("other"); n != 5 {
			t.Errorf("rows after the restore: %d, want 5", n)
		}
	})
	t.Run("plain Retained backup", func(t *testing.T) {
		waitOp(t, svc, must(svc.Instances.Delete(testProject, "gz").Do()))
		file := sql.BackupFile(sqlSvc, testProject, id)
		if !strings.HasSuffix(file, ".sql") || strings.Contains(file, string(filepath.Separator)+"gz"+string(filepath.Separator)) {
			t.Errorf("Retained backup's file %s, want a .sql moved to its new owner", file)
		}
		exec("other", "INSERT INTO items SELECT generate_series(6, 8)")
		restore("other", &sqladmin.InstancesRestoreBackupRequest{Backup: "projects/" + testProject + "/backups/" + strconv.FormatInt(id, 10)})
		if n := rows("other"); n != 5 {
			t.Errorf("rows after restoring the Retained backup: %d, want 5", n)
		}
	})
}
