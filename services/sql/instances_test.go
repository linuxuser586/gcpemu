package sql

import (
	"encoding/json"
	"net/url"
	"slices"
	"strings"
	"testing"

	sqladmin "google.golang.org/api/sqladmin/v1beta4"

	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// storeInstance stores an instance without a container.
func storeInstance(t *testing.T, s *Service, project string, in *sqladmin.DatabaseInstance) {
	t.Helper()
	if err := s.env.Store.Update(func(tx store.Tx) error {
		return store.PutJSON(tx, nsInstances, instKey(project, in.Name), &instanceRecord{Instance: in})
	}); err != nil {
		t.Fatal(err)
	}
}

// listInstanceNames lists the project's instances with filter, following
// pages, and returns their names.
func listInstanceNames(t *testing.T, s *Service, project, filter, maxResults string) []string {
	t.Helper()
	var names []string
	token := ""
	for {
		q := url.Values{"filter": {filter}, "maxResults": {maxResults}, "pageToken": {token}}
		code, body := call(s, "GET", "/projects/"+project+"/instances?"+q.Encode(), "")
		if code != 200 {
			t.Fatalf("list %q: %d %s", filter, code, body)
		}
		var resp sqladmin.InstancesListResponse
		if err := json.Unmarshal([]byte(body), &resp); err != nil {
			t.Fatal(err)
		}
		for _, in := range resp.Items {
			names = append(names, in.Name)
		}
		if token = resp.NextPageToken; token == "" {
			return names
		}
	}
}

func TestListInstancesFilter(t *testing.T) {
	s := testService(t, config.IAMOff)
	for _, in := range []*sqladmin.DatabaseInstance{
		{Name: "a", State: "RUNNABLE", Region: "us-central1", GceZone: "us-central1-a", DatabaseVersion: "POSTGRES_17",
			InstanceType: "CLOUD_SQL_INSTANCE", Settings: &sqladmin.Settings{Tier: "db-custom-1-3840", UserLabels: map[string]string{"env": "dev"}}},
		{Name: "b", State: "RUNNABLE", Region: "us-central1", GceZone: "us-central1-b", DatabaseVersion: "POSTGRES_17",
			InstanceType: "CLOUD_SQL_INSTANCE", Settings: &sqladmin.Settings{Tier: "db-f1-micro", UserLabels: map[string]string{"env": "prod"}}},
		{Name: "c", State: "STOPPED", Region: "us-east1", GceZone: "us-east1-b", DatabaseVersion: "POSTGRES_1",
			InstanceType: "READ_REPLICA_INSTANCE", Settings: &sqladmin.Settings{Tier: "db-custom-1-3840"}},
		{Name: "d", State: "RUNNABLE", Region: "us-east1", DatabaseVersion: "POSTGRES_15"},
	} {
		storeInstance(t, s, "proj-one", in)
	}
	for _, c := range []struct {
		filter string
		want   []string
	}{
		{``, []string{"a", "b", "c", "d"}},
		{`name=a`, []string{"a"}},
		{`name!=a`, []string{"b", "c", "d"}},
		{`name:a`, []string{"a"}},
		{`state:RUNNABLE`, []string{"a", "b", "d"}},
		{`state!=RUNNABLE`, []string{"c"}},
		{`region="us-east1"`, []string{"c", "d"}},
		// : is exact on every field.
		{`databaseVersion:POSTGRES_1`, []string{"c"}},
		{`databaseVersion=POSTGRES_17`, []string{"a", "b"}},
		{`databaseVersion!=POSTGRES_17`, []string{"c", "d"}},
		{`gceZone:us-central1`, nil},
		{`gceZone:us-central1-a`, []string{"a"}},
		{`gceZone!=us-central1-a`, []string{"b", "c", "d"}},
		{`instanceType=READ_REPLICA_INSTANCE`, []string{"c"}},
		{`instanceType:CLOUD_SQL_INSTANCE`, []string{"a", "b"}},
		{`instanceType!=CLOUD_SQL_INSTANCE`, []string{"c", "d"}},
		{`settings.tier:db-custom-1-3840`, []string{"a", "c"}},
		{`settings.tier=db-f1-micro`, []string{"b"}},
		{`settings.tier!=db-f1-micro`, []string{"a", "c", "d"}},
		// Terraform's google_sql_database_instances shape.
		{`databaseVersion:POSTGRES_17 AND region:us-central1 AND gceZone:us-central1-a AND settings.tier:db-custom-1-3840 AND state:RUNNABLE`, []string{"a"}},
		// Labels: a missing label, or missing settings, reads as empty.
		{`settings.userLabels.env:dev`, []string{"a"}},
		{`settings.userLabels.env=dev`, []string{"a"}},
		{`settings.userLabels.env:de`, nil},
		{`settings.userLabels.env!=dev`, []string{"b", "c", "d"}},
		{`NOT settings.userLabels.env=dev`, []string{"b", "c", "d"}},
		{`settings.userLabels.nokey=dev`, nil},
		// Boolean operators.
		{`name=a OR name=b`, []string{"a", "b"}},
		{`(name=a OR name=c) AND NOT state=STOPPED`, []string{"a"}},
		{`state=RUNNABLE region=us-east1`, []string{"d"}},
		// Single quotes are part of the value.
		{`name='a'`, nil},
	} {
		if got := listInstanceNames(t, s, "proj-one", c.filter, ""); !slices.Equal(got, c.want) {
			t.Errorf("filter %q = %v, want %v", c.filter, got, c.want)
		}
	}
	// A filtered list pages through the matching instances only.
	if got := listInstanceNames(t, s, "proj-one", `state=RUNNABLE`, "1"); !slices.Equal(got, []string{"a", "b", "d"}) {
		t.Errorf("paged filtered list = %v", got)
	}
}

// TestListInstancesFilterCompat checks the simple field:value terms
// existing callers send.
func TestListInstancesFilterCompat(t *testing.T) {
	s := testService(t, config.IAMOff)
	storeInstance(t, s, "proj-one", &sqladmin.DatabaseInstance{Name: "a", State: "RUNNABLE", Region: "us-east1",
		Settings: &sqladmin.Settings{UserLabels: map[string]string{"env": "dev"}}})
	for f, want := range map[string]bool{
		"":                                     true,
		"name:a":                               true,
		"name:b":                               false,
		"state:RUNNABLE AND region:us-east1":   true,
		"settings.userLabels.env:dev":          true,
		`settings.userLabels.env="prod"`:       false,
		"databaseVersion=POSTGRES_17 region:x": false,
	} {
		if got := len(listInstanceNames(t, s, "proj-one", f, "")) == 1; got != want {
			t.Errorf("filter %q = %v, want %v", f, got, want)
		}
	}
}

func TestListInstancesFilterRejected(t *testing.T) {
	s := testService(t, config.IAMOff)
	for _, c := range []struct{ filter, msg string }{
		{`foo:bar`, `unknown field \"foo\"`},
		{`settings.foo=x`, `unknown field \"settings.foo\"`},
		{`settings.userLabels.=x`, `unknown field \"settings.userLabels.\"`},
		{`settings.userLabels.env:*`, `does not support \":*\"`},
		{`name=a and state=RUNNABLE`, `at \"and\"`},
		{`name=a or name=b`, `at \"or\"`},
		{`name="a`, "unterminated"},
		{`(name=a`, `missing \")\"`},
		{`name`, "expected a field, an operator and a value"},
		{`name<a`, `field \"name\" does not support`},
	} {
		// The project has no instances: the error never depends on what
		// is stored.
		q := url.Values{"filter": {c.filter}}
		code, body := call(s, "GET", "/projects/proj-one/instances?"+q.Encode(), "")
		if code != 400 || !strings.Contains(body, "INVALID_ARGUMENT") || !strings.Contains(body, c.msg) {
			t.Errorf("filter %q: %d %s, want 400 containing %q", c.filter, code, body, c.msg)
		}
	}
}
