package sql

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	sqladmin "google.golang.org/api/sqladmin/v1beta4"

	"github.com/linuxuser586/gcpemu/internal/clock"
	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// putBackup stores a backup of an instance without a container.
func putBackup(t *testing.T, s *Service, project string, b *backupRecord) {
	t.Helper()
	r := b.Run
	if r.Status == "" {
		r.Status = "SUCCESSFUL"
	}
	if r.Type == "" {
		r.Type = "ON_DEMAND"
	}
	if r.Location == "" {
		r.Location = "us-central1"
	}
	if err := s.env.Store.Update(func(tx store.Tx) error {
		return store.PutJSON(tx, nsBackups, b.key(project), b)
	}); err != nil {
		t.Fatal(err)
	}
}

// listIDs lists the project's backups with filter, following pages, and
// returns their IDs (the last element of each name).
func listIDs(t *testing.T, s *Service, project, filter, pageSize string) []string {
	t.Helper()
	var ids []string
	token := ""
	for {
		q := url.Values{"filter": {filter}, "pageSize": {pageSize}, "pageToken": {token}}
		code, body := call(s, "GET", "/projects/"+project+"/backups?"+q.Encode(), "")
		if code != 200 {
			t.Fatalf("list %q: %d %s", filter, code, body)
		}
		var resp sqladmin.ListBackupsResponse
		if err := json.Unmarshal([]byte(body), &resp); err != nil {
			t.Fatal(err)
		}
		for _, b := range resp.Backups {
			ids = append(ids, b.Name[len("projects/"+project+"/backups/"):])
		}
		if token = resp.NextPageToken; token == "" {
			slices.Sort(ids)
			return ids
		}
	}
}

func TestListBackupsFilter(t *testing.T) {
	s := testService(t, config.IAMOff)
	for _, b := range []*sqladmin.BackupRun{
		{Id: 1, Instance: "a", StartTime: "2026-01-01T00:00:00Z"},
		{Id: 2, Instance: "a", Type: "AUTOMATED", StartTime: "2026-02-01T00:00:00Z"},
		{Id: 3, Instance: "b", Location: "europe-west1", StartTime: "2026-03-01T00:00:00Z"},
		{Id: 4, Instance: "c", StartTime: "2026-04-01T00:00:00Z"},
	} {
		putBackup(t, s, "proj-one", &backupRecord{Run: b})
	}
	for _, c := range []struct {
		filter string
		want   []string
	}{
		{``, []string{"1", "2", "3", "4"}},
		{`instance="a"`, []string{"1", "2"}},
		{`instance=a`, []string{"1", "2"}},
		{`instance!="a"`, []string{"3", "4"}},
		{`instance:a`, []string{"1", "2"}},
		{`type="ON_DEMAND" AND instance="a"`, []string{"1"}},
		{`type="ON_DEMAND" instance="a"`, []string{"1"}},
		{`instance="a" OR instance="b"`, []string{"1", "2", "3"}},
		{`NOT instance="a"`, []string{"3", "4"}},
		{`(instance="a" OR instance="b") AND NOT type="AUTOMATED"`, []string{"1", "3"}},
		{`location="europe-west1"`, []string{"3"}},
		{`backupInterval.startTime>="2026-03-01T00:00:00.000+00:00"`, []string{"3", "4"}},
		{`backupInterval.startTime<"2026-02-01T00:00:00Z"`, []string{"1"}},
		{`backupInterval.startTime>"2026-01-01T01:00:00+01:00"`, []string{"2", "3", "4"}},
		{`instanceDeletionTime<"2030-01-01T00:00:00Z"`, nil},
		{`instanceDeletionTime!="2030-01-01T00:00:00Z"`, []string{"1", "2", "3", "4"}},
		{"type=\"ON_DEMAND\"\r\v\finstance=\"a\"", []string{"1"}},
		{"instance=\u00a0", nil},
	} {
		if got := listIDs(t, s, "proj-one", c.filter, ""); !slices.Equal(got, c.want) {
			t.Errorf("filter %q = %v, want %v", c.filter, got, c.want)
		}
	}
	// A filtered list pages through the matching backups only.
	if got := listIDs(t, s, "proj-one", `instance!="c"`, "1"); !slices.Equal(got, []string{"1", "2", "3"}) {
		t.Errorf("paged filtered list = %v", got)
	}
}

