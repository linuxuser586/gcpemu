package sql_test

import (
	"context"
	"testing"
	"time"

	sqladmin "google.golang.org/api/sqladmin/v1beta4"

	"github.com/linuxuser586/gcpemu/emutest"
	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/runtime"
)

// TestPersistenceAcrossRestart covers FR-CORE-031 for Cloud SQL: with a
// data dir, rows survive an emulator restart (the container is re-created
// on the instance's named volume) and the instance comes back RUNNABLE.
func TestPersistenceAcrossRestart(t *testing.T) {
	emutest.RequireRuntime(t)
	t.Parallel()
	dir := t.TempDir()
	persistent := func(c *config.Config) { c.Ephemeral = false; c.DataDir = dir }
	ctx := context.Background()

	inst := emutest.Start(t, []string{"sql"}, persistent)
	rt, err := inst.Env.Containers.Runtime(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Never leave volumes behind, whatever happens.
		_ = rt.Cleanup(context.Background(), true)
		vols, _ := rt.ListVolumes(context.Background(), map[string]string{runtime.LabelInstance: rt.InstanceID})
		for _, v := range vols {
			_ = rt.RemoveVolume(context.Background(), v)
		}
	})
	svc := adminClient(t, inst)
	in := createInstance(t, svc, &sqladmin.DatabaseInstance{
		Name:         "keep",
		RootPassword: "rootpw",
		Settings: &sqladmin.Settings{IpConfiguration: &sqladmin.IpConfiguration{
			Ipv4Enabled: true, AuthorizedNetworks: []*sqladmin.AclEntry{{Value: "0.0.0.0/0"}},
		}},
	})
	conn, err := connect(ctx, publicIP(in), 5432, "postgres", "rootpw", "postgres")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, "CREATE TABLE kept (v text); INSERT INTO kept VALUES ('survives')"); err != nil {
		t.Fatal(err)
	}
	conn.Close(ctx)

	sctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	if err := inst.Shutdown(sctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	inst2 := emutest.Start(t, []string{"sql"}, persistent) // waits for Ready (instances recovered)
	svc2 := adminClient(t, inst2)
	in2, err := svc2.Instances.Get(testProject, "keep").Do()
	if err != nil {
		t.Fatal(err)
	}
	if in2.State != "RUNNABLE" {
		t.Fatalf("state after restart = %s", in2.State)
	}
	conn, err = connect(ctx, publicIP(in2), 5432, "postgres", "rootpw", "postgres")
	if err != nil {
		t.Fatal(err)
	}
	var v string
	if err := conn.QueryRow(ctx, "SELECT v FROM kept").Scan(&v); err != nil || v != "survives" {
		t.Fatalf("row after restart: %q %v", v, err)
	}
	conn.Close(ctx)
	waitOp(t, svc2, must(svc2.Instances.Delete(testProject, "keep").Do()))
	assertNoRuntimeObjects(t, inst2, "projects/"+testProject+"/instances/keep")
}
