package sql_test

import (
	"context"
	"strings"
	"testing"

	"github.com/linuxuser586/gcpemu/emutest"
)

// TestExportSeedRoundTrip covers FR-CORE-013 for Cloud SQL: a stopped
// instance seeded with a database and users (created while its container
// runs once) exports them, and the export recreates it after a Reset.
func TestExportSeedRoundTrip(t *testing.T) {
	emutest.RequireRuntime(t)
	t.Parallel()
	ctx := context.Background()
	inst := emutest.Start(t, []string{"sql"})
	seed := `sql:
  instances:
    - name: seeded
      project: ` + testProject + `
      databaseVersion: POSTGRES_17
      rootPassword: rootpw
      flags: {max_connections: "200"}
      settings: {activationPolicy: NEVER}
      databases: [app]
      users:
        - {name: app, password: apppw}
        - {name: sa@` + testProject + `.iam, type: CLOUD_IAM_SERVICE_ACCOUNT}
`
	if err := inst.Internal().ApplySeedData(ctx, []byte(seed), t.TempDir()); err != nil {
		t.Fatal(err)
	}
	got, err := inst.Internal().ExportSeed(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := `sql:
  instances:
    - name: seeded
      project: ` + testProject + `
      databaseVersion: POSTGRES_17
      rootPassword: rootpw
      flags:
        max_connections: "200"
      settings:
        activationPolicy: NEVER
      databases:
        - app
      users:
        - name: app
          password: apppw
        - name: sa@` + testProject + `.iam
          type: CLOUD_IAM_SERVICE_ACCOUNT
`
	if !strings.Contains(string(got), want) {
		t.Fatalf("export =\n%s\nwant section\n%s", got, want)
	}
	if err := inst.Internal().Reset(ctx); err != nil {
		t.Fatal(err)
	}
	if err := inst.Internal().ApplySeedData(ctx, got, t.TempDir()); err != nil {
		t.Fatalf("apply export: %v", err)
	}
	again, err := inst.Internal().ExportSeed(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if string(again) != string(got) {
		t.Fatalf("second export =\n%s\nfirst =\n%s", again, got)
	}
}
