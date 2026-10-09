package sql_test

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"google.golang.org/api/googleapi"
	sqladmin "google.golang.org/api/sqladmin/v1beta4"

	"github.com/linuxuser586/gcpemu/emutest"
)

// TestExecuteSql covers instances.executeSql, the Cloud SQL Data API that
// the console's query runner uses: it needs dataApiAccess, reports each
// statement's columns, types and NULLs, rolls back on an error, honours
// rowLimit, and runs as the named user.
func TestExecuteSql(t *testing.T) {
	emutest.RequireRuntime(t)
	t.Parallel()
	inst := emutest.Start(t, []string{"sql"})
	svc := adminClient(t, inst)
	createInstance(t, svc, &sqladmin.DatabaseInstance{
		Name: "data-api", DatabaseVersion: "POSTGRES_17", RootPassword: "rootpw",
		Settings: &sqladmin.Settings{Tier: "db-custom-1-3840", Edition: "ENTERPRISE"},
	})
	waitOp(t, svc, must(svc.Databases.Insert(testProject, "data-api", &sqladmin.Database{Name: "app"}).Do()))
	run := func(p *sqladmin.ExecuteSqlPayload) (*sqladmin.SqlInstancesExecuteSqlResponse, error) {
		return svc.Instances.ExecuteSql(testProject, "data-api", p).Do()
	}

	// Off until settings.dataApiAccess allows it.
	_, err := run(&sqladmin.ExecuteSqlPayload{User: "postgres", SqlStatement: "SELECT 1"})
	var ge *googleapi.Error
	if !errors.As(err, &ge) || ge.Code != http.StatusBadRequest || !strings.Contains(ge.Message, "ALLOW_DATA_API") {
		t.Fatalf("executeSql without data API access: %v", err)
	}
	waitOp(t, svc, must(svc.Instances.Patch(testProject, "data-api", &sqladmin.DatabaseInstance{
		Settings: &sqladmin.Settings{DataApiAccess: "ALLOW_DATA_API"},
	}).Do()))

	res, err := run(&sqladmin.ExecuteSqlPayload{User: "postgres", Database: "app", SqlStatement: `
		CREATE TABLE items (id int PRIMARY KEY, name text, price numeric);
		INSERT INTO items VALUES (1, 'one', 1.5), (2, NULL, 2), (3, 'three', NULL);
		SELECT id, name, price, current_user AS who FROM items ORDER BY id;`})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != nil || len(res.Results) != 3 {
		t.Fatalf("results = %+v, status = %+v", res.Results, res.Status)
	}
	if res.Results[0].Message != "CREATE TABLE" || res.Results[1].Message != "INSERT 0 3" || res.Results[2].Message != "SELECT 3" {
		t.Errorf("messages = %q, %q, %q", res.Results[0].Message, res.Results[1].Message, res.Results[2].Message)
	}
	sel := res.Results[2]
	var cols []string
	for _, c := range sel.Columns {
		cols = append(cols, c.Name+":"+c.Type)
	}
	if got := strings.Join(cols, ","); got != "id:INT4,name:TEXT,price:NUMERIC,who:NAME" {
		t.Errorf("columns = %s", got)
	}
	if v := sel.Rows[1].Values[1]; !v.NullValue {
		t.Errorf("row 2 name = %+v, want NULL", v)
	}
	if v := sel.Rows[0].Values; v[1].Value != "one" || v[2].Value != "1.5" || v[3].Value != "postgres" {
		t.Errorf("row 1 = %+v", v)
	}
	if res.Metadata == nil || !strings.HasSuffix(res.Metadata.SqlStatementExecutionTime, "s") {
		t.Errorf("metadata = %+v", res.Metadata)
	}

	// An error returns no results and rolls the whole request back.
	res, err = run(&sqladmin.ExecuteSqlPayload{User: "postgres", Database: "app",
		SqlStatement: "INSERT INTO items VALUES (4, 'four', 4); SELECT * FROM nowhere;"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status == nil || res.Status.Code != 3 || !strings.Contains(res.Status.Message, `relation "nowhere" does not exist`) || len(res.Results) != 0 {
		t.Fatalf("failed request: results = %+v, status = %+v", res.Results, res.Status)
	}

	// A read-only transaction refuses writes, as the console's default.
	res, err = run(&sqladmin.ExecuteSqlPayload{User: "postgres", Database: "app",
		SqlStatement: "BEGIN READ ONLY;\nDELETE FROM items\n;\nCOMMIT;"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status == nil || !strings.Contains(res.Status.Message, "read-only transaction") {
		t.Fatalf("read-only request: status = %+v", res.Status)
	}

	// rowLimit truncates each statement's rows; notices are messages.
	res, err = run(&sqladmin.ExecuteSqlPayload{User: "postgres", Database: "app", RowLimit: 2,
		SqlStatement: "DO $$BEGIN RAISE NOTICE 'hello'; END$$; SELECT count(*) FROM items; SELECT * FROM items ORDER BY id"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Results) != 3 || res.Results[1].Rows[0].Values[0].Value != "3" {
		t.Fatalf("results = %+v", res.Results)
	}
	if r := res.Results[2]; len(r.Rows) != 2 || !r.PartialResult {
		t.Errorf("rowLimit: %d rows, partial %v", len(r.Rows), r.PartialResult)
	}
	if len(res.Messages) != 1 || res.Messages[0].Message != "hello" || res.Messages[0].Severity != "NOTICE" {
		t.Errorf("messages = %+v", res.Messages)
	}

	// Runs as the named user, with its privileges.
	waitOp(t, svc, must(svc.Users.Insert(testProject, "data-api", &sqladmin.User{Name: "reader", Password: "pw"}).Do()))
	res, err = run(&sqladmin.ExecuteSqlPayload{User: "reader", Database: "app", SqlStatement: "SELECT * FROM items"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status == nil || res.Status.Code != 7 || !strings.Contains(res.Status.Message, "permission denied") {
		t.Errorf("as reader: status = %+v", res.Status)
	}
	if _, err := run(&sqladmin.ExecuteSqlPayload{User: "nobody", SqlStatement: "SELECT 1"}); err == nil {
		t.Error("an unknown user was accepted")
	}
	if _, err := run(&sqladmin.ExecuteSqlPayload{User: "postgres"}); err == nil {
		t.Error("an empty sqlStatement was accepted")
	}
}
