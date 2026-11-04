package sql

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	sqladmin "google.golang.org/api/sqladmin/v1beta4"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/store"
)

const (
	lockProject  = "proj-one"
	lockInstance = "main"
	lockPath     = "/projects/" + lockProject + "/instances/" + lockInstance
)

// lockedService returns a service holding a store-only Cloud SQL instance
// with a database, a user, a client certificate and a backup, whose
// Operations take latency to complete.
func lockedService(t *testing.T, latency string) (*Service, int64) {
	t.Helper()
	s := testService(t, config.IAMOff)
	s.env.Config.LROLatency["sql"] = latency
	putInstance(t, s, lockProject, lockInstance, false)
	var id int64
	if err := s.updateRecord(lockProject, lockInstance, func(r *instanceRecord) error {
		r.Instance.Settings.IpConfiguration = &sqladmin.IpConfiguration{PrivateNetwork: "projects/" + lockProject + "/global/networks/default"}
		r.PrivateIP = "10.9.0.3"
		return initPKI(r, lockProject, lockInstance)
	}); err != nil {
		t.Fatal(err)
	}
	rec, err := s.loadRecord(lockProject, lockInstance)
	if err != nil {
		t.Fatal(err)
	}
	b := &backupRecord{Run: s.newBackupRun(rec, "ON_DEMAND", "kept")}
	b.Run.Status = "SUCCESSFUL"
	if err := s.storeNewBackup(lockProject, b); err != nil {
		t.Fatal(err)
	}
	id = b.Run.Id
	if err := s.env.Store.Update(func(tx store.Tx) error {
		if err := store.PutJSON(tx, nsDatabases, childKey(lockProject, lockInstance, "postgres"), newDatabase(lockProject, lockInstance, "postgres", "", "")); err != nil {
			return err
		}
		if err := store.PutJSON(tx, nsUsers, childKey(lockProject, lockInstance, "kept"), &userRecord{User: newUser(lockProject, lockInstance, "kept", "BUILT_IN")}); err != nil {
			return err
		}
		return store.PutJSON(tx, nsSSLCerts, childKey(lockProject, lockInstance, "fp"), &sqladmin.SslCert{Sha1Fingerprint: "fp"})
	}); err != nil {
		t.Fatal(err)
	}
	return s, id
}

// runningOp starts an Operation that needs no runtime and runs for the
// service's latency.
func runningOp(t *testing.T, s *Service) {
	t.Helper()
	if code, body := call(s, "POST", lockPath+"/sslCerts", `{"commonName":"first"}`); code != 200 {
		t.Fatalf("first operation: %d %s", code, body)
	}
}

func opCount(t *testing.T, s *Service) int {
	t.Helper()
	code, body := call(s, "GET", "/projects/"+lockProject+"/operations", "")
	if code != 200 {
		t.Fatalf("list operations: %d %s", code, body)
	}
	var l sqladmin.OperationsListResponse
	if err := json.Unmarshal([]byte(body), &l); err != nil {
		t.Fatal(err)
	}
	return len(l.Items)
}

// While an Operation runs on a Cloud SQL instance, a request that would
// start any other Operation on it fails with 409 and changes nothing.
func TestOperationLockRejectsEveryOperation(t *testing.T) {
	type request struct{ name, method, path, body string }
	requests := func(id int64) []request {
		return []request{
			{"patch", "PATCH", lockPath, `{"settings":{"userLabels":{"changed":"yes"}}}`},
			{"delete", "DELETE", lockPath, ""},
			{"restart", "POST", lockPath + "/restart", ""},
			{"export", "POST", lockPath + "/export", `{"exportContext":{"uri":"gs://b/o.sql","databases":["postgres"]}}`},
			{"import", "POST", lockPath + "/import", `{"importContext":{"uri":"gs://b/o.sql","database":"postgres","fileType":"SQL"}}`},
			{"restoreBackup", "POST", lockPath + "/restoreBackup", fmt.Sprintf(`{"restoreBackupContext":{"backupRunId":"%d"}}`, id)},
			{"resetSslConfig", "POST", lockPath + "/resetSslConfig", ""},
			{"insertSslCert", "POST", lockPath + "/sslCerts", `{"commonName":"second"}`},
			{"deleteSslCert", "DELETE", lockPath + "/sslCerts/fp", ""},
			{"insertDatabase", "POST", lockPath + "/databases", `{"name":"second"}`},
			{"patchDatabase", "PATCH", lockPath + "/databases/postgres", `{}`},
			{"deleteDatabase", "DELETE", lockPath + "/databases/postgres", ""},
			{"insertUser", "POST", lockPath + "/users", `{"name":"second","password":"p"}`},
			{"updateUser", "PUT", lockPath + "/users?name=kept", `{"password":"p"}`},
			{"deleteUser", "DELETE", lockPath + "/users?name=kept", ""},
			{"insertBackupRun", "POST", lockPath + "/backupRuns", `{}`},
			{"deleteBackupRun", "DELETE", fmt.Sprintf("%s/backupRuns/%d", lockPath, id), ""},
			{"createBackup", "POST", "/projects/" + lockProject + "/backups", `{"instance":"` + lockInstance + `"}`},
			{"updateBackup", "PATCH", fmt.Sprintf("/projects/%s/backups/%d?updateMask=description", lockProject, id), `{"description":"changed"}`},
			{"deleteBackup", "DELETE", fmt.Sprintf("/projects/%s/backups/%d", lockProject, id), ""},
		}
	}
	_, id := lockedService(t, "0s")
	for _, c := range requests(id) {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			s, id := lockedService(t, "2s")
			for _, r := range requests(id) {
				if r.name == c.name {
					c = r
				}
			}
			before := snapshotOf(t, s)
			runningOp(t, s)
			code, body := call(s, c.method, c.path, c.body)
			if code != 409 || !strings.Contains(body, "operationInProgress") {
				t.Fatalf("got %d %s, want 409 operationInProgress", code, body)
			}
			if n := opCount(t, s); n != 1 {
				t.Errorf("the rejected request left %d Operations, want 1", n)
			}
			if after := snapshotOf(t, s); after != before {
				t.Errorf("the rejected request changed the instance:\n before %s\n after  %s", before, after)
			}
		})
	}
}

