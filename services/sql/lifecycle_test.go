package sql_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	sqladmin "google.golang.org/api/sqladmin/v1beta4"

	"github.com/linuxuser586/gcpemu/emutest"
)

// publicIP returns the instance's PRIMARY address.
func publicIP(in *sqladmin.DatabaseInstance) string {
	for _, ip := range in.IpAddresses {
		if ip.Type == "PRIMARY" {
			return ip.IpAddress
		}
	}
	return ""
}

func connect(ctx context.Context, host string, port int, user, password, db string) (*pgx.Conn, error) {
	cfg, err := pgx.ParseConfig(fmt.Sprintf("host=%s port=%d user=%s dbname=%s sslmode=prefer connect_timeout=5", host, port, user, db))
	if err != nil {
		return nil, err
	}
	cfg.Password = password
	return pgx.ConnectConfig(ctx, cfg)
}

// TestInstanceLifecycle covers FR-SQL-001/002/003 and NFR-PERF-002 with the
// sqladmin v1beta4 client: insert → RUNNABLE within 10 s, databases and
// users, flags with restart, stop/start, delete removing container and volume.
func TestInstanceLifecycle(t *testing.T) {
	emutest.RequireRuntime(t)
	// Not parallel: the NFR-PERF-002 timing is measured on an idle host.
	inst := emutest.Start(t, []string{"sql"})
	svc := adminClient(t, inst)
	ctx := context.Background()

	start := time.Now()
	in := createInstance(t, svc, &sqladmin.DatabaseInstance{
		Name:            "life",
		DatabaseVersion: "POSTGRES_17",
		Region:          "us-central1",
		RootPassword:    "rootpw",
		Settings: &sqladmin.Settings{
			Tier: "db-custom-1-3840",
			IpConfiguration: &sqladmin.IpConfiguration{
				Ipv4Enabled:        true,
				AuthorizedNetworks: []*sqladmin.AclEntry{{Name: "all", Value: "0.0.0.0/0"}},
			},
			DatabaseFlags: []*sqladmin.DatabaseFlags{{Name: "work_mem", Value: "8192"}},
		},
	})
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("insert → RUNNABLE took %v, want ≤ 10s (NFR-PERF-002)", d)
	}
	t.Logf("insert → RUNNABLE in %v", time.Since(start))
	ip := publicIP(in)
	if ip == "" || in.ConnectionName != testProject+":us-central1:life" || in.ServerCaCert == nil {
		t.Fatalf("instance = %+v", in)
	}

	conn, err := connect(ctx, ip, 5432, "postgres", "rootpw", "postgres")
	if err != nil {
		t.Fatal(err)
	}
	var super bool
	var workMem string
	if err := conn.QueryRow(ctx, "SELECT rolsuper FROM pg_roles WHERE rolname = current_user").Scan(&super); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRow(ctx, "SHOW work_mem").Scan(&workMem); err != nil {
		t.Fatal(err)
	}
	conn.Close(ctx)
	if super || workMem != "8MB" {
		t.Errorf("postgres superuser=%v work_mem=%s, want false/8MB", super, workMem)
	}

	// Databases and users.
	waitOp(t, svc, must(svc.Databases.Insert(testProject, "life", &sqladmin.Database{Name: "app"}).Do()))
	waitOp(t, svc, must(svc.Users.Insert(testProject, "life", &sqladmin.User{Name: "appuser", Password: "s3cret"}).Do()))
	conn, err = connect(ctx, ip, 5432, "appuser", "s3cret", "app")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, "CREATE TABLE t (id int); INSERT INTO t VALUES (1)"); err != nil {
		t.Fatalf("appuser cannot create a table in app: %v", err)
	}
	conn.Close(ctx)
	if _, err := connect(ctx, ip, 5432, "appuser", "wrong", "app"); err == nil {
		t.Fatal("wrong password accepted")
	}
	dbs := must(svc.Databases.List(testProject, "life").Do())
	users := must(svc.Users.List(testProject, "life").Do())
	if len(dbs.Items) != 2 || len(users.Items) != 2 {
		t.Fatalf("databases=%d users=%d, want 2/2", len(dbs.Items), len(users.Items))
	}
	waitOp(t, svc, must(svc.Users.Update(testProject, "life", &sqladmin.User{Password: "n3w"}).Name("appuser").Do()))
	if c, err := connect(ctx, ip, 5432, "appuser", "n3w", "app"); err != nil {
		t.Fatalf("new password: %v", err)
	} else {
		c.Close(ctx)
	}

	// Invalid flags are rejected synchronously.
	_, err = svc.Instances.Patch(testProject, "life", &sqladmin.DatabaseInstance{Settings: &sqladmin.Settings{
		DatabaseFlags: []*sqladmin.DatabaseFlags{{Name: "no_such_flag", Value: "1"}},
	}}).Do()
	if err == nil || !strings.Contains(err.Error(), "no_such_flag") {
		t.Fatalf("invalid flag: %v", err)
	}
	// A restart-required flag restarts PostgreSQL within the operation.
	waitOp(t, svc, must(svc.Instances.Patch(testProject, "life", &sqladmin.DatabaseInstance{Settings: &sqladmin.Settings{
		DatabaseFlags: []*sqladmin.DatabaseFlags{{Name: "max_connections", Value: "77"}},
	}}).Do()))
	conn, err = connect(ctx, ip, 5432, "postgres", "rootpw", "postgres")
	if err != nil {
		t.Fatal(err)
	}
	var maxConn, workMem2 string
	_ = conn.QueryRow(ctx, "SHOW max_connections").Scan(&maxConn)
	_ = conn.QueryRow(ctx, "SHOW work_mem").Scan(&workMem2)
	conn.Close(ctx)
	if maxConn != "77" || workMem2 != "4MB" {
		t.Errorf("after patch max_connections=%s work_mem=%s, want 77/4MB", maxConn, workMem2)
	}

	// Stop and start.
	waitOp(t, svc, must(svc.Instances.Patch(testProject, "life", &sqladmin.DatabaseInstance{Settings: &sqladmin.Settings{ActivationPolicy: "NEVER"}}).Do()))
	if c, err := connect(ctx, ip, 5432, "postgres", "rootpw", "postgres"); err == nil {
		c.Close(ctx)
		t.Fatal("connected to a stopped instance")
	}
	waitOp(t, svc, must(svc.Instances.Patch(testProject, "life", &sqladmin.DatabaseInstance{Settings: &sqladmin.Settings{ActivationPolicy: "ALWAYS"}}).Do()))
	in = must(svc.Instances.Get(testProject, "life").Do())
	conn, err = connect(ctx, publicIP(in), 5432, "appuser", "n3w", "app")
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := conn.QueryRow(ctx, "SELECT count(*) FROM t").Scan(&n); err != nil || n != 1 {
		t.Fatalf("after restart: n=%d err=%v", n, err)
	}
	conn.Close(ctx)

	// Restart.
	waitOp(t, svc, must(svc.Instances.Restart(testProject, "life").Do()))

	// Delete removes the container and the volume.
	waitOp(t, svc, must(svc.Instances.Delete(testProject, "life").Do()))
	if _, err := svc.Instances.Get(testProject, "life").Do(); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("get after delete: %v", err)
	}
	assertNoRuntimeObjects(t, inst, "projects/"+testProject+"/instances/life")
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}
