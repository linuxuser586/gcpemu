package sql_test

import (
	"context"
	"testing"

	sqladmin "google.golang.org/api/sqladmin/v1beta4"

	"github.com/linuxuser586/gcpemu/emutest"
)

// TestRetainedBackupRestoresADeletedInstance covers FR-SQL-008's Retained
// backups: deleting a Cloud SQL instance with retainBackupsOnDelete keeps
// its on-demand backup, which survives a State snapshot and its restore
// and then restores the deleted instance's data into another instance.
func TestRetainedBackupRestoresADeletedInstance(t *testing.T) {
	emutest.RequireRuntime(t)
	t.Parallel()
	inst := emutest.Start(t, []string{"sql"})
	svc := adminClient(t, inst)
	ctx := context.Background()
	ipc := func() *sqladmin.IpConfiguration {
		return &sqladmin.IpConfiguration{Ipv4Enabled: true, AuthorizedNetworks: []*sqladmin.AclEntry{{Value: "0.0.0.0/0"}}}
	}
	createInstance(t, svc, &sqladmin.DatabaseInstance{
		Name: "src", DatabaseVersion: "POSTGRES_18", RootPassword: "rootpw",
		Settings: &sqladmin.Settings{IpConfiguration: ipc(), RetainBackupsOnDelete: true},
	})
	if !must(svc.Instances.Get(testProject, "src").Do()).Settings.RetainBackupsOnDelete {
		t.Fatal("retainBackupsOnDelete not stored")
	}
	waitOp(t, svc, must(svc.Databases.Insert(testProject, "src", &sqladmin.Database{Name: "app"}).Do()))
	c := connectHost(t, inst, "src", "postgres", "rootpw", "app")
	if _, err := c.Exec(ctx, "CREATE TABLE items (id int PRIMARY KEY); INSERT INTO items SELECT generate_series(1, 5)"); err != nil {
		t.Fatal(err)
	}
	c.Close(ctx)
	waitOp(t, svc, must(svc.BackupRuns.Insert(testProject, "src", &sqladmin.BackupRun{Description: "before delete"}).Do()))

	waitOp(t, svc, must(svc.Instances.Delete(testProject, "src").Do()))
	assertNoRuntimeObjects(t, inst, "projects/"+testProject+"/instances/src")

	if _, err := inst.SaveSnapshot(ctx, "retained"); err != nil {
		t.Fatal(err)
	}
	if err := inst.RestoreSnapshot(ctx, "retained"); err != nil {
		t.Fatal(err)
	}

	backups := must(svc.Backups.ListBackups("projects/" + testProject).Do()).Backups
	if len(backups) != 1 {
		t.Fatalf("backups after the delete and a State snapshot: %+v, want the Retained backup", backups)
	}
	b := backups[0]
	if b.Type != "ON_DEMAND" || b.State != "SUCCESSFUL" || b.Description != "before delete" || b.InstanceDeletionTime == "" || b.InstanceSettings == nil {
		t.Errorf("Retained backup: type %s state %s description %q instanceDeletionTime %q instanceSettings %v",
			b.Type, b.State, b.Description, b.InstanceDeletionTime, b.InstanceSettings != nil)
	}

	createInstance(t, svc, &sqladmin.DatabaseInstance{
		Name: "dest", DatabaseVersion: "POSTGRES_18", RootPassword: "rootpw", Settings: &sqladmin.Settings{IpConfiguration: ipc()},
	})
	waitOp(t, svc, must(svc.Instances.RestoreBackup(testProject, "dest", &sqladmin.InstancesRestoreBackupRequest{Backup: b.Name}).Do()))
	c = connectHost(t, inst, "dest", "postgres", "rootpw", "app")
	defer c.Close(ctx)
	if n := count(t, c, "SELECT count(*) FROM items"); n != 5 {
		t.Errorf("rows after restoring the Retained backup: %d, want 5", n)
	}
}
