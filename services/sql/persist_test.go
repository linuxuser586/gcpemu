package sql_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	container "cloud.google.com/go/container/apiv1"
	"cloud.google.com/go/container/apiv1/containerpb"
	"google.golang.org/api/option"
	sqladmin "google.golang.org/api/sqladmin/v1beta4"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/linuxuser586/gcpemu/emutest"
	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/runtime"
	"github.com/linuxuser586/gcpemu/services/sql"
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

// TestRecoveryWithGKE covers #113: with a GKE cluster in the same
// instance, whose control plane and node take any free external address,
// a public-IP instance comes back RUNNABLE with its address and data after
// each of several restarts. A FAILED instance is tried again on the next
// start, and instances.restart brings one back from its data volume.
func TestRecoveryWithGKE(t *testing.T) {
	emutest.RequireRuntime(t)
	t.Parallel()
	dir := t.TempDir()
	persistent := func(c *config.Config) { c.Ephemeral = false; c.DataDir = dir }
	ctx := context.Background()

	inst := emutest.Start(t, []string{"sql", "gke"}, persistent)
	rt, err := inst.Env.Containers.Runtime(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_ = rt.Cleanup(ctx, true)
	})
	svc := adminClient(t, inst)
	in := createInstance(t, svc, &sqladmin.DatabaseInstance{
		Name:         "db",
		RootPassword: "rootpw",
		Settings: &sqladmin.Settings{IpConfiguration: &sqladmin.IpConfiguration{
			Ipv4Enabled: true, AuthorizedNetworks: []*sqladmin.AclEntry{{Value: "0.0.0.0/0"}},
		}},
	})
	ip := publicIP(in)
	conn, err := connect(ctx, ip, 5432, "postgres", "rootpw", "postgres")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, "CREATE TABLE kept (v text); INSERT INTO kept VALUES ('survives')"); err != nil {
		t.Fatal(err)
	}
	conn.Close(ctx)

	const gkeParent = "projects/" + testProject + "/locations/us-central1-a"
	cm := gkeClient(t, inst)
	op, err := cm.CreateCluster(ctx, &containerpb.CreateClusterRequest{Parent: gkeParent, Cluster: &containerpb.Cluster{
		Name: "c", NodePools: []*containerpb.NodePool{{Name: "np", InitialNodeCount: 1}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	waitClusterOp(t, cm, gkeParent+"/operations/"+op.Name)

	check := func(svc *sqladmin.Service, when string) {
		t.Helper()
		in, err := svc.Instances.Get(testProject, "db").Do()
		if err != nil {
			t.Fatal(err)
		}
		if in.State != "RUNNABLE" || publicIP(in) != ip {
			t.Fatalf("%s: state %s, public IP %s; want RUNNABLE, %s", when, in.State, publicIP(in), ip)
		}
		conn, err := connect(ctx, ip, 5432, "postgres", "rootpw", "postgres")
		if err != nil {
			t.Fatalf("%s: %v", when, err)
		}
		defer conn.Close(ctx)
		var v string
		if err := conn.QueryRow(ctx, "SELECT v FROM kept").Scan(&v); err != nil || v != "survives" {
			t.Fatalf("%s: row %q %v", when, v, err)
		}
	}
	sqlService := func(inst *emutest.Instance) emu.Service {
		for _, s := range inst.Services() {
			if s.Name() == "sql" {
				return s
			}
		}
		t.Fatal("no sql service")
		return nil
	}

	for i := range 3 {
		if i == 1 {
			// A recovery that failed is tried again on the next start.
			if err := sql.MarkFailed(ctx, sqlService(inst), testProject, "db"); err != nil {
				t.Fatal(err)
			}
		}
		sctx, cancel := context.WithTimeout(ctx, time.Minute)
		err := inst.Shutdown(sctx)
		cancel()
		if err != nil {
			t.Fatalf("shutdown: %v", err)
		}
		inst = emutest.Start(t, []string{"sql", "gke"}, persistent) // waits for Ready (instances recovered)
		check(adminClient(t, inst), fmt.Sprintf("restart %d", i+1))
	}

	if err := sql.MarkFailed(ctx, sqlService(inst), testProject, "db"); err != nil {
		t.Fatal(err)
	}
	svc = adminClient(t, inst)
	waitOp(t, svc, must(svc.Instances.Restart(testProject, "db").Do()))
	check(svc, "instances.restart of a FAILED instance")
}

func gkeClient(t *testing.T, inst *emutest.Instance) *container.ClusterManagerClient {
	t.Helper()
	cm, err := container.NewClusterManagerClient(context.Background(),
		option.WithEndpoint(inst.Endpoint("gateway")), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cm.Close() })
	return cm
}

func waitClusterOp(t *testing.T, cm *container.ClusterManagerClient, name string) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Minute); ; time.Sleep(500 * time.Millisecond) {
		op, err := cm.GetOperation(context.Background(), &containerpb.GetOperationRequest{Name: name})
		if err != nil {
			t.Fatal(err)
		}
		if op.Status == containerpb.Operation_DONE {
			if op.Error != nil {
				t.Fatalf("%s failed: %s", op.OperationType, op.Error.Message)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s not done after 5m", op.OperationType)
		}
	}
}
