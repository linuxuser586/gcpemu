package sql_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"google.golang.org/api/option"
	sqladmin "google.golang.org/api/sqladmin/v1beta4"
	storage "google.golang.org/api/storage/v1"

	"github.com/linuxuser586/gcpemu/emutest"
)

func count(t testing.TB, c *pgx.Conn, q string) int {
	t.Helper()
	var n int
	if err := c.QueryRow(context.Background(), q).Scan(&n); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}

// TestDataFeatures covers FR-SQL-007 (SSL modes, sslCerts client
// certificates), FR-SQL-008 (backup run, restore, clone) and FR-SQL-009 /
// FR-INT-013 (SQL and CSV import/export through emulated Cloud Storage).
func TestDataFeatures(t *testing.T) {
	emutest.RequireRuntime(t)
	t.Parallel()
	inst := emutest.Start(t, []string{"sql", "gcs"})
	svc := adminClient(t, inst)
	ctx := context.Background()
	open := []*sqladmin.AclEntry{{Value: "0.0.0.0/0"}}
	in := createInstance(t, svc, &sqladmin.DatabaseInstance{
		Name: "data", DatabaseVersion: "POSTGRES_17", RootPassword: "rootpw",
		Settings: &sqladmin.Settings{IpConfiguration: &sqladmin.IpConfiguration{Ipv4Enabled: true, AuthorizedNetworks: open}},
	})
	ip := publicIP(in)
	waitOp(t, svc, must(svc.Databases.Insert(testProject, "data", &sqladmin.Database{Name: "app"}).Do()))
	conn, err := connect(ctx, ip, 5432, "postgres", "rootpw", "app")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, "CREATE TABLE items (id int PRIMARY KEY, name text); INSERT INTO items SELECT g, 'item' || g FROM generate_series(1, 5) g"); err != nil {
		t.Fatal(err)
	}
	conn.Close(ctx)

	// Export / import through GCS.
	st, err := storage.NewService(ctx, option.WithEndpoint(inst.GatewayURL()+"/storage/v1/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Buckets.Insert(testProject, &storage.Bucket{Name: "sql-dumps"}).Do(); err != nil {
		t.Fatal(err)
	}
	waitOp(t, svc, must(svc.Instances.Export(testProject, "data", &sqladmin.InstancesExportRequest{ExportContext: &sqladmin.ExportContext{
		Uri: "gs://sql-dumps/app.sql.gz", Databases: []string{"app"}, FileType: "SQL",
	}}).Do()))
	waitOp(t, svc, must(svc.Instances.Export(testProject, "data", &sqladmin.InstancesExportRequest{ExportContext: &sqladmin.ExportContext{
		Uri: "gs://sql-dumps/items.csv", Databases: []string{"app"}, FileType: "CSV",
		CsvExportOptions: &sqladmin.ExportContextCsvExportOptions{SelectQuery: "SELECT id, name FROM items WHERE id <= 3"},
	}}).Do()))
	obj, err := st.Objects.Get("sql-dumps", "items.csv").Download()
	if err != nil {
		t.Fatal(err)
	}
	obj.Body.Close()
	waitOp(t, svc, must(svc.Databases.Insert(testProject, "data", &sqladmin.Database{Name: "restored"}).Do()))
	waitOp(t, svc, must(svc.Instances.Import(testProject, "data", &sqladmin.InstancesImportRequest{ImportContext: &sqladmin.ImportContext{
		Uri: "gs://sql-dumps/app.sql.gz", Database: "restored", FileType: "SQL",
	}}).Do()))
	conn, err = connect(ctx, ip, 5432, "postgres", "rootpw", "restored")
	if err != nil {
		t.Fatal(err)
	}
	if n := count(t, conn, "SELECT count(*) FROM items"); n != 5 {
		t.Fatalf("imported rows = %d", n)
	}
	if _, err := conn.Exec(ctx, "CREATE TABLE csv_items (id int, name text)"); err != nil {
		t.Fatal(err)
	}
	conn.Close(ctx)
	waitOp(t, svc, must(svc.Instances.Import(testProject, "data", &sqladmin.InstancesImportRequest{ImportContext: &sqladmin.ImportContext{
		Uri: "gs://sql-dumps/items.csv", Database: "restored", FileType: "CSV",
		CsvImportOptions: &sqladmin.ImportContextCsvImportOptions{Table: "csv_items", Columns: []string{"id", "name"}},
	}}).Do()))
	conn, _ = connect(ctx, ip, 5432, "postgres", "rootpw", "restored")
	if n := count(t, conn, "SELECT count(*) FROM csv_items"); n != 3 {
		t.Fatalf("CSV rows = %d", n)
	}
	conn.Close(ctx)

	// Backup, damage, restore.
	waitOp(t, svc, must(svc.BackupRuns.Insert(testProject, "data", &sqladmin.BackupRun{Description: "before"}).Do()))
	runs := must(svc.BackupRuns.List(testProject, "data").Do())
	if len(runs.Items) != 1 || runs.Items[0].Status != "SUCCESSFUL" {
		t.Fatalf("backup runs = %+v", runs.Items)
	}
	conn, _ = connect(ctx, ip, 5432, "postgres", "rootpw", "app")
	_, _ = conn.Exec(ctx, "DELETE FROM items")
	conn.Close(ctx)
	waitOp(t, svc, must(svc.Instances.RestoreBackup(testProject, "data", &sqladmin.InstancesRestoreBackupRequest{
		RestoreBackupContext: &sqladmin.RestoreBackupContext{BackupRunId: runs.Items[0].Id},
	}).Do()))
	in = must(svc.Instances.Get(testProject, "data").Do())
	conn, err = connect(ctx, publicIP(in), 5432, "postgres", "rootpw", "app")
	if err != nil {
		t.Fatal(err)
	}
	if n := count(t, conn, "SELECT count(*) FROM items"); n != 5 {
		t.Fatalf("rows after restore = %d", n)
	}
	conn.Close(ctx)

	// Clone.
	waitOp(t, svc, must(svc.Instances.Clone(testProject, "data", &sqladmin.InstancesCloneRequest{CloneContext: &sqladmin.CloneContext{
		DestinationInstanceName: "data-clone",
	}}).Do()))
	cl := must(svc.Instances.Get(testProject, "data-clone").Do())
	conn, err = connect(ctx, publicIP(cl), 5432, "postgres", "rootpw", "app")
	if err != nil {
		t.Fatal(err)
	}
	if n := count(t, conn, "SELECT count(*) FROM items"); n != 5 {
		t.Fatalf("rows in clone = %d", n)
	}
	conn.Close(ctx)
	if dbs := must(svc.Databases.List(testProject, "data-clone").Do()); len(dbs.Items) != 3 {
		t.Fatalf("clone databases = %d", len(dbs.Items))
	}

	// SSL modes and client certificates.
	waitOp(t, svc, must(svc.Instances.Patch(testProject, "data", &sqladmin.DatabaseInstance{Settings: &sqladmin.Settings{
		IpConfiguration: &sqladmin.IpConfiguration{Ipv4Enabled: true, AuthorizedNetworks: open, SslMode: "ENCRYPTED_ONLY"},
	}}).Do()))
	if _, err := connectSSL(ctx, ip, "disable", nil); err == nil {
		t.Fatal("plaintext connection accepted with ENCRYPTED_ONLY")
	}
	c, err := connectSSL(ctx, ip, "require", nil)
	if err != nil {
		t.Fatalf("TLS connection with ENCRYPTED_ONLY: %v", err)
	}
	c.Close(ctx)

	waitOp(t, svc, must(svc.Instances.Patch(testProject, "data", &sqladmin.DatabaseInstance{Settings: &sqladmin.Settings{
		IpConfiguration: &sqladmin.IpConfiguration{Ipv4Enabled: true, AuthorizedNetworks: open, SslMode: "TRUSTED_CLIENT_CERTIFICATE_REQUIRED"},
	}}).Do()))
	if got := must(svc.Instances.Get(testProject, "data").Do()); !got.Settings.IpConfiguration.RequireSsl {
		t.Error("requireSsl not reported for TRUSTED_CLIENT_CERTIFICATE_REQUIRED")
	}
	if _, err := connectSSL(ctx, ip, "require", nil); err == nil {
		t.Fatal("connection without a client certificate accepted")
	}
	ins := must(svc.SslCerts.Insert(testProject, "data", &sqladmin.SslCertsInsertRequest{CommonName: "client1"}).Do())
	pair, err := tls.X509KeyPair([]byte(ins.ClientCert.CertInfo.Cert), []byte(ins.ClientCert.CertPrivateKey))
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM([]byte(ins.ServerCaCert.Cert))
	tlsCfg := &tls.Config{Certificates: []tls.Certificate{pair}, RootCAs: roots, ServerName: testProject + ":data", InsecureSkipVerify: true}
	c, err = connectSSL(ctx, ip, "require", tlsCfg)
	if err != nil {
		t.Fatalf("client certificate: %v", err)
	}
	c.Close(ctx)
	waitOp(t, svc, must(svc.SslCerts.Delete(testProject, "data", ins.ClientCert.CertInfo.Sha1Fingerprint).Do()))
	if _, err := connectSSL(ctx, ip, "require", tlsCfg); err == nil {
		t.Fatal("deleted client certificate accepted")
	}
}

