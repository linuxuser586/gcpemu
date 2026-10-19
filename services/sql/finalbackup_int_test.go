package sql_test

import (
	"context"
	"fmt"
	"testing"

	sqladmin "google.golang.org/api/sqladmin/v1beta4"

	"github.com/linuxuser586/gcpemu/emutest"
)

// TestFinalBackupRestoresADeletedInstance covers FR-SQL-008's Final
// backups: deleting a running Cloud SQL instance with enableFinalBackup,
// and a stopped one whose patched finalBackupConfig takes one, each leaves a
// FINAL backup that restores its data into another instance.
func TestFinalBackupRestoresADeletedInstance(t *testing.T) {
	emutest.RequireRuntime(t)
	t.Parallel()
	inst := emutest.Start(t, []string{"sql"})
	svc := adminClient(t, inst)
	ctx := context.Background()
	open := []*sqladmin.AclEntry{{Value: "0.0.0.0/0"}}
	ipc := func() *sqladmin.IpConfiguration {
		return &sqladmin.IpConfiguration{Ipv4Enabled: true, AuthorizedNetworks: open}
	}
	withRows := func(name string, n int, settings *sqladmin.Settings) {
		t.Helper()
		settings.IpConfiguration = ipc()
		in := createInstance(t, svc, &sqladmin.DatabaseInstance{
			Name: name, DatabaseVersion: "POSTGRES_18", RootPassword: "rootpw", Settings: settings,
		})
		waitOp(t, svc, must(svc.Databases.Insert(testProject, name, &sqladmin.Database{Name: "app"}).Do()))
		c, err := connect(ctx, publicIP(in), 5432, "postgres", "rootpw", "app")
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close(ctx)
		if _, err := c.Exec(ctx, fmt.Sprintf("CREATE TABLE items (id int PRIMARY KEY); INSERT INTO items SELECT generate_series(1, %d)", n)); err != nil {
			t.Fatal(err)
		}
	}
	withRows("live", 3, &sqladmin.Settings{})
	withRows("stopped", 4, &sqladmin.Settings{})
	waitOp(t, svc, must(svc.Instances.Patch(testProject, "stopped", &sqladmin.DatabaseInstance{
		Settings: &sqladmin.Settings{ActivationPolicy: "NEVER", FinalBackupConfig: &sqladmin.FinalBackupConfig{Enabled: true, RetentionDays: 9}},
	}).Do()))
	if c := must(svc.Instances.Get(testProject, "stopped").Do()).Settings.FinalBackupConfig; c == nil || !c.Enabled || c.RetentionDays != 9 {
		t.Fatalf("patched finalBackupConfig %+v, want enabled with retentionDays 9", c)
	}

	waitOp(t, svc, must(svc.Instances.Delete(testProject, "live").EnableFinalBackup(true).FinalBackupDescription("live's last").Do()))
	waitOp(t, svc, must(svc.Instances.Delete(testProject, "stopped").Do()))

	parent := "projects/" + testProject
	finals := must(svc.Backups.ListBackups(parent).Filter(`type="FINAL"`).Do()).Backups
	byInstance := map[string]*sqladmin.Backup{}
	for _, b := range finals {
		byInstance[b.Instance] = b
	}
	if len(finals) != 2 || byInstance["live"] == nil || byInstance["stopped"] == nil {
		t.Fatalf("Final backups: %+v, want one each of live and stopped", finals)
	}
	for _, b := range finals {
		if b.State != "SUCCESSFUL" || b.InstanceDeletionTime == "" || b.ExpiryTime == "" {
			t.Errorf("Final backup of %s: state %s, instanceDeletionTime %q, expiryTime %q", b.Instance, b.State, b.InstanceDeletionTime, b.ExpiryTime)
		}
	}
	if s := byInstance["stopped"].InstanceSettings; s == nil || s.Settings.FinalBackupConfig == nil {
		t.Errorf("instanceSettings of stopped's Final backup %+v, want its settings", s)
	}
	if d := byInstance["live"].Description; d != "live's last" {
		t.Errorf("description %q, want the finalBackupDescription", d)
	}

	dest := createInstance(t, svc, &sqladmin.DatabaseInstance{
		Name: "dest", DatabaseVersion: "POSTGRES_18", RootPassword: "rootpw", Settings: &sqladmin.Settings{IpConfiguration: ipc()},
	})
	for name, want := range map[string]int{"live": 3, "stopped": 4} {
		waitOp(t, svc, must(svc.Instances.RestoreBackup(testProject, "dest", &sqladmin.InstancesRestoreBackupRequest{
			Backup: byInstance[name].Name,
		}).Do()))
		dest = must(svc.Instances.Get(testProject, "dest").Do())
		c, err := connect(ctx, publicIP(dest), 5432, "postgres", "rootpw", "app")
		if err != nil {
			t.Fatalf("connect after restoring %s's Final backup: %v", name, err)
		}
		if n := count(t, c, "SELECT count(*) FROM items"); n != want {
			t.Errorf("rows after restoring %s's Final backup: %d, want %d", name, n, want)
		}
		c.Close(ctx)
	}
	for _, name := range []string{"live", "stopped"} {
		assertNoRuntimeObjects(t, inst, "projects/"+testProject+"/instances/"+name)
	}
}
