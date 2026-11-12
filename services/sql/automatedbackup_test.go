package sql

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	sqladmin "google.golang.org/api/sqladmin/v1beta4"

	"github.com/linuxuser586/gcpemu/internal/clock"
)

// Automated backups (FR-SQL-008's daily automated backups at
// backupConfiguration.startTime kept to retainedBackups, ADR 0069, #1038).

func TestLastBackupSlot(t *testing.T) {
	at := func(s string) time.Time {
		v, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	for _, c := range []struct {
		now, start, want string
	}{
		{"2026-03-10T12:00:00Z", "00:00", "2026-03-10T00:00:00Z"},
		{"2026-03-10T00:00:00Z", "00:00", "2026-03-10T00:00:00Z"},
		{"2026-03-10T23:59:00Z", "00:00", "2026-03-10T00:00:00Z"},
		{"2026-03-10T03:59:00Z", "04:00", "2026-03-09T04:00:00Z"},
		{"2026-03-01T03:00:00Z", "23:30", "2026-02-28T23:30:00Z"},
		// The slot is in UTC whatever the clock's zone.
		{"2026-03-10T01:00:00-05:00", "05:00", "2026-03-10T05:00:00Z"},
	} {
		got, ok := lastBackupSlot(at(c.now), c.start)
		if !ok || !got.Equal(at(c.want)) {
			t.Errorf("lastBackupSlot(%s, %s) = %v, %t; want %s", c.now, c.start, got, ok, c.want)
		}
	}
	for _, bad := range []string{"", "24:00", "7am", "12:60", "7:00", "07:00:00", " 07:00", "07:00 "} {
		if _, ok := lastBackupSlot(at("2026-03-10T12:00:00Z"), bad); ok {
			t.Errorf("lastBackupSlot with startTime %q: ok, want not", bad)
		}
	}
}

// autoService is a fileService whose backups dump without a container.
func autoService(t *testing.T) (*Service, *clock.Offset) {
	s, clk := fileService(t)
	s.dump = func(_ context.Context, _, _ string, w io.Writer) error {
		_, err := io.WriteString(w, "-- dump")
		return err
	}
	return s, clk
}

// putWithAutomatedBackups stores a running store-only Cloud SQL instance with
// Automated backups at start, keeping retained of them, and lets a sweep
// find it, as the emulator would soon after it was stored.
func putWithAutomatedBackups(t *testing.T, s *Service, clk *clock.Offset, name, start string, retained int64) {
	t.Helper()
	putInstance(t, s, "proj-one", name, false)
	if err := s.updateRecord("proj-one", name, func(r *instanceRecord) error {
		st := r.Instance.Settings
		applySettingsDefaults(st, nil, "", r.Instance.DatabaseVersion, true)
		st.ActivationPolicy = "ALWAYS"
		st.BackupConfiguration.Enabled = true
		st.BackupConfiguration.StartTime = start
		st.BackupConfiguration.BackupRetentionSettings = &sqladmin.BackupRetentionSettings{RetentionUnit: "COUNT", RetainedBackups: retained}
		// 1 day of logs, so that any retainedBackups is valid (#1044).
		st.BackupConfiguration.TransactionLogRetentionDays = 1
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	advance(t, s, clk, 0)
}

// backupRuns lists the instance's backup runs of type typ, newest first.
func backupRuns(t *testing.T, s *Service, name, typ string) []*sqladmin.BackupRun {
	t.Helper()
	code, body := call(s, "GET", "/projects/proj-one/instances/"+name+"/backupRuns", "")
	if code != 200 {
		t.Fatalf("backupRuns.list: %d %s", code, body)
	}
	var resp sqladmin.BackupRunsListResponse
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatal(err)
	}
	var out []*sqladmin.BackupRun
	for _, r := range resp.Items {
		if r.Type == typ {
			out = append(out, r)
		}
	}
	return out
}

// backupOps lists the instance's BACKUP_VOLUME Operations.
func backupOps(t *testing.T, s *Service, name string) []*sqladmin.Operation {
	t.Helper()
	code, body := call(s, "GET", "/projects/proj-one/operations?instance="+name, "")
	if code != 200 {
		t.Fatalf("operations.list: %d %s", code, body)
	}
	var resp sqladmin.OperationsListResponse
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatal(err)
	}
	var out []*sqladmin.Operation
	for _, op := range resp.Items {
		if op.OperationType == "BACKUP_VOLUME" {
			out = append(out, op)
		}
	}
	return out
}

// TestAutomatedBackupAtStartTime covers FR-SQL-008's daily automated
// backups at backupConfiguration.startTime; the other tests in this file
// cover the rest of the clause (catch-up, skips, retention, Retained
// Automated backups).
func TestAutomatedBackupAtStartTime(t *testing.T) {
	s, clk := autoService(t) // 2026-01-01T00:00Z
	putWithAutomatedBackups(t, s, clk, "a", "02:00", 7)

	advance(t, s, clk, time.Hour)
	if got := backupRuns(t, s, "a", "AUTOMATED"); len(got) != 0 {
		t.Fatalf("before startTime: %d Automated backups, want none", len(got))
	}
	advance(t, s, clk, 2*time.Hour)
	got := backupRuns(t, s, "a", "AUTOMATED")
	if len(got) != 1 || got[0].Status != "SUCCESSFUL" || got[0].Description != "" {
		t.Fatalf("after startTime: %+v, want one SUCCESSFUL Automated backup", got)
	}
	ops := backupOps(t, s, "a")
	if len(ops) != 1 || ops[0].Status != "DONE" || ops[0].Error != nil {
		t.Fatalf("Operations: %+v, want one successful BACKUP_VOLUME", ops)
	}
	advance(t, s, clk, 10*time.Hour)
	if got := backupRuns(t, s, "a", "AUTOMATED"); len(got) != 1 {
		t.Errorf("later the same day: %d Automated backups, want 1", len(got))
	}
	if ops := backupOps(t, s, "a"); len(ops) != 1 {
		t.Errorf("later the same day: %d BACKUP_VOLUME Operations, want 1", len(ops))
	}
}

const day = 24 * time.Hour

func TestAutomatedBackupMakesUpMissedSlotsOnce(t *testing.T) {
	s, clk := autoService(t)
	putWithAutomatedBackups(t, s, clk, "a", "02:00", 7)

	advance(t, s, clk, 3*day+3*time.Hour)
	if got := backupRuns(t, s, "a", "AUTOMATED"); len(got) != 1 {
		t.Fatalf("after 3 missed days: %d Automated backups, want 1", len(got))
	}
	advance(t, s, clk, time.Hour)
	if got := backupRuns(t, s, "a", "AUTOMATED"); len(got) != 1 {
		t.Errorf("an hour later: %d Automated backups, want still 1", len(got))
	}
}

func TestDeletingAnAutomatedBackupDoesNotRetakeIt(t *testing.T) {
	s, clk := autoService(t)
	putWithAutomatedBackups(t, s, clk, "a", "02:00", 7)
	advance(t, s, clk, 3*time.Hour)
	runs := backupRuns(t, s, "a", "AUTOMATED")
	if len(runs) != 1 {
		t.Fatalf("%d Automated backups, want 1", len(runs))
	}
	code, body := call(s, "DELETE", "/projects/proj-one/instances/a/backupRuns/"+strconv.FormatInt(runs[0].Id, 10), "")
	if code != 200 {
		t.Fatalf("delete: %d %s", code, body)
	}
	waitOpDone(t, s, "proj-one", body)

	advance(t, s, clk, time.Hour)
	if got := backupRuns(t, s, "a", "AUTOMATED"); len(got) != 0 {
		t.Errorf("after deleting the day's backup: %d Automated backups, want none", len(got))
	}
}

func TestAutomatedBackupWaitsForAnotherOperation(t *testing.T) {
	s, clk := autoService(t)
	putWithAutomatedBackups(t, s, clk, "a", "02:00", 7)
	// An on-demand backup holds the instance until unblock.
	unblock, blocked := make(chan struct{}), make(chan struct{})
	var once sync.Once
	s.dump = func(_ context.Context, _, _ string, w io.Writer) error {
		once.Do(func() { close(blocked); <-unblock })
		_, err := io.WriteString(w, "-- dump")
		return err
	}
	code, body := call(s, "POST", "/projects/proj-one/instances/a/backupRuns", `{"description":"mine"}`)
	if code != 200 {
		t.Fatalf("backupRuns.insert: %d %s", code, body)
	}
	<-blocked

	advance(t, s, clk, 3*time.Hour)
	if got := backupRuns(t, s, "a", "AUTOMATED"); len(got) != 0 {
		t.Fatalf("during the on-demand backup: %d Automated backups, want none", len(got))
	}
	close(unblock)
	waitOpDone(t, s, "proj-one", body)
	advance(t, s, clk, time.Minute)
	if got := backupRuns(t, s, "a", "AUTOMATED"); len(got) != 1 || got[0].Status != "SUCCESSFUL" {
		t.Errorf("once free: %+v, want one SUCCESSFUL Automated backup", got)
	}
}

func TestFailedAutomatedBackupIsNotRetriedBeforeTheNextSlot(t *testing.T) {
	s, clk := autoService(t)
	putWithAutomatedBackups(t, s, clk, "a", "02:00", 7)
	s.dump = func(context.Context, string, string, io.Writer) error {
		return newOpError("ERROR_RDBMS", "pg_dumpall: disk full")
	}

	advance(t, s, clk, 3*time.Hour)
	got := backupRuns(t, s, "a", "AUTOMATED")
	if len(got) != 1 || got[0].Status != "FAILED" || got[0].Error == nil {
		t.Fatalf("after a failed attempt: %+v, want one FAILED Automated backup", got)
	}
	if ops := backupOps(t, s, "a"); len(ops) != 1 || ops[0].Error == nil {
		t.Fatalf("Operations: %+v, want one failed BACKUP_VOLUME", ops)
	}
	advance(t, s, clk, 10*time.Hour)
	if got := backupRuns(t, s, "a", "AUTOMATED"); len(got) != 1 {
		t.Errorf("later the same day: %d Automated backups, want 1", len(got))
	}
	advance(t, s, clk, 14*time.Hour)
	if got := backupRuns(t, s, "a", "AUTOMATED"); len(got) != 2 {
		t.Errorf("at the next slot: %d Automated backups, want 2", len(got))
	}
}

func TestNoAutomatedBackupWhileDisabledOrStopped(t *testing.T) {
	for _, c := range []struct {
		name    string
		off, on func(st *sqladmin.Settings)
	}{
		{"enabled false", func(st *sqladmin.Settings) { st.BackupConfiguration.Enabled = false },
			func(st *sqladmin.Settings) { st.BackupConfiguration.Enabled = true }},
		{"activation policy NEVER", func(st *sqladmin.Settings) { st.ActivationPolicy = "NEVER" },
			func(st *sqladmin.Settings) { st.ActivationPolicy = "ALWAYS" }},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, clk := autoService(t)
			putWithAutomatedBackups(t, s, clk, "a", "02:00", 7)
			set := func(fn func(st *sqladmin.Settings)) {
				if err := s.updateRecord("proj-one", "a", func(r *instanceRecord) error { fn(r.Instance.Settings); return nil }); err != nil {
					t.Fatal(err)
				}
			}
			set(c.off)
			advance(t, s, clk, 2*day+3*time.Hour)
			if got := backupRuns(t, s, "a", "AUTOMATED"); len(got) != 0 {
				t.Fatalf("while off: %d Automated backups, want none", len(got))
			}
			// Store-only: the record changes as a start would leave it,
			// without the patch that would also mark the slot handled.
			set(c.on)
			advance(t, s, clk, time.Minute)
			if got := backupRuns(t, s, "a", "AUTOMATED"); len(got) != 1 {
				t.Errorf("once on past missed slots: %d Automated backups, want 1", len(got))
			}
		})
	}
}