func connectSSL(ctx context.Context, ip, mode string, tlsCfg *tls.Config) (*pgx.Conn, error) {
	cfg, err := pgx.ParseConfig(fmt.Sprintf("host=%s port=5432 user=postgres dbname=postgres sslmode=%s connect_timeout=5", ip, mode))
	if err != nil {
		return nil, err
	}
	cfg.Password = "rootpw"
	if tlsCfg != nil {
		cfg.TLSConfig = tlsCfg
		cfg.Fallbacks = nil
	}
	return pgx.ConnectConfig(ctx, cfg)
}

// TestSeed covers FR-CORE-011: a seed declares an instance with
// databases, users and init SQL; applying it twice is idempotent.
func TestSeed(t *testing.T) {
	emutest.RequireRuntime(t)
	t.Parallel()
	inst := emutest.Start(t, []string{"sql"})
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "schema.sql"), []byte("CREATE TABLE IF NOT EXISTS notes (body text);"), 0o600); err != nil {
		t.Fatal(err)
	}
	seed := `
sql:
  instances:
    - name: seeded
      project: ` + testProject + `
      databaseVersion: POSTGRES_17
      rootPassword: rootpw
      authorizedNetworks: [0.0.0.0/0]
      flags: {cloudsql.iam_authentication: "on"}
      databases: [notes]
      users:
        - {name: writer, password: pw}
      initSQL:
        - {database: notes, file: ./schema.sql}
        - {database: notes, sql: "GRANT ALL ON notes TO writer; INSERT INTO notes VALUES ('hello');"}
`
	path := filepath.Join(dir, "seed.yaml")
	if err := os.WriteFile(path, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if err := inst.ApplySeed(ctx, path); err != nil {
			t.Fatalf("apply %d: %v", i, err)
		}
	}
	svc := adminClient(t, inst)
	in := must(svc.Instances.Get(testProject, "seeded").Do())
	if in.State != "RUNNABLE" || len(in.Settings.DatabaseFlags) != 1 {
		t.Fatalf("seeded instance = %s %+v", in.State, in.Settings.DatabaseFlags)
	}
	conn, err := connect(ctx, publicIP(in), 5432, "writer", "pw", "notes")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if n := count(t, conn, "SELECT count(*) FROM notes"); n != 1 {
		t.Fatalf("notes rows = %d, want 1 (init SQL applied once)", n)
	}
	if !strings.Contains(inst.EnvVars()["GCPEMU_SQL_SEEDED"], ":") {
		t.Errorf("GCPEMU_SQL_SEEDED = %q", inst.EnvVars()["GCPEMU_SQL_SEEDED"])
	}
}
