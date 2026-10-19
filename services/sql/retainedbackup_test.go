package sql

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	sqladmin "google.golang.org/api/sqladmin/v1beta4"

	"github.com/linuxuser586/gcpemu/internal/clock"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// callV1 serves one v1 request and returns the status and body.
func callV1(s *Service, method, path, body string) (int, string) {
	r := httptest.NewRequest(method, "/v1"+path, strings.NewReader(body))
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w.Code, w.Body.String()
}

// waitOpDone waits for the operation in a v1beta4 response body.
func waitOpDone(t *testing.T, s *Service, project, body string) *sqladmin.Operation {
	t.Helper()
	var op sqladmin.Operation
	if err := json.Unmarshal([]byte(body), &op); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); op.Status != "DONE"; {
		if time.Now().After(deadline) {
			t.Fatalf("operation %s not done", op.Name)
		}
		time.Sleep(10 * time.Millisecond)
		_, body = call(s, "GET", "/projects/"+project+"/operations/"+op.Name, "")
		_ = json.Unmarshal([]byte(body), &op)
	}
	return &op
}

func TestRetainBackupsOnDeleteIsStored(t *testing.T) {
	s, _ := fileService(t)
	type caller func(s *Service, method, path, body string) (int, string)
	settings := func(t *testing.T, call caller, name string) *sqladmin.Settings {
		t.Helper()
		code, body := call(s, "GET", "/projects/proj-one/instances/"+name, "")
		if code != 200 {
			t.Fatalf("%d %s", code, body)
		}
		var in sqladmin.DatabaseInstance
		if err := json.Unmarshal([]byte(body), &in); err != nil {
			t.Fatal(err)
		}
		return in.Settings
	}
	for _, api := range []struct {
		name string
		call caller
	}{{"v1beta4", call}, {"v1", callV1}} {
		t.Run(api.name, func(t *testing.T) {
			name := "keep-" + strings.ReplaceAll(api.name, "beta", "b")
			if code, body := api.call(s, "POST", "/projects/proj-one/instances",
				`{"name":"`+name+`","databaseVersion":"POSTGRES_18","settings":{"tier":"db-perf-optimized-N-2","retainBackupsOnDelete":true}}`); code != 200 {
				t.Fatalf("insert: %d %s", code, body)
			}
			if st := settings(t, api.call, name); !st.RetainBackupsOnDelete {
				t.Errorf("after insert: retainBackupsOnDelete false, want true")
			}
			// Patch and update need the instance created; a store-only one
			// stands in.
			patched := name + "-p"
			putInstance(t, s, "proj-one", patched, false)
			if err := s.updateRecord("proj-one", patched, func(r *instanceRecord) error {
				applySettingsDefaults(r.Instance.Settings, nil, "", r.Instance.DatabaseVersion, true)
				r.Instance.Settings.IpConfiguration.Ipv4Enabled = true
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			for _, c := range []struct {
				method string
				want   bool
			}{{"PATCH", true}, {"PATCH", false}, {"PUT", true}, {"PUT", false}} {
				code, body := api.call(s, c.method, "/projects/proj-one/instances/"+patched, fmt.Sprintf(
					`{"settings":{"tier":"db-perf-optimized-N-2","ipConfiguration":{"ipv4Enabled":true},"retainBackupsOnDelete":%t}}`, c.want))
				if code != 200 {
					t.Fatalf("%s to %t: %d %s", c.method, c.want, code, body)
				}
				if op := waitOpDone(t, s, "proj-one", body); op.Error != nil {
					t.Fatalf("%s to %t: %+v", c.method, c.want, op.Error.Errors[0])
				}
				if got := settings(t, api.call, patched).RetainBackupsOnDelete; got != c.want {
					t.Errorf("after %s to %t: retainBackupsOnDelete %t", c.method, c.want, got)
				}
			}
		})
	}
	if code, body := call(s, "POST", "/projects/proj-one/instances",
		`{"name":"plain","databaseVersion":"POSTGRES_18","settings":{"tier":"db-perf-optimized-N-2"}}`); code != 200 {
		t.Fatalf("insert: %d %s", code, body)
	}
	if st := settings(t, call, "plain"); st.RetainBackupsOnDelete {
		t.Errorf("default retainBackupsOnDelete true, want false")
	}
}

// fileService is a clockService whose backup dumps go to a temporary
// directory.
func fileService(t *testing.T) (*Service, *clock.Offset) {
	s, clk := clockService(t)
	s.env.DataDir = t.TempDir()
	return s, clk
}

// putRetainingInstance stores a store-only Cloud SQL instance whose
// settings.retainBackupsOnDelete is retain.
func putRetainingInstance(t *testing.T, s *Service, project, name string, retain bool) {
	t.Helper()
	putInstance(t, s, project, name, false)
	if err := s.updateRecord(project, name, func(r *instanceRecord) error {
		r.Instance.Settings.RetainBackupsOnDelete = retain
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// putBackupWithFile stores an on-demand backup of the instance with a dump
// file where a backup run writes one, and returns the file.
func putBackupWithFile(t *testing.T, s *Service, project string, run *sqladmin.BackupRun) string {
	t.Helper()
	file, err := s.backupFile(project, run.Instance, run.Id)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("-- dump"), 0o600); err != nil {
		t.Fatal(err)
	}
	putBackup(t, s, project, &backupRecord{Run: run, File: file})
	return file
}

// storedFile returns the stored backup's dump file, which must exist.
func storedFile(t *testing.T, s *Service, project string, id int64) string {
	t.Helper()
	b, ok := s.findBackup(project, id)
	if !ok {
		t.Fatalf("backup %d not stored", id)
	}
	if _, err := os.Stat(b.File); err != nil {
		t.Fatalf("dump file of backup %d: %v", id, err)
	}
	return b.File
}

func TestDeleteRetainsOnDemandBackups(t *testing.T) {
	s, clk := fileService(t)
	putRetainingInstance(t, s, "proj-one", "a", true)
	putBackupWithFile(t, s, "proj-one", &sqladmin.BackupRun{Id: 1, Instance: "a", Description: "first"})
	putBackupWithFile(t, s, "proj-one", &sqladmin.BackupRun{Id: 2, Instance: "a", Description: "broken", Status: "FAILED"})
	before := clk.Now().UTC()

	deleteAndWait(t, s, "proj-one", "a", "")

	got := getBackups(t, s, "proj-one", "")
	if len(got) != 2 {
		t.Fatalf("backups after delete: %d, want the 2 on-demand backups retained", len(got))
	}
	want := map[string]struct{ desc, state string }{
		"projects/proj-one/backups/1": {"first", "SUCCESSFUL"},
		"projects/proj-one/backups/2": {"broken", "FAILED"},
	}
	for _, b := range got {
		w, ok := want[b.Name]
		if !ok {
			t.Errorf("unexpected backup %s", b.Name)
			continue
		}
		if b.Type != "ON_DEMAND" || b.Instance != "a" || b.Description != w.desc || b.State != w.state {
			t.Errorf("%s: type %q instance %q description %q state %q, want ON_DEMAND a %q %q", b.Name, b.Type, b.Instance, b.Description, b.State, w.desc, w.state)
		}
		deletedAt(t, b, before, clk.Now())
		if in := b.InstanceSettings; in == nil || in.Name != "a" || !in.Settings.RetainBackupsOnDelete {
			t.Errorf("%s: instanceSettings %+v, want the deleted Cloud SQL instance", b.Name, in)
		}
		if b.ExpiryTime != "" {
			t.Errorf("%s: expiryTime %q, want none", b.Name, b.ExpiryTime)
		}
	}
	storedFile(t, s, "proj-one", 1)
	storedFile(t, s, "proj-one", 2)
	if got := listIDs(t, s, "proj-one", `type="ON_DEMAND" AND instanceDeletionTime>"2000-01-01T00:00:00Z"`, ""); len(got) != 2 {
		t.Errorf("filtered on type and instanceDeletionTime: %v, want both Retained backups", got)
	}
}

func TestDeleteFailsABackupRunStillRunning(t *testing.T) {
	s, _ := fileService(t)
	putRetainingInstance(t, s, "proj-one", "a", true)
	putBackup(t, s, "proj-one", &backupRecord{Run: &sqladmin.BackupRun{Id: 1, Instance: "a", Status: "RUNNING"}})

	deleteAndWait(t, s, "proj-one", "a", "")

	got := getBackups(t, s, "proj-one", "")
	if len(got) != 1 || got[0].State != "FAILED" || got[0].Error == nil || !strings.Contains(got[0].Error.Message, "deleted before the backup finished") {
		t.Fatalf("backups after delete: %+v, want the running backup retained FAILED", got)
	}
}

func TestDeleteWithoutRetainBackupsOnDeleteDeletesBackups(t *testing.T) {
	s, _ := fileService(t)
	putRetainingInstance(t, s, "proj-one", "a", false)
	file := putBackupWithFile(t, s, "proj-one", &sqladmin.BackupRun{Id: 1, Instance: "a"})

	deleteAndWait(t, s, "proj-one", "a", "")

	if got := listIDs(t, s, "proj-one", "", ""); len(got) != 0 {
		t.Errorf("backups after delete: %v, want none", got)
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Errorf("dump file after delete: %v, want it removed", err)
	}
}

func TestRetainedBackupIsNotABackupRunOfAnInstanceReusingTheName(t *testing.T) {
	s, _ := fileService(t)
	putRetainingInstance(t, s, "proj-one", "a", true)
	putBackupWithFile(t, s, "proj-one", &sqladmin.BackupRun{Id: 1, Instance: "a"})
	deleteAndWait(t, s, "proj-one", "a", "")

	putRetainingInstance(t, s, "proj-one", "a", true)
	code, body := call(s, "GET", "/projects/proj-one/instances/a/backupRuns", "")
	if code != 200 || strings.Contains(body, `"id":"1"`) {
		t.Errorf("backupRuns of the new instance: %d %s, want no Retained backup", code, body)
	}
	if code, body := call(s, "GET", "/projects/proj-one/instances/a/backupRuns/1", ""); code != 404 {
		t.Errorf("backupRuns.get of the Retained backup on the new instance: %d %s, want 404", code, body)
	}
	for _, inst := range []string{"a", "a" + deletedSuffix} {
		code, body := call(s, "POST", "/projects/proj-one/instances/a/restoreBackup",
			`{"restoreBackupContext":{"backupRunId":"1","instanceId":"`+inst+`"}}`)
		if code != 404 {
			t.Errorf("restoreBackupContext of the Retained backup as instance %q: %d %s, want 404", inst, code, body)
		}
	}

	// The new instance's own backup is retained beside the earlier one.
	putBackupWithFile(t, s, "proj-one", &sqladmin.BackupRun{Id: 2, Instance: "a"})
	deleteAndWait(t, s, "proj-one", "a", "")
	if got := listIDs(t, s, "proj-one", "", ""); len(got) != 2 {
		t.Errorf("backups after deleting the new instance: %v, want both Retained backups", got)
	}
	storedFile(t, s, "proj-one", 1)
	storedFile(t, s, "proj-one", 2)

	// Deleting a later instance without retention leaves them too.
	putRetainingInstance(t, s, "proj-one", "a", false)
	deleteAndWait(t, s, "proj-one", "a", "")
	if got := listIDs(t, s, "proj-one", "", ""); len(got) != 2 {
		t.Errorf("backups after deleting a non-retaining instance of the name: %v, want both Retained backups", got)
	}
	storedFile(t, s, "proj-one", 1)
}

func TestBackupsDeleteRemovesARetainedBackup(t *testing.T) {
	s, _ := fileService(t)
	putRetainingInstance(t, s, "proj-one", "a", true)
	putBackupWithFile(t, s, "proj-one", &sqladmin.BackupRun{Id: 1, Instance: "a"})
	deleteAndWait(t, s, "proj-one", "a", "")
	file := storedFile(t, s, "proj-one", 1)

	code, body := call(s, "DELETE", "/projects/proj-one/backups/1", "")
	if code != 200 {
		t.Fatalf("delete: %d %s", code, body)
	}
	if op := waitOpDone(t, s, "proj-one", body); op.Error != nil {
		t.Fatalf("delete: %+v", op.Error.Errors[0])
	}
	if code, body := call(s, "GET", "/projects/proj-one/backups/1", ""); code != 404 {
		t.Errorf("get after delete: %d %s, want 404", code, body)
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Errorf("dump file after delete: %v, want it removed", err)
	}
}

func TestDeleteTakesAFinalBackupAndRetainsBackups(t *testing.T) {
	s, _ := fileService(t)
	putRetainingInstance(t, s, "proj-one", "a", true)
	putBackupWithFile(t, s, "proj-one", &sqladmin.BackupRun{Id: 1, Instance: "a"})
	putBackupWithFile(t, s, "proj-one", &sqladmin.BackupRun{Id: 2, Instance: "a"})

	deleteAndWait(t, s, "proj-one", "a", "?enableFinalBackup=true")

	if got := listIDs(t, s, "proj-one", `type="FINAL"`, ""); len(got) != 1 {
		t.Errorf("Final backups: %v, want 1", got)
	}
	retained := getBackups(t, s, "proj-one", `type="ON_DEMAND"`)
	if len(retained) != 2 {
		t.Fatalf("Retained backups: %d, want 2", len(retained))
	}
	for _, b := range retained {
		if b.ExpiryTime != "" {
			t.Errorf("%s: expiryTime %q, want none (the Final backup's expiry is its own)", b.Name, b.ExpiryTime)
		}
	}
}

func TestRetainedBackupExpiryUpdateIsRefused(t *testing.T) {
	s, _ := fileService(t)
	putRetainingInstance(t, s, "proj-one", "a", true)
	putBackupWithFile(t, s, "proj-one", &sqladmin.BackupRun{Id: 1, Instance: "a"})
	deleteAndWait(t, s, "proj-one", "a", "")

	// A Retained backup is an on-demand backup, which has no expiry.
	code, body := call(s, "PATCH", "/projects/proj-one/backups/1?updateMask=ttlDays", `{"ttlDays":"300"}`)
	if code != 400 || !strings.Contains(body, "final backup") {
		t.Fatalf("update: %d %s, want 400 naming a final backup", code, body)
	}
	if b := getBackups(t, s, "proj-one", "")[0]; b.ExpiryTime != "" {
		t.Errorf("expiryTime %q, want none", b.ExpiryTime)
	}
}

func TestFinalBackupStoredByV090IsMigrated(t *testing.T) {
	s, _ := fileService(t)
	b := &backupRecord{Run: &sqladmin.BackupRun{Id: 1, Instance: "a", Type: "FINAL", Status: "SUCCESSFUL"}, InstanceDeletionTime: "2026-01-01T00:00:00Z"}
	if err := s.env.Store.Update(func(tx store.Tx) error {
		return store.PutJSON(tx, nsBackups, childKey("proj-one", "a"+legacyFinalSuffix, backupKey(1)), b)
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Stop(context.Background()) })

	code, body := call(s, "DELETE", "/projects/proj-one/backups/1", "")
	if code != 200 {
		t.Fatalf("delete: %d %s", code, body)
	}
	if op := waitOpDone(t, s, "proj-one", body); op.Error != nil {
		t.Fatalf("delete of the migrated Final backup: %+v", op.Error.Errors[0])
	}
	if got := listIDs(t, s, "proj-one", "", ""); len(got) != 0 {
		t.Errorf("backups after delete: %v, want none", got)
	}
}
