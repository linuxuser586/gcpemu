package sql_test

import (
	"context"
	"testing"

	sqladmin "google.golang.org/api/sqladmin/v1beta4"

	"github.com/linuxuser586/gcpemu/emutest"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/services/sql"
)

// TestRefreshCountsAsMaintenance covers ADR 0084: an instance created
// before a Product image refresh runs the new minor when its container is
// recreated, and reports it in databaseInstalledVersion and
// maintenanceVersion.
func TestRefreshCountsAsMaintenance(t *testing.T) {
	emutest.RequireRuntime(t)
	t.Parallel()
	ctx := context.Background()
	inst := emutest.Start(t, []string{"sql"})
	var svcSQL emu.Service
	for _, s := range inst.Internal().Services() {
		if s.Name() == "sql" {
			svcSQL = s
		}
	}
	svc := adminClient(t, inst)
	createInstance(t, svc, &sqladmin.DatabaseInstance{Name: "old", DatabaseVersion: "POSTGRES_17", RootPassword: "rootpw"})
	t.Cleanup(func() { _, _ = svc.Instances.Delete(testProject, "old").Do() })

	if err := sql.SetInstalledVersion(svcSQL, testProject, "old", "POSTGRES_17_10", "POSTGRES_17_10.R20260801.00_00"); err != nil {
		t.Fatal(err)
	}
	// Remove the container so that instances.restart recreates it from the
	// version table, as an Instance start or a restore does.
	if err := sql.MarkFailed(ctx, svcSQL, testProject, "old"); err != nil {
		t.Fatal(err)
	}
	waitOp(t, svc, must(svc.Instances.Restart(testProject, "old").Do()))

	got := must(svc.Instances.Get(testProject, "old").Do())
	wantInstalled, wantMaintenance := sql.InstalledVersion("POSTGRES_17"), sql.MaintenanceVersion("POSTGRES_17")
	if got.DatabaseInstalledVersion != wantInstalled || got.MaintenanceVersion != wantMaintenance {
		t.Fatalf("after restart: databaseInstalledVersion %q, maintenanceVersion %q; want %s, %s",
			got.DatabaseInstalledVersion, got.MaintenanceVersion, wantInstalled, wantMaintenance)
	}
}

// TestInsertReportsInstalledVersions checks insert against Cloud SQL's
// database versions page and maintenance changelog (#895).
func TestInsertReportsInstalledVersions(t *testing.T) {
	inst := emutest.Start(t, []string{"sql"})
	svc := adminClient(t, inst)
	for dbv, want := range map[string]string{
		"POSTGRES_14": "POSTGRES_14_24",
		"POSTGRES_15": "POSTGRES_15_19",
		"POSTGRES_16": "POSTGRES_16_15",
		"POSTGRES_17": "POSTGRES_17_11",
		"POSTGRES_18": "POSTGRES_18_6",
	} {
		name := "v" + dbv[len("POSTGRES_"):]
		waitOp(t, svc, must(svc.Instances.Insert(testProject, &sqladmin.DatabaseInstance{Name: name, DatabaseVersion: dbv,
			Settings: &sqladmin.Settings{Tier: "db-f1-micro", Edition: "ENTERPRISE", ActivationPolicy: "NEVER"}}).Do()))
		got := must(svc.Instances.Get(testProject, name).Do())
		if got.DatabaseInstalledVersion != want || got.MaintenanceVersion != want+".R20261013.02_04" {
			t.Errorf("%s: databaseInstalledVersion %q, maintenanceVersion %q; want %s, %s.R20261013.02_04",
				dbv, got.DatabaseInstalledVersion, got.MaintenanceVersion, want, want)
		}
	}
}
