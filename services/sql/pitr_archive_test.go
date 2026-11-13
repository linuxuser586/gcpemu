package sql_test

import (
	"context"
	"strings"
	"testing"
	"time"

	sqladmin "google.golang.org/api/sqladmin/v1beta4"

	"github.com/linuxuser586/gcpemu/emutest"
	"github.com/linuxuser586/gcpemu/services/sql"
	"github.com/linuxuser586/gcpemu/services/sql/proxy"
)

// TestCompressedWALArchive covers #1048 on PostgreSQL 14 (the PITR test
// covers 18): the archive command run in the image gzips a segment
// crash-safely and re-archives idempotently, and an instance that was
// archiving with the old uncompressed command gets the new one when its
// container restarts, and a clone recovers from archived WAL that mixes
// uncompressed and gzipped segments.
func TestCompressedWALArchive(t *testing.T) {
	emutest.RequireRuntime(t)
	t.Parallel()
	inst := emutest.Start(t, []string{"sql"})
	svc := adminClient(t, inst)
	ctx := context.Background()
	createInstance(t, svc, &sqladmin.DatabaseInstance{
		Name: "walgz", DatabaseVersion: "POSTGRES_14", RootPassword: "rootpw",
		Settings: &sqladmin.Settings{
			IpConfiguration:     &sqladmin.IpConfiguration{Ipv4Enabled: true, AuthorizedNetworks: []*sqladmin.AclEntry{{Value: "0.0.0.0/0"}}},
			BackupConfiguration: &sqladmin.BackupConfiguration{Enabled: true, PointInTimeRecoveryEnabled: true},
		},
	})
	waitOp(t, svc, must(svc.Databases.Insert(testProject, "walgz", &sqladmin.Database{Name: "app"}).Do()))
	sqlSvc := serviceNamed(t, inst, "sql")
	sh := func(script string) error {
		_, err := sql.ExecIn(ctx, sqlSvc, testProject, "walgz", "sh", "-c", script)
		return err
	}
	out := func(script string) string {
		t.Helper()
		o, err := sql.ExecIn(ctx, sqlSvc, testProject, "walgz", "sh", "-c", script)
		if err != nil {
			t.Fatalf("%s: %v", script, err)
		}
		return strings.TrimSpace(o)
	}

	t.Run("archive command", func(t *testing.T) {
		// The command run as PostgreSQL runs it, into a scratch directory.
		archive := func(seg, src string) error {
			cmd := strings.ReplaceAll(sql.ArchiveCommand, proxy.ArchiveDir+"/wal", "/tmp/w")
			cmd = strings.ReplaceAll(cmd, "%f", seg)
			return sh(strings.ReplaceAll(cmd, "%p", src))
		}
		out("rm -rf /tmp/w && mkdir /tmp/w && head -c 65536 /dev/urandom > /tmp/a && head -c 65536 /dev/urandom > /tmp/b")
		const seg, old = "000000010000000000000042", "000000010000000000000041"
		if err := archive(seg, "/tmp/a"); err != nil {
			t.Fatalf("first archive: %v", err)
		}
		if got := out("ls -A /tmp/w"); got != seg+".gz" {
			t.Fatalf("scratch directory holds %q, want only %s.gz (no temporary file)", got, seg)
		}
		if err := sh("gzip -dc /tmp/w/" + seg + ".gz | cmp -s - /tmp/a"); err != nil {
			t.Fatalf("%s.gz does not decompress to the segment: %v", seg, err)
		}
		if err := archive(seg, "/tmp/a"); err != nil {
			t.Fatalf("identical re-archive with .gz present: %v", err)
		}
		if err := archive(seg, "/tmp/b"); err == nil {
			t.Fatal("re-archive of different content with .gz present succeeded")
		}
		if err := sh("gzip -dc /tmp/w/" + seg + ".gz | cmp -s - /tmp/a"); err != nil {
			t.Fatalf("a refused re-archive changed %s.gz: %v", seg, err)
		}
		// A segment archived uncompressed by the old command.
		out("cp /tmp/a /tmp/w/" + old)
		if err := archive(old, "/tmp/a"); err != nil {
			t.Fatalf("identical re-archive with an uncompressed file present: %v", err)
		}
		if err := archive(old, "/tmp/b"); err == nil {
			t.Fatal("re-archive of different content with an uncompressed file present succeeded")
		}
		if err := sh("cmp -s /tmp/w/" + old + " /tmp/a"); err != nil {
			t.Fatalf("a refused re-archive changed %s: %v", old, err)
		}
		if got := strings.Fields(out("ls -A /tmp/w")); len(got) != 2 {
			t.Fatalf("scratch directory holds %v, want %s and %s.gz only", got, old, seg)
		}
		out("rm -rf /tmp/w /tmp/a /tmp/b")
	})

	t.Run("upgrade from the uncompressed command", func(t *testing.T) {
		exec := func(instance, q string) {
			t.Helper()
			c := connectHost(t, inst, instance, "postgres", "rootpw", "app")
			defer c.Close(ctx)
			if _, err := c.Exec(ctx, q); err != nil {
				t.Fatalf("%s: %v", q, err)
			}
		}
		archiveNow := func() {
			t.Helper()
			if err := sql.ArchiveNow(ctx, sqlSvc, testProject, "walgz"); err != nil {
				t.Fatal(err)
			}
		}
		const oldCommand = "test ! -f " + proxy.ArchiveDir + "/wal/%f && cp %p " + proxy.ArchiveDir + "/wal/%f"
		if _, err := sql.ExecSQL(ctx, sqlSvc, testProject, "walgz",
			"ALTER SYSTEM SET archive_command = '"+oldCommand+"';\nSELECT pg_reload_conf();"); err != nil {
			t.Fatal(err)
		}
		exec("walgz", "CREATE TABLE items (id int PRIMARY KEY); INSERT INTO items SELECT generate_series(1, 5)")
		// The archiver picks the reloaded command up asynchronously.
		var wal []string
		for i := 0; ; i++ {
			archiveNow()
			var err error
			if _, wal, err = sql.WALArchive(ctx, sqlSvc, testProject, "walgz"); err != nil {
				t.Fatal(err)
			}
			if hasPlainSegment(wal) {
				break
			}
			if i == 10 {
				t.Fatalf("no uncompressed segment archived by the old command: %v", wal)
			}
			time.Sleep(200 * time.Millisecond)
		}

		// The loop ended on a WAL switch, so the current segment may be
		// empty, or the background writer may already have logged into it.
		// A record now makes it non-empty every run: the shutdown then
		// switches out of it and archives it with the uncompressed command.
		exec("walgz", "SELECT pg_logical_emit_message(false, 'gcpemu', 'x')")
		if err := sql.RestartContainer(ctx, sqlSvc, testProject, "walgz"); err != nil {
			t.Fatal(err)
		}
		// before marks the end of the restart: whatever the uncompressed
		// command archived until then, the shutdown included, stays
		// uncompressed.
		_, wal, err := sql.WALArchive(ctx, sqlSvc, testProject, "walgz")
		if err != nil {
			t.Fatal(err)
		}
		before := len(wal)
		if got, err := sql.ExecSQL(ctx, sqlSvc, testProject, "walgz", "SELECT current_setting('archive_command');"); err != nil || strings.TrimSpace(got) != sql.ArchiveCommand {
			t.Fatalf("archive_command after restart = %q, %v; want the compressing command", got, err)
		}
		// The first segment archived after the restart, with nothing else
		// in between, is compressed: a restart that returned before the
		// WAL archive command took effect would archive it uncompressed.
		archiveNow()
		if _, wal, err = sql.WALArchive(ctx, sqlSvc, testProject, "walgz"); err != nil || len(wal) <= before || !hasGzippedSegment(wal[before:]) || hasPlainSegment(wal[before:]) {
			t.Fatalf("first WAL archived after the restart = %v (of %v), %v; want only .gz segments", wal[min(before, len(wal)):], wal, err)
		}
		exec("walgz", "INSERT INTO items SELECT generate_series(6, 10)")
		archiveNow()
		bases, wal, err := sql.WALArchive(ctx, sqlSvc, testProject, "walgz")
		if err != nil || len(bases) == 0 || len(wal) <= before || !hasGzippedSegment(wal[before:]) || hasPlainSegment(wal[before:]) {
			t.Fatalf("WAL archived after the restart = %v (of %v), %v; want only .gz segments", wal[min(before, len(wal)):], wal, err)
		}

		// The clone replays from the Base backup taken on create, across
		// the uncompressed segments and the .gz ones after them.
		exec("walgz", "SELECT 1")
		target := must(svc.Projects.Instances.GetLatestRecoveryTime(testProject, "walgz").Do()).LatestRecoveryTime
		time.Sleep(50 * time.Millisecond)
		exec("walgz", "INSERT INTO items SELECT generate_series(11, 15)")
		waitOp(t, svc, must(svc.Instances.Clone(testProject, "walgz", &sqladmin.InstancesCloneRequest{CloneContext: &sqladmin.CloneContext{
			DestinationInstanceName: "walgz-clone", PointInTime: target,
		}}).Do()))
		c := connectHost(t, inst, "walgz-clone", "postgres", "rootpw", "app")
		defer c.Close(ctx)
		if n := count(t, c, "SELECT count(*) FROM items"); n != 10 {
			t.Fatalf("rows in the point-in-time clone = %d, want 10", n)
		}
	})
}

// hasGzippedSegment reports whether wal holds a WAL segment stored as
// <segment>.gz.
func hasGzippedSegment(wal []string) bool {
	for _, f := range wal {
		if seg, ok := strings.CutSuffix(f, ".gz"); ok && isSegment(seg) {
			return true
		}
	}
	return false
}

// hasPlainSegment reports whether wal holds a WAL segment stored
// uncompressed.
func hasPlainSegment(wal []string) bool {
	for _, f := range wal {
		if isSegment(f) {
			return true
		}
	}
	return false
}

// isSegment reports whether name is a WAL segment's file name (not a
// .partial, .backup or timeline history file).
func isSegment(name string) bool {
	return len(name) == 24 && strings.Trim(name, "0123456789ABCDEF") == ""
}