func TestListBackupsFilterRejected(t *testing.T) {
	s := testService(t, config.IAMOff)
	for _, c := range []struct{ filter, msg string }{
		{`state="SUCCESSFUL"`, `unknown field \"state\"`},
		{`bogus=1`, `unknown field \"bogus\"`},
		{`(instance="a"`, `missing \")\"`},
		{`instance="a")`, `unexpected \")\"`},
		{`instance="a`, "unterminated"},
		{`instance`, "expected a field, an operator and a value"},
		{`instance="a" AND`, "unexpected end"},
		{`instance<"a"`, `field \"instance\" does not support`},
		{`backupInterval.startTime:"2026"`, `does not support \":\"`},
		{`backupInterval.startTime>"yesterday"`, "not an RFC 3339 timestamp"},
	} {
		q := url.Values{"filter": {c.filter}}
		code, body := call(s, "GET", "/projects/proj-one/backups?"+q.Encode(), "")
		if code != 400 || !strings.Contains(body, "INVALID_ARGUMENT") || !strings.Contains(body, c.msg) {
			t.Errorf("filter %q: %d %s, want 400 containing %q", c.filter, code, body, c.msg)
		}
	}
}

// clockService is a testService on an advanceable emulator clock.
func clockService(t *testing.T) (*Service, *clock.Offset) {
	s := testService(t, config.IAMOff)
	clk := clock.NewOffset(clock.NewFake())
	s.env.Clock = clk
	return s, clk
}

