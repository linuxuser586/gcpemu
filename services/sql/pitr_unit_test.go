package sql

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	sqladmin "google.golang.org/api/sqladmin/v1beta4"

	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/store"
)

func TestPITRSettings(t *testing.T) {
	off := &sqladmin.Settings{DatabaseFlags: []*sqladmin.DatabaseFlags{{Name: "work_mem", Value: "4096"}}}
	on := &sqladmin.Settings{
		DatabaseFlags:       off.DatabaseFlags,
		BackupConfiguration: &sqladmin.BackupConfiguration{PointInTimeRecoveryEnabled: true},
	}
	got := pgSettings(serverFlags(on))
	if got["archive_mode"] != "on" || !strings.Contains(got["archive_command"], "/var/lib/gcpemu-archive/wal/%f") || got["work_mem"] != "4096" {
		t.Fatalf("PITR settings = %v", got)
	}
	if s := pgSettings(serverFlags(off)); s["archive_mode"] != "" || s["archive_command"] != "" {
		t.Fatalf("settings without PITR = %v", s)
	}
	if !needsRestart(serverFlags(off), serverFlags(on)) || !needsRestart(serverFlags(on), serverFlags(off)) {
		t.Error("toggling point-in-time recovery must restart PostgreSQL (archive_mode)")
	}
	if _, ok := flagMap(on)[pitrFlag]; ok {
		t.Error("the PITR marker must not appear among the instance's database flags")
	}
}

// putInstance stores a RUNNABLE instance record without a container.
func putInstance(t *testing.T, s *Service, project, name string, pitr bool) {
	t.Helper()
	rec := &instanceRecord{Instance: &sqladmin.DatabaseInstance{
		Name: name, Project: project, State: "RUNNABLE", DatabaseVersion: "POSTGRES_18", Region: "us-central1",
		Settings: &sqladmin.Settings{BackupConfiguration: &sqladmin.BackupConfiguration{PointInTimeRecoveryEnabled: pitr}},
	}}
	if err := s.env.Store.Update(func(tx store.Tx) error {
		return store.PutJSON(tx, nsInstances, instKey(project, name), rec)
	}); err != nil {
		t.Fatal(err)
	}
}

// call serves one v1beta4 request and returns the status and body.
func call(s *Service, method, path, body string) (int, string) {
	r := httptest.NewRequest(method, "/sql/v1beta4"+path, strings.NewReader(body))
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w.Code, w.Body.String()
}

func TestBackupAndPITRValidation(t *testing.T) {
	s := testService(t, config.IAMOff)
	putInstance(t, s, "proj-one", "plain", false)
	putInstance(t, s, "proj-one", "pitr", true)
	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)
	for _, c := range []struct {
		name, method, path, body string
		code                     int
		msg                      string
	}{
		{"clone without PITR", "POST", "/projects/proj-one/instances/plain/clone",
			`{"cloneContext":{"destinationInstanceName":"c1","pointInTime":"2026-01-01T00:00:00Z"}}`, 400, "Point-in-time recovery is not enabled"},
		{"clone into the future", "POST", "/projects/proj-one/instances/pitr/clone",
			`{"cloneContext":{"destinationInstanceName":"c1","pointInTime":"` + future + `"}}`, 400, "in the future"},
		{"clone with a bad time", "POST", "/projects/proj-one/instances/pitr/clone",
			`{"cloneContext":{"destinationInstanceName":"c1","pointInTime":"yesterday"}}`, 400, "pointInTime"},
		{"recovery time without PITR", "GET", "/projects/proj-one/instances/plain/getLatestRecoveryTime", "", 400, "Point-in-time recovery is not enabled"},
		{"restore a malformed backup name", "POST", "/projects/proj-one/instances/plain/restoreBackup",
			`{"backup":"backups/1"}`, 400, "projects/{project}/backups/{backup}"},
		{"restore an unknown backup", "POST", "/projects/proj-one/instances/plain/restoreBackup",
			`{"backup":"projects/proj-one/backups/42"}`, 404, "does not exist"},
		{"restore with two sources", "POST", "/projects/proj-one/instances/plain/restoreBackup",
			`{"backup":"projects/proj-one/backups/42","restoreBackupContext":{"backupRunId":"42"}}`, 400, "Only one of"},
		{"get an unknown backup", "GET", "/projects/proj-one/backups/42", "", 404, "does not exist"},
		{"delete an unknown backup", "DELETE", "/projects/proj-one/backups/42", "", 404, "does not exist"},
		{"create a backup of an unknown instance", "POST", "/projects/proj-one/backups", `{"instance":"nope"}`, 404, ""},
		{"create a backup without an instance", "POST", "/projects/proj-one/backups", `{}`, 400, "instance"},
		{"update without a mask", "PATCH", "/projects/proj-one/backups/42", `{"description":"x"}`, 400, "updateMask"},
	} {
		code, body := call(s, c.method, c.path, c.body)
		if code != c.code || !strings.Contains(body, c.msg) {
			t.Errorf("%s: %d %s, want %d containing %q", c.name, code, body, c.code, c.msg)
		}
	}
	code, body := call(s, "GET", "/projects/proj-one/backups", "")
	if code != 200 || strings.Contains(body, `"backups"`) {
		t.Errorf("empty list: %d %s", code, body)
	}
}