func TestNoAutomatedBackupWhileNotRunnable(t *testing.T) {
	s, clk := autoService(t)
	putWithAutomatedBackups(t, s, clk, "a", "02:00", 7)
	if err := s.setState("proj-one", "a", "MAINTENANCE"); err != nil {
		t.Fatal(err)
	}
	advance(t, s, clk, 3*time.Hour)
	if got := backupRuns(t, s, "a", "AUTOMATED"); len(got) != 0 {
		t.Fatalf("while recovering: %d Automated backups, want none", len(got))
	}
	if err := s.setState("proj-one", "a", "RUNNABLE"); err != nil {
		t.Fatal(err)
	}
	advance(t, s, clk, time.Minute)
	if got := backupRuns(t, s, "a", "AUTOMATED"); len(got) != 1 {
		t.Errorf("once RUNNABLE: %d Automated backups, want 1", len(got))
	}
}

func TestStateFromBeforeAutomatedBackupsTakesNoneOnUpgrade(t *testing.T) {
	s, clk := autoService(t)
	advance(t, s, clk, 3*time.Hour) // 03:00, past today's 02:00 slot
	putInstance(t, s, "proj-one", "a", false)
	if err := s.updateRecord("proj-one", "a", func(r *instanceRecord) error {
		r.Instance.Settings.BackupConfiguration.Enabled = true
		r.Instance.Settings.BackupConfiguration.StartTime = "02:00"
		r.BackupSlot = "" // as stored before #1038
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	advance(t, s, clk, time.Minute)
	if got := backupRuns(t, s, "a", "AUTOMATED"); len(got) != 0 {
		t.Fatalf("on upgrade: %d Automated backups, want none", len(got))
	}
	advance(t, s, clk, day)
	if got := backupRuns(t, s, "a", "AUTOMATED"); len(got) != 1 {
		t.Errorf("at the next startTime: %d Automated backups, want 1", len(got))
	}
}

func TestNewInstanceTakesItsFirstAutomatedBackupAtTheNextStartTime(t *testing.T) {
	s, clk := autoService(t)
	advance(t, s, clk, day-time.Minute) // 23:59
	// Created stopped, so no container is needed; a start then leaves it
	// running.
	code, body := call(s, "POST", "/projects/proj-one/instances", `{"name":"a","databaseVersion":"POSTGRES_18","settings":{"tier":"db-perf-optimized-N-2",`+
		`"activationPolicy":"NEVER","backupConfiguration":{"enabled":true,"startTime":"00:00"}}}`)
	if code != 200 {
		t.Fatalf("insert: %d %s", code, body)
	}
	if op := waitOpDone(t, s, "proj-one", body); op.Error != nil {
		t.Fatalf("insert: %+v", op.Error.Errors[0])
	}
	if err := s.updateRecord("proj-one", "a", func(r *instanceRecord) error {
		r.Instance.Settings.ActivationPolicy = "ALWAYS"
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if got := backupRuns(t, s, "a", "AUTOMATED"); len(got) != 0 {
		t.Fatalf("at creation: %d Automated backups, want none", len(got))
	}
	// No sweep runs before 00:00, so creation itself must have marked
	// 23:59's slot handled.
	advance(t, s, clk, 90*time.Second) // 00:00:30
	if got := backupRuns(t, s, "a", "AUTOMATED"); len(got) != 1 {
		t.Errorf("after 00:00: %d Automated backups, want 1", len(got))
	}
}

// patchSettings patches the store-only instance's settings with the JSON
// object fields and waits for the Operation.
func patchSettings(t *testing.T, s *Service, name, fields string) {
	t.Helper()
	code, body := call(s, "PATCH", "/projects/proj-one/instances/"+name, `{"settings":{"tier":"db-perf-optimized-N-2","ipConfiguration":{"ipv4Enabled":true},`+fields+`}}`)
	if code != 200 {
		t.Fatalf("patch %s: %d %s", fields, code, body)
	}
	if op := waitOpDone(t, s, "proj-one", body); op.Error != nil {
		t.Fatalf("patch %s: %+v", fields, op.Error.Errors[0])
	}
}

func TestChangingStartTimeToEarlierTodayWaitsForTomorrow(t *testing.T) {
	s, clk := autoService(t)
	putWithAutomatedBackups(t, s, clk, "a", "02:00", 7)
	advance(t, s, clk, 90*time.Minute) // 01:30

	patchSettings(t, s, "a", `"backupConfiguration":{"startTime":"01:00"}`)
	advance(t, s, clk, time.Hour) // 02:30, past the old start time too
	if got := backupRuns(t, s, "a", "AUTOMATED"); len(got) != 0 {
		t.Fatalf("today: %d Automated backups, want none", len(got))
	}
	advance(t, s, clk, day-time.Hour) // 01:30 tomorrow
	if got := backupRuns(t, s, "a", "AUTOMATED"); len(got) != 1 {
		t.Errorf("tomorrow after 01:00: %d Automated backups, want 1", len(got))
	}
}

func TestEnablingBackupsWaitsForTheNextStartTime(t *testing.T) {
	s, clk := autoService(t)
	putWithAutomatedBackups(t, s, clk, "a", "02:00", 7)
	if err := s.updateRecord("proj-one", "a", func(r *instanceRecord) error {
		r.Instance.Settings.BackupConfiguration.Enabled = false
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	advance(t, s, clk, 3*time.Hour)

	patchSettings(t, s, "a", `"backupConfiguration":{"enabled":true}`)
	advance(t, s, clk, time.Hour)
	if got := backupRuns(t, s, "a", "AUTOMATED"); len(got) != 0 {
		t.Fatalf("today: %d Automated backups, want none", len(got))
	}
	advance(t, s, clk, day)
	if got := backupRuns(t, s, "a", "AUTOMATED"); len(got) != 1 {
		t.Errorf("at the next start time: %d Automated backups, want 1", len(got))
	}
}

// automatedFiles returns the dump files of the instance's Automated
// backups, newest first.
func automatedFiles(t *testing.T, s *Service, name string) []string {
	t.Helper()
	var out []string
	for _, r := range backupRuns(t, s, name, "AUTOMATED") {
		b, ok := s.loadBackup("proj-one", name, strconv.FormatInt(r.Id, 10))
		if !ok {
			t.Fatalf("backup %d not stored", r.Id)
		}
		out = append(out, b.File)
	}
	return out
}

func TestAutomatedBackupsAreKeptToRetainedBackups(t *testing.T) {
	s, clk := autoService(t)
	putWithAutomatedBackups(t, s, clk, "a", "02:00", 2)
	ondemand := putBackupWithFile(t, s, "proj-one", &sqladmin.BackupRun{Id: 1, Instance: "a", Type: "ON_DEMAND", Status: "SUCCESSFUL"})

	advance(t, s, clk, 3*time.Hour)
	advance(t, s, clk, day)
	first := automatedFiles(t, s, "a")
	if len(first) != 2 {
		t.Fatalf("after two days: %d Automated backups, want 2", len(first))
	}
	advance(t, s, clk, day)
	got := automatedFiles(t, s, "a")
	if len(got) != 2 || got[1] != first[0] {
		t.Fatalf("after three days: %v, want the newest two (oldest of before %s gone)", got, first[1])
	}
	if _, err := os.Stat(first[1]); !os.IsNotExist(err) {
		t.Errorf("the oldest Automated backup's dump: %v, want it removed", err)
	}
	if got := backupRuns(t, s, "a", "ON_DEMAND"); len(got) != 1 {
		t.Errorf("on-demand backups: %d, want 1 untouched", len(got))
	}
	if _, err := os.Stat(ondemand); err != nil {
		t.Errorf("on-demand dump: %v", err)
	}
}

func TestRetentionDeletesFailedAutomatedBackupsOlderThanTheOldestKept(t *testing.T) {
	s, clk := autoService(t)
	putWithAutomatedBackups(t, s, clk, "a", "02:00", 1)
	ok := s.dump
	s.dump = func(context.Context, string, string, io.Writer) error {
		return newOpError("ERROR_RDBMS", "disk full")
	}
	advance(t, s, clk, 3*time.Hour)
	s.dump = ok
	advance(t, s, clk, day)
	got := backupRuns(t, s, "a", "AUTOMATED")
	if len(got) != 1 || got[0].Status != "SUCCESSFUL" {
		t.Errorf("Automated backups: %+v, want only the SUCCESSFUL one", got)
	}
}

func TestLoweringRetainedBackupsDeletesTheExcess(t *testing.T) {
	s, clk := autoService(t)
	putWithAutomatedBackups(t, s, clk, "a", "02:00", 7)
	advance(t, s, clk, 3*time.Hour)
	advance(t, s, clk, day)
	advance(t, s, clk, day)
	if got := backupRuns(t, s, "a", "AUTOMATED"); len(got) != 3 {
		t.Fatalf("%d Automated backups, want 3", len(got))
	}

	patchSettings(t, s, "a", `"backupConfiguration":{"backupRetentionSettings":{"retentionUnit":"COUNT","retainedBackups":1}}`)
	if got := backupRuns(t, s, "a", "AUTOMATED"); len(got) != 1 {
		t.Errorf("after lowering retainedBackups to 1: %d Automated backups, want 1", len(got))
	}
}

func TestDeletingAnInstanceDeletesItsAutomatedBackups(t *testing.T) {
	s, clk := autoService(t)
	putWithAutomatedBackups(t, s, clk, "a", "02:00", 7)
	advance(t, s, clk, 3*time.Hour)
	files := automatedFiles(t, s, "a")

	deleteAndWait(t, s, "proj-one", "a", "")
	if got := listIDs(t, s, "proj-one", "", ""); len(got) != 0 {
		t.Errorf("backups after delete: %v, want none", got)
	}
	if _, err := os.Stat(files[0]); !os.IsNotExist(err) {
		t.Errorf("dump after delete: %v, want it removed", err)
	}
}

func TestRetainedAutomatedBackupsExpireOneADay(t *testing.T) {
	s, clk := autoService(t)
	putWithAutomatedBackups(t, s, clk, "a", "02:00", 7)
	if err := s.updateRecord("proj-one", "a", func(r *instanceRecord) error {
		r.Instance.Settings.RetainBackupsOnDelete = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	putBackupWithFile(t, s, "proj-one", &sqladmin.BackupRun{Id: 1, Instance: "a", Type: "ON_DEMAND", Status: "SUCCESSFUL"})
	advance(t, s, clk, 3*time.Hour)
	advance(t, s, clk, day)
	advance(t, s, clk, day)
	deleted := clk.Now().UTC()
	deleteAndWait(t, s, "proj-one", "a", "")

	got := getBackups(t, s, "proj-one", `type="AUTOMATED"`)
	sort.Slice(got, func(i, j int) bool { return got[i].Name > got[j].Name }) // newest first
	if len(got) != 3 {
		t.Fatalf("Retained Automated backups: %d, want 3", len(got))
	}
	for i, b := range got {
		want := deleted.Add(time.Duration(7-i) * day)
		at, err := time.Parse(time.RFC3339Nano, b.ExpiryTime)
		if err != nil || !at.Equal(want) || b.InstanceDeletionTime == "" {
			t.Errorf("%s: expiryTime %q instanceDeletionTime %q, want expiry %s", b.Name, b.ExpiryTime, b.InstanceDeletionTime, want.Format(time.RFC3339Nano))
		}
	}
	// Only a Final backup's expiry can be changed.
	if code, body := call(s, "PATCH", "/"+got[0].Name+"?updateMask=ttlDays", `{"ttlDays":"300"}`); code != 400 {
		t.Errorf("update of a Retained Automated backup's ttlDays: %d %s, want 400", code, body)
	}

	advance(t, s, clk, 5*day)
	if got := listIDs(t, s, "proj-one", `type="AUTOMATED"`, ""); len(got) != 2 {
		t.Errorf("5 days after the delete: %v, want 2 Retained Automated backups left", got)
	}
	advance(t, s, clk, 2*day)
	if got := listIDs(t, s, "proj-one", `type="AUTOMATED"`, ""); len(got) != 0 {
		t.Errorf("7 days after the delete: %v, want none left", got)
	}
	if got := listIDs(t, s, "proj-one", `type="ON_DEMAND"`, ""); len(got) != 1 {
		t.Errorf("Retained on-demand backups: %v, want 1 that never expires", got)
	}
}

// TestAutomatedBackupTakesABaseBackupWithPITR covers the Base backup that
// lets transaction log retention drop WAL older than its anchor
// (planRetention, TestRetentionDropsWhatIsOlderThanTheAnchor; ADR 0022,
// so the fake clock moves no Recovery window here).
func TestAutomatedBackupTakesABaseBackupWithPITR(t *testing.T) {
	s, clk := autoService(t)
	putWithAutomatedBackups(t, s, clk, "a", "02:00", 7)
	var bases []string
	s.baseBackup = func(_ context.Context, project, name string) error {
		bases = append(bases, project+"/"+name)
		return nil
	}
	advance(t, s, clk, 3*time.Hour)
	if len(bases) != 0 {
		t.Fatalf("without point-in-time recovery: Base backups %v, want none", bases)
	}
	if err := s.updateRecord("proj-one", "a", func(r *instanceRecord) error {
		r.Instance.Settings.BackupConfiguration.PointInTimeRecoveryEnabled = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	advance(t, s, clk, day)
	if len(bases) != 1 || bases[0] != "proj-one/a" {
		t.Errorf("with point-in-time recovery: Base backups %v, want one of proj-one/a", bases)
	}
}

// TestBackupStartTimeIsValidated covers #1059: insert, patch and update
// reject a backupConfiguration.startTime that is not two-digit 24-hour
// HH:MM, which the Automated backup schedule could never run.
func TestBackupStartTimeIsValidated(t *testing.T) {
	s, _ := fileService(t)
	bad := []string{"7am", "24:00", "12:60", "7:00", "07:00:00", " 07:00", "07:00 "}
	good := []string{"00:00", "07:30", "23:59"}
	instance := func(name string) (int, *sqladmin.DatabaseInstance) {
		t.Helper()
		code, body := call(s, "GET", "/projects/proj-one/instances/"+name, "")
		var in sqladmin.DatabaseInstance
		if code == 200 {
			if err := json.Unmarshal([]byte(body), &in); err != nil {
				t.Fatal(err)
			}
		}
		return code, &in
	}
	instanceOps := func(name string) int {
		t.Helper()
		code, body := call(s, "GET", "/projects/proj-one/operations?instance="+name, "")
		if code != 200 {
			t.Fatalf("operations.list: %d %s", code, body)
		}
		var resp sqladmin.OperationsListResponse
		if err := json.Unmarshal([]byte(body), &resp); err != nil {
			t.Fatal(err)
		}
		return len(resp.Items)
	}
	insert := func(name, backup string) (int, string) {
		return call(s, "POST", "/projects/proj-one/instances",
			`{"name":"`+name+`","databaseVersion":"POSTGRES_18","settings":{"tier":"db-perf-optimized-N-2"`+backup+`}}`)
	}
	withStart := func(start string) string {
		return `,"backupConfiguration":{"enabled":true,"startTime":` + strconv.Quote(start) + `}`
	}

	for i, start := range bad {
		name := "bad-" + strconv.Itoa(i)
		code, body := insert(name, withStart(start))
		if code != 400 || !strings.Contains(body, "backupConfiguration.startTime") {
			t.Errorf("insert with startTime %q: %d %s, want 400 naming the field", start, code, body)
		}
		if code, _ := instance(name); code != 404 {
			t.Errorf("insert with startTime %q: instance GET %d, want 404", start, code)
		}
		if n := instanceOps(name); n != 0 {
			t.Errorf("insert with startTime %q: %d Operations, want none", start, n)
		}
	}
	for i, start := range good {
		name := "good-" + strconv.Itoa(i)
		if code, body := insert(name, withStart(start)); code != 200 {
			t.Errorf("insert with startTime %q: %d %s", start, code, body)
			continue
		}
		if _, in := instance(name); in.Settings.BackupConfiguration.StartTime != start {
			t.Errorf("insert with startTime %q: stored %q", start, in.Settings.BackupConfiguration.StartTime)
		}
	}
	if code, body := insert("omitted", ""); code != 200 {
		t.Fatalf("insert without startTime: %d %s", code, body)
	}
	if _, in := instance("omitted"); in.Settings.BackupConfiguration.StartTime != "00:00" {
		t.Errorf("insert without startTime: stored %q, want 00:00", in.Settings.BackupConfiguration.StartTime)
	}

	// Patch and update need the instance created; a store-only one stands in.
	putInstance(t, s, "proj-one", "stored", false)
	if err := s.updateRecord("proj-one", "stored", func(r *instanceRecord) error {
		applySettingsDefaults(r.Instance.Settings, nil, "", r.Instance.DatabaseVersion, true)
		r.Instance.Settings.Tier = "db-perf-optimized-N-2"
		r.Instance.Settings.IpConfiguration.Ipv4Enabled = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{"PATCH", "PUT"} {
		for _, start := range bad {
			_, before := instance("stored")
			code, body := call(s, method, "/projects/proj-one/instances/stored",
				`{"settings":{"tier":"db-perf-optimized-N-2","ipConfiguration":{"ipv4Enabled":true}`+withStart(start)+`}}`)
			if code != 400 || !strings.Contains(body, "backupConfiguration.startTime") {
				t.Errorf("%s with startTime %q: %d %s, want 400 naming the field", method, start, code, body)
			}
			_, after := instance("stored")
			if a, b := after.Settings, before.Settings; a.SettingsVersion != b.SettingsVersion ||
				a.BackupConfiguration.StartTime != b.BackupConfiguration.StartTime {
				t.Errorf("%s with startTime %q: settings changed to version %d startTime %q, want %d %q",
					method, start, a.SettingsVersion, a.BackupConfiguration.StartTime, b.SettingsVersion, b.BackupConfiguration.StartTime)
			}
		}
		for _, start := range good {
			code, body := call(s, method, "/projects/proj-one/instances/stored",
				`{"settings":{"tier":"db-perf-optimized-N-2","ipConfiguration":{"ipv4Enabled":true}`+withStart(start)+`}}`)
			if code != 200 {
				t.Fatalf("%s with startTime %q: %d %s", method, start, code, body)
			}
			if op := waitOpDone(t, s, "proj-one", body); op.Error != nil {
				t.Fatalf("%s with startTime %q: %+v", method, start, op.Error.Errors[0])
			}
			if _, in := instance("stored"); in.Settings.BackupConfiguration.StartTime != start {
				t.Errorf("%s with startTime %q: stored %q", method, start, in.Settings.BackupConfiguration.StartTime)
			}
		}
	}
}

// TestStoredBadStartTimeFailsPatchesUntilFixed covers #1059's rule that a
// patch is checked against the merged settings: an instance stored with a
// startTime the schedule can't read fails every patch that leaves it, which
// at last explains why it took no Automated backups.
func TestStoredBadStartTimeFailsPatchesUntilFixed(t *testing.T) {
	s, _ := fileService(t)
	putInstance(t, s, "proj-one", "a", false)
	if err := s.updateRecord("proj-one", "a", func(r *instanceRecord) error {
		applySettingsDefaults(r.Instance.Settings, nil, "", r.Instance.DatabaseVersion, true)
		r.Instance.Settings.Tier = "db-perf-optimized-N-2"
		r.Instance.Settings.IpConfiguration.Ipv4Enabled = true
		r.Instance.Settings.BackupConfiguration.StartTime = "7am"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	path := "/projects/proj-one/instances/a"
	if code, body := call(s, "PATCH", path, `{"settings":{"userLabels":{"k":"v"}}}`); code != 400 || !strings.Contains(body, "backupConfiguration.startTime (7am)") {
		t.Errorf("patch leaving startTime 7am: %d %s, want 400 naming it", code, body)
	}
	code, body := call(s, "PATCH", path, `{"settings":{"backupConfiguration":{"startTime":"07:00"}}}`)
	if code != 200 {
		t.Fatalf("patch fixing startTime: %d %s", code, body)
	}
	if op := waitOpDone(t, s, "proj-one", body); op.Error != nil {
		t.Fatalf("patch fixing startTime: %+v", op.Error.Errors[0])
	}
}