// snapshotOf is what a client can read of the Cloud SQL instance, its
// users, databases and backups.
func snapshotOf(t *testing.T, s *Service) string {
	t.Helper()
	var out strings.Builder
	for _, p := range []string{lockPath, lockPath + "/users", lockPath + "/databases", lockPath + "/backupRuns"} {
		code, body := call(s, "GET", p, "")
		if code != 200 {
			t.Fatalf("GET %s: %d %s", p, code, body)
		}
		out.WriteString(body)
	}
	return out.String()
}

// denyAll refuses every permission.
type denyAll struct{ emu.Authorizer }

func (denyAll) Check(_ context.Context, permission, resource string) error {
	return apierr.PermissionDenied("Permission '%s' denied on resource '%s'.", permission, resource)
}

// doneGapStore holds back the writer that first stored a DONE Operation
// until gap closes, so a test can act in the moment a client sees DONE.
type doneGapStore struct {
	store.Store
	s    *Service
	held *atomic.Bool
	gap  chan struct{}
}

func (d doneGapStore) Update(fn func(store.Tx) error) error {
	err := d.Store.Update(fn)
	if anyOpDone(d.s) && d.held.CompareAndSwap(false, true) {
		<-d.gap
	}
	return err
}

// anyOpDone reports whether an Operation shows DONE.
func anyOpDone(s *Service) bool {
	return slices.ContainsFunc(s.Operations(), func(op emu.OperationInfo) bool { return op.Done })
}

// Once an Operation shows DONE its Operation lock is free, so a client
// that polls and goes straight on is not told it is still running.
func TestOperationDoneFreesLock(t *testing.T) {
	for _, latency := range []string{"0s", "1ms"} {
		t.Run(latency, func(t *testing.T) {
			s, _ := lockedService(t, latency)
			gap := make(chan struct{})
			s.env.Store = doneGapStore{Store: s.env.Store, s: s, held: &atomic.Bool{}, gap: gap}
			first := make(chan int, 1)
			go func() {
				code, _ := call(s, "POST", lockPath+"/sslCerts", `{"commonName":"first"}`)
				first <- code
			}()
			for deadline := time.Now().Add(10 * time.Second); !anyOpDone(s); {
				if time.Now().After(deadline) {
					close(gap)
					t.Fatal("no Operation finished")
				}
				time.Sleep(time.Millisecond)
			}
			code, body := call(s, "POST", lockPath+"/sslCerts", `{"commonName":"second"}`)
			close(gap)
			if code != 200 {
				t.Errorf("next Operation after DONE: %d %s", code, body)
			}
			if code := <-first; code != 200 {
				t.Errorf("first Operation: %d", code)
			}
		})
	}
}

// A bad request on a busy Cloud SQL instance gets its own error: only a
// request that could otherwise start an Operation is told one is running.
func TestOperationLockBadRequestKeepsItsError(t *testing.T) {
	tests := []struct {
		name, method, path, body string
		want                     int
	}{
		{"unknown user", "DELETE", lockPath + "/users?name=nobody", "", 404},
		{"unknown database", "DELETE", lockPath + "/databases/nobody", "", 404},
		{"unknown certificate", "DELETE", lockPath + "/sslCerts/nobody", "", 404},
		{"invalid user", "POST", lockPath + "/users", `{"name":""}`, 400},
		{"missing common name", "POST", lockPath + "/sslCerts", `{}`, 400},
		{"invalid patch", "PATCH", lockPath, `{"name":"renamed"}`, 400},
		{"protected delete", "DELETE", lockPath, "", 400},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, _ := lockedService(t, "2s")
			if err := s.updateRecord(lockProject, lockInstance, func(r *instanceRecord) error {
				r.Instance.Settings.DeletionProtectionEnabled = true
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			runningOp(t, s)
			if code, body := call(s, tt.method, tt.path, tt.body); code != tt.want {
				t.Errorf("got %d %s, want %d", code, body, tt.want)
			}
		})
	}
	t.Run("denied", func(t *testing.T) {
		s := testService(t, config.IAMOff)
		s.env.Auth = denyAll{s.env.Auth}
		putInstance(t, s, lockProject, lockInstance, false)
		if err := s.acquire(context.Background(), instKey(lockProject, lockInstance), "running"); err != nil {
			t.Fatal(err)
		}
		if code, body := call(s, "POST", lockPath+"/restart", ""); code != 403 {
			t.Errorf("got %d %s, want 403", code, body)
		}
	})
}