// advance moves the emulator clock forward and applies what became due.
func advance(t *testing.T, s *Service, clk *clock.Offset, d time.Duration) {
	t.Helper()
	clk.Advance(d)
	if err := s.ClockAdvanced(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestBackupExpiryValidation(t *testing.T) {
	s, clk := clockService(t)
	putInstance(t, s, "proj-one", "a", false)
	putBackup(t, s, "proj-one", &backupRecord{Run: &sqladmin.BackupRun{Id: 1, Instance: "a"}})
	at := func(d time.Duration) string { return clk.Now().Add(d).Format(time.RFC3339Nano) }
	const day = 24 * time.Hour
	for _, c := range []struct {
		name, method, path, body string
		code                     int
		msg                      string
	}{
		{"create with ttlDays 366", "POST", "/projects/proj-one/backups", `{"instance":"a","ttlDays":"366"}`, 400, "ttlDays must be between 1 and 365"},
		{"update with ttlDays 366", "PATCH", "/projects/proj-one/backups/1?updateMask=ttlDays", `{"ttlDays":"366"}`, 400, "ttlDays must be between 1 and 365"},
		{"update with ttlDays -1", "PATCH", "/projects/proj-one/backups/1?updateMask=ttlDays", `{"ttlDays":"-1"}`, 400, "ttlDays must be between 1 and 365"},
		{"update with expiryTime 366 days ahead", "PATCH", "/projects/proj-one/backups/1?updateMask=expiryTime",
			`{"expiryTime":"` + at(366*day) + `"}`, 400, "at most 365 days"},
		{"update with ttlDays 365", "PATCH", "/projects/proj-one/backups/1?updateMask=ttlDays", `{"ttlDays":"365"}`, 200, "UPDATE_BACKUP"},
		{"update with expiryTime 364 days ahead", "PATCH", "/projects/proj-one/backups/1?updateMask=expiryTime",
			`{"expiryTime":"` + at(364*day) + `"}`, 200, "UPDATE_BACKUP"},
	} {
		code, body := call(s, c.method, c.path, c.body)
		if code != c.code || !strings.Contains(body, c.msg) {
			t.Errorf("%s: %d %s, want %d containing %q", c.name, code, body, c.code, c.msg)
		}
	}
}

func TestBackupExpiresTTLDaysAfterTheUpdate(t *testing.T) {
	s, clk := clockService(t)
	putBackup(t, s, "proj-one", &backupRecord{Run: &sqladmin.BackupRun{Id: 1, Instance: "a"}})
	if code, body := call(s, "PATCH", "/projects/proj-one/backups/1?updateMask=ttlDays", `{"ttlDays":"1"}`); code != 200 {
		t.Fatalf("update: %d %s", code, body)
	}
	advance(t, s, clk, 23*time.Hour)
	if code, body := call(s, "GET", "/projects/proj-one/backups/1", ""); code != 200 {
		t.Fatalf("after 23h: %d %s, want the backup", code, body)
	}
	advance(t, s, clk, 2*time.Hour)
	if code, body := call(s, "GET", "/projects/proj-one/backups/1", ""); code != 404 {
		t.Errorf("after 25h: %d %s, want 404", code, body)
	}
}

func TestBackupWithAPastExpiryGoesOnTheNextSweep(t *testing.T) {
	s, clk := clockService(t)
	putBackup(t, s, "proj-one", &backupRecord{Run: &sqladmin.BackupRun{Id: 1, Instance: "a"}})
	past := clk.Now().Add(-24 * time.Hour).Format(time.RFC3339Nano)
	if code, body := call(s, "PATCH", "/projects/proj-one/backups/1?updateMask=expiryTime", `{"expiryTime":"`+past+`"}`); code != 200 {
		t.Fatalf("update: %d %s", code, body)
	}
	advance(t, s, clk, 0)
	if code, body := call(s, "GET", "/projects/proj-one/backups/1", ""); code != 404 {
		t.Errorf("after the sweep: %d %s, want 404", code, body)
	}
}

func TestExpiredBackupsAreDeletedWhenTheClockAdvances(t *testing.T) {
	s, clk := clockService(t)
	start := clk.Now()
	dump := func(name string) string {
		p := filepath.Join(t.TempDir(), name)
		if err := os.WriteFile(p, []byte("-- dump"), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	expiring := dump("1.sql")
	putBackup(t, s, "proj-one", &backupRecord{Run: &sqladmin.BackupRun{Id: 1, Instance: "a"}, File: expiring,
		ExpiryTime: start.Add(24 * time.Hour).Format(time.RFC3339Nano)})
	putBackup(t, s, "proj-one", &backupRecord{Run: &sqladmin.BackupRun{Id: 2, Instance: "a", Status: "RUNNING"},
		ExpiryTime: start.Add(-time.Hour).Format(time.RFC3339Nano)})
	putBackup(t, s, "proj-one", &backupRecord{Run: &sqladmin.BackupRun{Id: 3, Instance: "a"}, File: dump("3.sql")})

	advance(t, s, clk, 23*time.Hour)
	if got := listIDs(t, s, "proj-one", "", ""); !slices.Equal(got, []string{"1", "2", "3"}) {
		t.Fatalf("after 23h: backups %v, want all three", got)
	}

	advance(t, s, clk, 2*time.Hour)
	if code, body := call(s, "GET", "/projects/proj-one/backups/1", ""); code != 404 {
		t.Errorf("expired backup: %d %s, want 404", code, body)
	}
	if _, err := os.Stat(expiring); !os.IsNotExist(err) {
		t.Errorf("expired backup's dump: %v, want removed", err)
	}
	// A RUNNING backup is kept until it finishes; one without an expiry
	// is kept.
	if got := listIDs(t, s, "proj-one", "", ""); !slices.Equal(got, []string{"2", "3"}) {
		t.Errorf("after 25h: backups %v, want [2 3]", got)
	}
	if _, body := call(s, "GET", "/projects/proj-one/operations", ""); strings.Contains(body, "DELETE_BACKUP") {
		t.Errorf("expiry recorded an operation: %s", body)
	}

	if err := s.updateBackupRecord("proj-one", "a", 2, func(b *backupRecord) { b.Run.Status = "SUCCESSFUL" }); err != nil {
		t.Fatal(err)
	}
	advance(t, s, clk, 0)
	if got := listIDs(t, s, "proj-one", "", ""); !slices.Equal(got, []string{"3"}) {
		t.Errorf("after the RUNNING backup finished: backups %v, want [3]", got)
	}
}
