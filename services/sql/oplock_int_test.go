package sql_test

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/api/googleapi"
	sqladmin "google.golang.org/api/sqladmin/v1beta4"

	"github.com/linuxuser586/gcpemu/emutest"
)

// Seed's work on a Cloud SQL instance never fails on the Operation lock,
// and calls that aren't Operations carry on while one runs: a running
// Operation only refuses another Operation.
func TestOperationLockOnRealInstance(t *testing.T) {
	emutest.RequireRuntime(t)
	t.Parallel()
	seed := filepath.Join(t.TempDir(), "seed.yaml")
	if err := os.WriteFile(seed, []byte(`sql:
  instances:
    - name: seeded
      project: `+testProject+`
      databaseVersion: POSTGRES_17
      rootPassword: rootpw
      settings: {dataApiAccess: ALLOW_DATA_API}
      databases: [notes]
      users:
        - {name: writer, password: pw}
`), 0o600); err != nil {
		t.Fatal(err)
	}
	inst := emutest.Start(t, []string{"sql"}, emutest.WithSeed(seed), emutest.WithOperationLatency("sql", 4*time.Second))
	svc := adminClient(t, inst)

	if _, err := svc.Databases.Get(testProject, "seeded", "notes").Do(); err != nil {
		t.Fatalf("seeded database: %v", err)
	}
	if _, err := svc.Users.Get(testProject, "seeded", "writer").Do(); err != nil {
		t.Fatalf("seeded user: %v", err)
	}

	cert := &sqladmin.SslCertsInsertRequest{CommonName: "first"}
	first, err := svc.SslCerts.Insert(testProject, "seeded", cert).Do()
	if err != nil {
		t.Fatalf("first Operation: %v", err)
	}
	res, err := svc.Instances.ExecuteSql(testProject, "seeded", &sqladmin.ExecuteSqlPayload{
		User: "postgres", Database: "notes", SqlStatement: "SELECT 1",
	}).Do()
	if err != nil {
		t.Fatalf("executeSql during an Operation: %v", err)
	}
	if len(res.Results) != 1 || len(res.Results[0].Rows) != 1 {
		t.Errorf("executeSql: %+v", res)
	}
	if _, err := svc.Users.List(testProject, "seeded").Do(); err != nil {
		t.Errorf("list users during an Operation: %v", err)
	}

	// The first Operation was still running for all of that.
	_, err = svc.Databases.Insert(testProject, "seeded", &sqladmin.Database{Name: "second"}).Do()
	var ge *googleapi.Error
	if !errors.As(err, &ge) || ge.Code != http.StatusConflict || !strings.Contains(err.Error(), "operationInProgress") {
		t.Fatalf("second Operation: %v, want 409 operationInProgress", err)
	}
	waitOp(t, svc, first.Operation)
	waitOp(t, svc, must(svc.Databases.Insert(testProject, "seeded", &sqladmin.Database{Name: "second"}).Do()))
}
