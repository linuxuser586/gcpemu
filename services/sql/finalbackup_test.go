package sql

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	sqladmin "google.golang.org/api/sqladmin/v1beta4"
)

// deleteAndWait deletes a store-only Cloud SQL instance with query and
// waits for its DELETE operation. Without a container runtime a Final
// backup's dump fails, so its backup is FAILED, but it is still kept.
func deleteAndWait(t *testing.T, s *Service, project, name, query string) {
	t.Helper()
	code, body := call(s, "DELETE", "/projects/"+project+"/instances/"+name+query, "")
	if code != 200 {
		t.Fatalf("delete %s%s: %d %s", name, query, code, body)
	}
	var op sqladmin.Operation
	if err := json.Unmarshal([]byte(body), &op); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); op.Status != "DONE"; {
		if time.Now().After(deadline) {
			t.Fatalf("delete %s: operation %s not done", name, op.Name)
		}
		time.Sleep(10 * time.Millisecond)
		_, body = call(s, "GET", "/projects/"+project+"/operations/"+op.Name, "")
		_ = json.Unmarshal([]byte(body), &op)
	}
}

// getBackups lists the project's backups matching filter.
func getBackups(t *testing.T, s *Service, project, filter string) []*sqladmin.Backup {
	t.Helper()
	var out []*sqladmin.Backup
	for _, id := range listIDs(t, s, project, filter, "") {
		code, body := call(s, "GET", "/projects/"+project+"/backups/"+id, "")
		if code != 200 {
			t.Fatalf("get backup %s: %d %s", id, code, body)
		}
		var b sqladmin.Backup
		if err := json.Unmarshal([]byte(body), &b); err != nil {
			t.Fatal(err)
		}
		out = append(out, &b)
	}
	return out
}

// deletedAt returns b's instanceDeletionTime, which must be between
// before and after.
func deletedAt(t *testing.T, b *sqladmin.Backup, before, after time.Time) time.Time {
	t.Helper()
	at, err := time.Parse(time.RFC3339Nano, b.InstanceDeletionTime)
	if err != nil || at.Before(before) || at.After(after) {
		t.Fatalf("instanceDeletionTime %q, want a time of the delete (%v..%v)", b.InstanceDeletionTime, before, after)
	}
	return at
}

func TestDeleteWithFinalBackupKeepsAFinalBackup(t *testing.T) {
	s, clk := clockService(t)
	putInstance(t, s, "proj-one", "a", false)
	putBackup(t, s, "proj-one", &backupRecord{Run: &sqladmin.BackupRun{Id: 1, Instance: "a"}})
	deleted := clk.Now().UTC()

	deleteAndWait(t, s, "proj-one", "a", "?enableFinalBackup=true&finalBackupTtlDays=7&finalBackupDescription=last")

	if code, body := call(s, "GET", "/projects/proj-one/instances/a", ""); code != 404 {
		t.Fatalf("instance after delete: %d %s, want 404", code, body)
	}
	got := getBackups(t, s, "proj-one", "")
	if len(got) != 1 {
		t.Fatalf("backups after delete: %d, want only the Final backup (the on-demand one goes with the instance)", len(got))
	}
	b := got[0]
	if b.Type != "FINAL" || b.Instance != "a" || b.Description != "last" {
		t.Errorf("Final backup: type %q instance %q description %q, want FINAL a last", b.Type, b.Instance, b.Description)
	}
	if in := b.InstanceSettings; in == nil || in.Name != "a" || in.DatabaseVersion != "POSTGRES_18" {
		t.Errorf("instanceSettings %+v, want the deleted Cloud SQL instance", in)
	}
	at := deletedAt(t, b, deleted, clk.Now())
	if want := at.Add(7 * 24 * time.Hour).Format(time.RFC3339Nano); b.ExpiryTime != want {
		t.Errorf("expiryTime %q, want %q (7 days after the deletion)", b.ExpiryTime, want)
	}
}