// Work that isn't an Operation a client can see is waited out, as a
// State snapshot save is.
func TestOperationLockWaitsOutHiddenWork(t *testing.T) {
	s, _ := lockedService(t, "0s")
	start := time.Now()
	work := s.lock(lockProject, lockInstance)
	time.AfterFunc(200*time.Millisecond, work.Unlock)
	if code, body := call(s, "POST", lockPath+"/sslCerts", `{"commonName":"second"}`); code != 200 {
		t.Fatalf("got %d %s, want 200", code, body)
	}
	if d := time.Since(start); d < 150*time.Millisecond {
		t.Errorf("the request returned after %v, before the work ended", d)
	}
}

// The wait is bounded, and its error does not claim an Operation is
// running, as the client could not have seen one.
func TestOperationLockHiddenWorkTimesOut(t *testing.T) {
	defer func(d time.Duration) { hiddenWait = d }(hiddenWait)
	hiddenWait = 50 * time.Millisecond
	s, _ := lockedService(t, "0s")
	work := s.lock(lockProject, lockInstance)
	defer work.Unlock()
	code, body := call(s, "POST", lockPath+"/sslCerts", `{"commonName":"second"}`)
	if code != 503 || !strings.Contains(body, "retry shortly") || strings.Contains(body, "operationInProgress") {
		t.Fatalf("got %d %s, want 503 retry shortly", code, body)
	}
	if n := opCount(t, s); n != 0 {
		t.Errorf("the rejected request left %d Operations", n)
	}
}

// A request that fails after claiming the lock gives it back.
func TestOperationLockFailedPatchFreesLock(t *testing.T) {
	s, _ := lockedService(t, "0s")
	if err := s.updateRecord(lockProject, lockInstance, func(r *instanceRecord) error {
		r.PrivateIP = ""
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	code, body := call(s, "PATCH", lockPath, `{"settings":{"userLabels":{"changed":"yes"}}}`)
	if code == 200 {
		t.Fatalf("patch without a private services connection succeeded: %s", body)
	}
	if code, body := call(s, "POST", lockPath+"/sslCerts", `{"commonName":"next"}`); code != 200 {
		t.Errorf("next Operation: %d %s", code, body)
	}
}

// A Retained backup outlives its Cloud SQL instance, so an Operation on it
// has no instance to wait for, even if a new one reuses the name and is
// running an Operation.
func TestOperationLockSkipsBackupOfDeletedInstance(t *testing.T) {
	s, id := lockedService(t, "2s")
	if err := s.updateBackupRecord(lockProject, lockInstance, id, func(b *backupRecord) {
		b.InstanceDeletionTime = "2026-11-01T00:00:00Z"
	}); err != nil {
		t.Fatal(err)
	}
	runningOp(t, s)
	backup := fmt.Sprintf("/projects/%s/backups/%d", lockProject, id)
	if code, body := call(s, "PATCH", backup+"?updateMask=description", `{"description":"changed"}`); code != 200 {
		t.Errorf("update: %d %s", code, body)
	}
	if code, body := call(s, "DELETE", backup, ""); code != 200 {
		t.Errorf("delete: %d %s", code, body)
	}
}

// A new Cloud SQL instance can reuse the name of a deleted one whose
// Operation has not released the lock yet, as Seed relies on: creating an
// instance never fails because of the lock.
func TestOperationLockCreateReusesName(t *testing.T) {
	s, _ := fileService(t)
	key := instKey(lockProject, lockInstance)
	if err := s.acquire(context.Background(), key, "deleting"); err != nil {
		t.Fatal(err)
	}
	code, body := call(s, "POST", "/projects/"+lockProject+"/instances",
		`{"name":"`+lockInstance+`","databaseVersion":"POSTGRES_18","settings":{"tier":"db-perf-optimized-N-2"}}`)
	if code != 200 {
		t.Fatalf("got %d %s, want 200", code, body)
	}
	waitOpDone(t, s, lockProject, body)
	s.releaseOp(&sqladmin.Operation{TargetProject: lockProject, TargetId: lockInstance, Name: "deleting"})
}
