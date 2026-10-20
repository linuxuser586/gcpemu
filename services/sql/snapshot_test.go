package sql_test

import (
	"context"
	"testing"

	sqladmin "google.golang.org/api/sqladmin/v1beta4"

	"github.com/linuxuser586/gcpemu/emutest"
)

// TestStateSnapshot covers FR-CORE-012 for Cloud SQL: a State snapshot
// keeps an instance's Postgres data, users and backup runs, and restoring
// it brings the instance back RUNNABLE without anything done since.
func TestStateSnapshot(t *testing.T) {
	emutest.RequireRuntime(t)
	t.Parallel()
	ctx := context.Background()
	inst := emutest.Start(t, []string{"sql"})
	svc := adminClient(t, inst)
	in := createInstance(t, svc, &sqladmin.DatabaseInstance{
		Name:         "snap",
		RootPassword: "rootpw",
		Settings: &sqladmin.Settings{IpConfiguration: &sqladmin.IpConfiguration{
			Ipv4Enabled: true, AuthorizedNetworks: []*sqladmin.AclEntry{{Value: "0.0.0.0/0"}},
		}},
	})
	exec := func(in *sqladmin.DatabaseInstance, q string) {
		t.Helper()
		conn := connectHost(t, inst, in.Name, "postgres", "rootpw", "postgres")
		defer conn.Close(ctx)
		if _, err := conn.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	exec(in, "CREATE TABLE kept (v text); INSERT INTO kept VALUES ('before')")
	waitOp(t, svc, must(svc.BackupRuns.Insert(testProject, "snap", &sqladmin.BackupRun{Description: "kept"}).Do()))

	if _, err := inst.Internal().SaveSnapshot(ctx, "fixture"); err != nil {
		t.Fatal(err)
	}

	exec(in, "INSERT INTO kept VALUES ('after')")
	waitOp(t, svc, must(svc.Users.Insert(testProject, "snap", &sqladmin.User{Name: "later", Password: "pw"}).Do()))

	if err := inst.Internal().RestoreSnapshot(ctx, "fixture"); err != nil {
		t.Fatal(err)
	}
	in = must(svc.Instances.Get(testProject, "snap").Do())
	if in.State != "RUNNABLE" {
		t.Fatalf("state after restore = %s", in.State)
	}
	conn := connectHost(t, inst, in.Name, "postgres", "rootpw", "postgres")
	var rows []string
	r, err := conn.Query(ctx, "SELECT v FROM kept ORDER BY v")
	if err != nil {
		t.Fatal(err)
	}
	for r.Next() {
		var v string
		_ = r.Scan(&v)
		rows = append(rows, v)
	}
	r.Close()
	if len(rows) != 1 || rows[0] != "before" {
		t.Errorf("rows after restore = %q", rows)
	}
	var roles int
	if err := conn.QueryRow(ctx, "SELECT count(*) FROM pg_roles WHERE rolname = 'later'").Scan(&roles); err != nil || roles != 0 {
		t.Errorf("role made after the snapshot: %d %v", roles, err)
	}
	conn.Close(ctx)
	for _, u := range must(svc.Users.List(testProject, "snap").Do()).Items {
		if u.Name == "later" {
			t.Errorf("user made after the snapshot was restored")
		}
	}

	// The backup run and its file came back with the snapshot.
	runs := must(svc.BackupRuns.List(testProject, "snap").Do())
	if len(runs.Items) != 1 {
		t.Fatalf("backup runs after restore = %+v", runs.Items)
	}
	waitOp(t, svc, must(svc.Instances.RestoreBackup(testProject, "snap", &sqladmin.InstancesRestoreBackupRequest{
		RestoreBackupContext: &sqladmin.RestoreBackupContext{BackupRunId: runs.Items[0].Id},
	}).Do()))

	waitOp(t, svc, must(svc.Instances.Delete(testProject, "snap").Do()))
	assertNoRuntimeObjects(t, inst, "projects/"+testProject+"/instances/snap")
}