// putConfiguredInstance stores a store-only Cloud SQL instance whose
// settings.finalBackupConfig is cfg.
func putConfiguredInstance(t *testing.T, s *Service, project, name string, cfg *sqladmin.FinalBackupConfig) {
	t.Helper()
	putInstance(t, s, project, name, false)
	if err := s.updateRecord(project, name, func(r *instanceRecord) error {
		r.Instance.Settings.FinalBackupConfig = cfg
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestFinalBackupExpiry(t *testing.T) {
	const day = 24 * time.Hour
	for _, c := range []struct {
		name  string
		cfg   *sqladmin.FinalBackupConfig
		query string
		after time.Duration // after the deletion; 0 means the expiryTime asked for
	}{
		{"defaults to 30 days", nil, "?enableFinalBackup=true", 30 * day},
		{"from finalBackupConfig without delete parameters", &sqladmin.FinalBackupConfig{Enabled: true, RetentionDays: 10}, "", 10 * day},
		{"finalBackupConfig without retentionDays defaults to 30 days", &sqladmin.FinalBackupConfig{Enabled: true}, "", 30 * day},
		{"finalBackupTtlDays wins over finalBackupConfig", &sqladmin.FinalBackupConfig{Enabled: true, RetentionDays: 10}, "?finalBackupTtlDays=3", 3 * day},
		{"finalBackupExpiryTime as given", nil, "?enableFinalBackup=true&finalBackupExpiryTime=", 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, clk := clockService(t)
			putConfiguredInstance(t, s, "proj-one", "a", c.cfg)
			before := clk.Now().UTC()
			query, asked := c.query, ""
			if c.after == 0 {
				asked = before.Add(40 * day).Format(time.RFC3339Nano)
				query += url.QueryEscape(asked)
			}
			deleteAndWait(t, s, "proj-one", "a", query)
			got := getBackups(t, s, "proj-one", `type="FINAL"`)
			if len(got) != 1 {
				t.Fatalf("Final backups: %d, want 1", len(got))
			}
			want := asked
			if c.after != 0 {
				want = deletedAt(t, got[0], before, clk.Now()).Add(c.after).Format(time.RFC3339Nano)
			}
			if got[0].ExpiryTime != want {
				t.Errorf("expiryTime %q, want %q", got[0].ExpiryTime, want)
			}
		})
	}
}

func TestDeleteRejectsInvalidFinalBackupParameters(t *testing.T) {
	s, clk := clockService(t)
	putInstance(t, s, "proj-one", "plain", false)
	putConfiguredInstance(t, s, "proj-one", "configured", &sqladmin.FinalBackupConfig{Enabled: true})
	in := func(d time.Duration) string { return url.QueryEscape(clk.Now().Add(d).Format(time.RFC3339Nano)) }
	for _, c := range []struct {
		name, instance, query, msg string
	}{
		{"both finalBackupTtlDays and finalBackupExpiryTime", "plain", "?enableFinalBackup=true&finalBackupTtlDays=7&finalBackupExpiryTime=" + in(time.Hour), "Only one of finalBackupTtlDays and finalBackupExpiryTime"},
		{"finalBackupTtlDays 0", "plain", "?enableFinalBackup=true&finalBackupTtlDays=0", "between 1 and 365"},
		{"finalBackupTtlDays 366", "plain", "?enableFinalBackup=true&finalBackupTtlDays=366", "between 1 and 365"},
		{"finalBackupExpiryTime 366 days ahead", "plain", "?enableFinalBackup=true&finalBackupExpiryTime=" + in(366*24*time.Hour), "at most 365 days"},
		{"finalBackupExpiryTime not a timestamp", "plain", "?enableFinalBackup=true&finalBackupExpiryTime=soon", "not an RFC 3339 timestamp"},
		{"enableFinalBackup not a boolean", "plain", "?enableFinalBackup=maybe", "not a boolean"},
		{"enableFinalBackup false against finalBackupConfig", "configured", "?enableFinalBackup=false", "finalBackupConfig takes a final backup"},
		{"finalBackupTtlDays without a Final backup", "plain", "?finalBackupTtlDays=7", "need a final backup"},
		{"finalBackupDescription with enableFinalBackup false", "plain", "?enableFinalBackup=false&finalBackupDescription=x", "need a final backup"},
	} {
		code, body := call(s, "DELETE", "/projects/proj-one/instances/"+c.instance+c.query, "")
		if code != 400 || !strings.Contains(body, c.msg) {
			t.Errorf("%s: %d %s, want 400 containing %q", c.name, code, body, c.msg)
		}
		if code, body := call(s, "GET", "/projects/proj-one/instances/"+c.instance, ""); code != 200 || strings.Contains(body, "PENDING_DELETE") {
			t.Errorf("%s: instance afterwards %d %s, want it RUNNABLE", c.name, code, body)
		}
	}
	if got := listIDs(t, s, "proj-one", "", ""); len(got) != 0 {
		t.Errorf("backups after rejected deletes: %v, want none", got)
	}
}

func TestFinalBackupIsNotABackupRunOfAnInstanceReusingTheName(t *testing.T) {
	s, _ := clockService(t)
	putInstance(t, s, "proj-one", "a", false)
	deleteAndWait(t, s, "proj-one", "a", "?enableFinalBackup=true")
	final := listIDs(t, s, "proj-one", `type="FINAL"`, "")
	if len(final) != 1 {
		t.Fatalf("Final backups: %v, want 1", final)
	}

	putInstance(t, s, "proj-one", "a", false)
	code, body := call(s, "GET", "/projects/proj-one/instances/a/backupRuns", "")
	if code != 200 || strings.Contains(body, final[0]) {
		t.Errorf("backupRuns of the new instance: %d %s, want no Final backup %s", code, body, final[0])
	}
	if code, body := call(s, "GET", "/projects/proj-one/instances/a/backupRuns/"+final[0], ""); code != 404 {
		t.Errorf("backupRuns.get of the Final backup on the new instance: %d %s, want 404", code, body)
	}

	deleteAndWait(t, s, "proj-one", "a", "")
	if got := listIDs(t, s, "proj-one", "", ""); len(got) != 1 || got[0] != final[0] {
		t.Errorf("backups after deleting the new instance: %v, want the earlier Final backup %v", got, final)
	}
	// A filter on instanceDeletionTime tells it from a live instance's.
	if got := listIDs(t, s, "proj-one", `instanceDeletionTime>"2000-01-01T00:00:00Z"`, ""); len(got) != 1 {
		t.Errorf("backups with an instanceDeletionTime: %v, want the Final backup", got)
	}
}

func TestFinalBackupExpiryUpdateIsCappedFromTheDeletion(t *testing.T) {
	const day = 24 * time.Hour
	s, clk := clockService(t)
	putInstance(t, s, "proj-one", "a", false)
	before := clk.Now().UTC()
	deleteAndWait(t, s, "proj-one", "a", "?enableFinalBackup=true")
	b := getBackups(t, s, "proj-one", `type="FINAL"`)[0]
	deleted := deletedAt(t, b, before, clk.Now())
	id := b.Name[strings.LastIndex(b.Name, "/")+1:]
	clk.Advance(100 * day)
	path := "/projects/proj-one/backups/" + id + "?updateMask="
	for _, c := range []struct {
		name, mask, body string
		code             int
	}{
		{"ttlDays reaching past 365 days after the deletion", "ttlDays", `{"ttlDays":"300"}`, 400},
		{"expiryTime 366 days after the deletion", "expiryTime", `{"expiryTime":"` + deleted.Add(366*day).Format(time.RFC3339Nano) + `"}`, 400},
		{"expiryTime 364 days after the deletion", "expiryTime", `{"expiryTime":"` + deleted.Add(364*day).Format(time.RFC3339Nano) + `"}`, 200},
		{"ttlDays 264, within 365 days of the deletion", "ttlDays", `{"ttlDays":"264"}`, 200},
		{"description", "description", `{"description":"kept"}`, 200},
	} {
		code, body := call(s, "PATCH", path+c.mask, c.body)
		if code != c.code {
			t.Errorf("%s: %d %s, want %d", c.name, code, body, c.code)
		}
		if code == 400 && !strings.Contains(body, "365 days after the instance's deletion") {
			t.Errorf("%s: %s, want the limit named", c.name, body)
		}
	}
	b = getBackups(t, s, "proj-one", `type="FINAL"`)[0]
	if b.Description != "kept" {
		t.Errorf("description %q, want kept", b.Description)
	}
}

func TestFinalBackupExpiresOnTheEmulatorClock(t *testing.T) {
	const day = 24 * time.Hour
	s, clk := clockService(t)
	putInstance(t, s, "proj-one", "a", false)
	deleteAndWait(t, s, "proj-one", "a", "?enableFinalBackup=true&finalBackupTtlDays=2")
	advance(t, s, clk, day)
	if got := listIDs(t, s, "proj-one", `type="FINAL"`, ""); len(got) != 1 {
		t.Fatalf("after a day: Final backups %v, want 1", got)
	}
	advance(t, s, clk, 2*day)
	if got := listIDs(t, s, "proj-one", "", ""); len(got) != 0 {
		t.Errorf("after 3 days: backups %v, want the Final backup expired", got)
	}
}

func TestFinalBackupConfigIsStoredAndValidated(t *testing.T) {
	s, _ := clockService(t)
	insert := func(name string, days int) (int, string) {
		return call(s, "POST", "/projects/proj-one/instances", fmt.Sprintf(
			`{"name":%q,"databaseVersion":"POSTGRES_18","settings":{"tier":"db-perf-optimized-N-2","finalBackupConfig":{"enabled":true,"retentionDays":%d}}}`, name, days))
	}
	for _, days := range []int{-1, 366} {
		if code, body := insert(fmt.Sprintf("bad%d", days+1), days); code != 400 || !strings.Contains(body, "retentionDays must be between 1 and 365") {
			t.Errorf("insert with retentionDays %d: %d %s, want 400", days, code, body)
		}
	}
	if code, body := insert("good", 12); code != 200 {
		t.Fatalf("insert with retentionDays 12: %d %s", code, body)
	}
	_, body := call(s, "GET", "/projects/proj-one/instances/good", "")
	var in sqladmin.DatabaseInstance
	if err := json.Unmarshal([]byte(body), &in); err != nil {
		t.Fatal(err)
	}
	if c := in.Settings.FinalBackupConfig; c == nil || !c.Enabled || c.RetentionDays != 12 {
		t.Errorf("finalBackupConfig %+v, want enabled with retentionDays 12", c)
	}
	// Patched settings are checked too (TestFinalBackupRestoresADeletedInstance
	// patches one in).
	putInstance(t, s, "proj-one", "stored", false)
	if code, body := call(s, "PATCH", "/projects/proj-one/instances/stored", `{"settings":{"finalBackupConfig":{"enabled":true,"retentionDays":400}}}`); code != 400 || !strings.Contains(body, "retentionDays") {
		t.Errorf("patch with retentionDays 400: %d %s, want 400", code, body)
	}
}
