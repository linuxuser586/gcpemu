package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// runLogs runs `gcpemu logs args...` on the data directory dir.
func runLogs(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()
	cmd := New(nil)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs(append([]string{"logs", "--data-dir", dir, "--config", filepath.Join(dir, "none.yaml")}, args...))
	err := cmd.Execute()
	return out.String(), err
}

func writeLogs(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLogsPrintsOldFilesOldestFirst(t *testing.T) {
	dir := t.TempDir()
	writeLogs(t, dir, map[string]string{
		"gcpemu.log.3": "three\n",
		"gcpemu.log.1": "one\n",
		"gcpemu.log":   "current\n",
	})
	out, err := runLogs(t, dir)
	if err != nil {
		t.Fatal(err)
	}
	if want := "three\none\ncurrent\n"; out != want {
		t.Errorf("logs = %q, want %q", out, want)
	}
}

func TestLogsFiltersByServiceInBothFormats(t *testing.T) {
	dir := t.TempDir()
	writeLogs(t, dir, map[string]string{
		"gcpemu.log.2": "level=INFO msg=a service=gcs\nlevel=INFO msg=b service=pubsub\n",
		"gcpemu.log":   `{"msg":"c","service":"gcs"}` + "\n" + `{"msg":"d","service":"sql"}` + "\n",
	})
	out, err := runLogs(t, dir, "gcs")
	if err != nil {
		t.Fatal(err)
	}
	if want := "level=INFO msg=a service=gcs\n" + `{"msg":"c","service":"gcs"}` + "\n"; out != want {
		t.Errorf("logs gcs = %q, want %q", out, want)
	}
}

func TestLogsPrintsALongLineIntact(t *testing.T) {
	dir := t.TempDir()
	long := "level=INFO service=gcs msg=" + strings.Repeat("y", 200<<10) + "\n"
	writeLogs(t, dir, map[string]string{"gcpemu.log.1": long, "gcpemu.log": "last"})
	out, err := runLogs(t, dir, "gcs")
	if err != nil {
		t.Fatal(err)
	}
	if out != long {
		t.Errorf("long line not printed intact: got %d bytes, want %d", len(out), len(long))
	}
	if out, _ := runLogs(t, dir); !strings.HasSuffix(out, "last") {
		t.Errorf("unterminated last line not printed: %q", out[len(out)-10:])
	}
}

func TestLogsWithoutLogFilesFails(t *testing.T) {
	dir := t.TempDir()
	_, err := runLogs(t, dir)
	if !errors.Is(err, os.ErrNotExist) || !strings.Contains(err.Error(), filepath.Join(dir, "gcpemu.log")) {
		t.Errorf("logs = %v, want the missing gcpemu.log error", err)
	}
}

func TestLogTailAfterRotationReadsFromTheStart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gcpemu.log")
	writeLogs(t, filepath.Dir(path), map[string]string{"gcpemu.log": "level=ERROR msg=\"not ready\"\nError: boom\n"})
	got := logTail(path, 1<<20, 20)
	if want := "  level=ERROR msg=\"not ready\"\n  Error: boom"; got != want {
		t.Errorf("logTail = %q, want %q", got, want)
	}
}
