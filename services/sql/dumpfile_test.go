package sql

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	sqladmin "google.golang.org/api/sqladmin/v1beta4"
)

// Backup dumps are written gzipped as <id>.sql.gz, streamed to a temporary
// file and renamed into place (#1043).

const testDump = "-- dump\nCREATE TABLE t ();\n"

// streamService is a fileService whose backups dump testDump.
func streamService(t *testing.T) *Service {
	t.Helper()
	s, _ := fileService(t)
	s.dump = func(_ context.Context, _, _ string, w io.Writer) error {
		_, err := io.WriteString(w, testDump)
		return err
	}
	return s
}

// gunzipFile returns the decompressed content of a gzipped file.
func gunzipFile(t *testing.T, file string) string {
	t.Helper()
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) < 2 || b[0] != 0x1f || b[1] != 0x8b {
		t.Fatalf("%s does not start with the gzip magic bytes: % x", file, b[:min(len(b), 2)])
	}
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// onDemandBackup takes an on-demand backup of the instance and returns its
// Operation once done.
func onDemandBackup(t *testing.T, s *Service, name string) *sqladmin.Operation {
	t.Helper()
	code, body := call(s, "POST", "/projects/proj-one/instances/"+name+"/backupRuns", `{}`)
	if code != 200 {
		t.Fatalf("backupRuns.insert: %d %s", code, body)
	}
	return waitOpDone(t, s, "proj-one", body)
}

func TestOnDemandBackupIsGzipped(t *testing.T) {
	s := streamService(t)
	putInstance(t, s, "proj-one", "a", false)
	op := onDemandBackup(t, s, "a")
	if op.Error != nil {
		t.Fatalf("backup failed: %+v", op.Error.Errors[0])
	}
	b, ok := s.loadBackup("proj-one", "a", strconv.FormatInt(op.BackupContext.BackupId, 10))
	if !ok {
		t.Fatal("backup not stored")
	}
	if want := strconv.FormatInt(op.BackupContext.BackupId, 10) + ".sql.gz"; filepath.Base(b.File) != want {
		t.Errorf("backup file %s, want %s", filepath.Base(b.File), want)
	}
	if got := gunzipFile(t, b.File); got != testDump {
		t.Errorf("decompressed backup = %q, want %q", got, testDump)
	}
}

func TestAutomatedBackupIsGzipped(t *testing.T) {
	s, clk := autoService(t)
	putWithAutomatedBackups(t, s, clk, "a", "02:00", 7)
	advance(t, s, clk, 3*time.Hour)
	files := automatedFiles(t, s, "a")
	if len(files) != 1 {
		t.Fatalf("Automated backups: %v, want 1", files)
	}
	if !strings.HasSuffix(files[0], ".sql.gz") {
		t.Errorf("Automated backup file %s, want <id>.sql.gz", files[0])
	}
	if got := gunzipFile(t, files[0]); got != "-- dump" {
		t.Errorf("decompressed backup = %q, want %q", got, "-- dump")
	}
}

// A pg_dumpall that fails part way leaves the backup FAILED with its error
// and no dump, finished or temporary, in the backup directory.
func TestFailedDumpLeavesNoFile(t *testing.T) {
	s := streamService(t)
	s.dump = func(_ context.Context, _, _ string, w io.Writer) error {
		if _, err := io.WriteString(w, "-- part of a dump\n"); err != nil {
			return err
		}
		return newOpError("ERROR_RDBMS", "pg_dumpall: error: query failed")
	}
	putInstance(t, s, "proj-one", "a", false)
	op := onDemandBackup(t, s, "a")
	runs := backupRuns(t, s, "a", "ON_DEMAND")
	if len(runs) != 1 || runs[0].Status != "FAILED" || runs[0].Error == nil || !strings.Contains(runs[0].Error.Message, "pg_dumpall: error: query failed") {
		r, _ := json.Marshal(runs)
		t.Fatalf("backup runs %s, want one FAILED with pg_dumpall's error", r)
	}
	if op.Error == nil {
		t.Error("BACKUP_VOLUME Operation succeeded, want it failed")
	}
	dir, err := s.backupDir("proj-one", "a")
	if err != nil {
		t.Fatal(err)
	}
	if left, _ := os.ReadDir(dir); len(left) != 0 {
		t.Errorf("backup directory holds %v, want nothing", left)
	}
}

// A backup from before #1043 has a plain <id>.sql dump; when it becomes a
// Retained backup its file moves to its new owner's directory under the
// same name, not renamed to .sql.gz.
func TestRetainedPlainBackupKeepsItsName(t *testing.T) {
	s, _ := fileService(t)
	putRetainingInstance(t, s, "proj-one", "a", true)
	dir, err := s.backupDir("proj-one", "a")
	if err != nil {
		t.Fatal(err)
	}
	plain := filepath.Join(dir, "1.sql")
	if err := os.WriteFile(plain, []byte(testDump), 0o600); err != nil {
		t.Fatal(err)
	}
	putBackup(t, s, "proj-one", &backupRecord{Run: &sqladmin.BackupRun{Id: 1, Instance: "a", Status: "SUCCESSFUL"}, File: plain})

	deleteAndWait(t, s, "proj-one", "a", "")

	file := storedFile(t, s, "proj-one", 1)
	if filepath.Base(file) != "1.sql" || file == plain {
		t.Errorf("Retained backup's file %s, want 1.sql moved out of %s", file, dir)
	}
	if b, _ := os.ReadFile(file); string(b) != testDump {
		t.Errorf("Retained backup's dump = %q, want the plain dump unchanged", b)
	}
}

// openDump reads gzipped and plain dumps alike, by their first bytes
// whatever the file's name.
func TestOpenDumpReadsGzippedAndPlainDumps(t *testing.T) {
	dir := t.TempDir()
	gz := filepath.Join(dir, "1.sql.gz")
	if err := writeDumpFile(gz, func(w io.Writer) error { _, err := io.WriteString(w, testDump); return err }); err != nil {
		t.Fatal(err)
	}
	plainNamedGz := filepath.Join(dir, "2.sql.gz")
	if err := os.WriteFile(plainNamedGz, []byte(testDump), 0o600); err != nil {
		t.Fatal(err)
	}
	empty := filepath.Join(dir, "3.sql")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for file, want := range map[string]string{gz: testDump, plainNamedGz: testDump, empty: ""} {
		r, err := openDump(file)
		if err != nil {
			t.Fatalf("openDump(%s): %v", filepath.Base(file), err)
		}
		got, err := io.ReadAll(r)
		r.Close()
		if err != nil || string(got) != want {
			t.Errorf("openDump(%s) reads %q, %v; want %q", filepath.Base(file), got, err, want)
		}
	}
}
