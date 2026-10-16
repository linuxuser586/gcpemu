package proxy

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// tgz builds a gzipped tar of name → content.
func tgz(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for name, data := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestPrepareRestore(t *testing.T) {
	dir, root := t.TempDir(), t.TempDir()
	pgdata := filepath.Join(root, "18", "docker") // PostgreSQL 18's layout

	// No bundle: a normal start.
	if ok, err := prepareRestore(pgdata, dir); ok || err != nil {
		t.Fatalf("without a bundle: %v %v", ok, err)
	}

	base := tgz(t, map[string][]byte{
		"PG_VERSION":           []byte("18\n"),
		"backup_label":         []byte("START WAL LOCATION: 0/2000028\n"),
		"global/pg_control":    []byte("control"),
		"postgresql.auto.conf": []byte("archive_mode = 'on'\nwork_mem = '64MB'\n"),
	})
	bundle := tgz(t, map[string][]byte{
		"base/1760000000000000/base.tar.gz": base,
		"wal/000000010000000000000002":      []byte("segment"),
	})
	if err := os.WriteFile(filepath.Join(dir, RestoreBundle), bundle, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, RestoreTarget), []byte("2026-10-16 10:00:00.123456+00"), 0o600); err != nil {
		t.Fatal(err)
	}

	ok, err := prepareRestore(pgdata, dir)
	if !ok || err != nil {
		t.Fatalf("with a bundle: %v %v", ok, err)
	}
	if b, err := os.ReadFile(filepath.Join(pgdata, "global/pg_control")); err != nil || string(b) != "control" {
		t.Fatalf("base backup not laid out in PGDATA: %q %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(pgdata, "recovery.signal")); err != nil {
		t.Fatalf("recovery.signal: %v", err)
	}
	// The agent runs in a Linux container; Windows has no permission bits
	// to check.
	if runtime.GOOS != "windows" {
		if fi, err := os.Stat(pgdata); err != nil || fi.Mode().Perm() != 0o700 {
			t.Fatalf("PGDATA mode: %v %v", fi.Mode(), err)
		}
		// The server user must be able to reach PGDATA through its parents.
		if fi, err := os.Stat(filepath.Dir(pgdata)); err != nil || fi.Mode().Perm()&0o005 != 0o005 {
			t.Fatalf("PGDATA parent mode: %v %v", fi.Mode(), err)
		}
	}
	conf, _ := os.ReadFile(filepath.Join(pgdata, "postgresql.auto.conf"))
	want := []string{
		"restore_command = 'cp " + filepath.Join(dir, "restore", "wal") + "/%f \"%p\"'",
		"recovery_target_time = '2026-10-16 10:00:00.123456+00'",
		"recovery_target_action = 'promote'",
	}
	for _, w := range want {
		if !strings.Contains(string(conf), w) {
			t.Errorf("postgresql.auto.conf lacks %q:\n%s", w, conf)
		}
	}
	// The source's ALTER SYSTEM settings are dropped; the emulator applies
	// the clone's own once recovery is done.
	if strings.Contains(string(conf), "archive_mode") || strings.Contains(string(conf), "work_mem") {
		t.Errorf("source settings kept:\n%s", conf)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "restore", "wal", "000000010000000000000002")); err != nil || string(b) != "segment" {
		t.Fatalf("WAL not unpacked: %q %v", b, err)
	}

	// A later start of the same container finds the cluster in place.
	if ok, err := prepareRestore(pgdata, dir); ok || err != nil {
		t.Fatalf("second start: %v %v", ok, err)
	}
}

func TestPrepareRestoreRejectsEscapingPaths(t *testing.T) {
	dir, pgdata := t.TempDir(), filepath.Join(t.TempDir(), "pgdata")
	bundle := tgz(t, map[string][]byte{"../escape": []byte("x")})
	if err := os.WriteFile(filepath.Join(dir, RestoreBundle), bundle, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareRestore(pgdata, dir); err == nil {
		t.Fatal("a bundle entry outside the restore directory was accepted")
	}
	if _, err := os.Stat(filepath.Join(dir, "..", "escape")); err == nil {
		t.Fatal("file written outside the restore directory")
	}
}
