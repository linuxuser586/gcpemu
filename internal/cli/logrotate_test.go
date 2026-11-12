package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/instance"
)

// appendLog opens path as the detached child's stdout is opened: append
// only, so each write lands at the end of the file, wherever that is.
func appendLog(t *testing.T, path string) *os.File {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestRotateLogKeepsThreeOldFiles(t *testing.T) {
	const limit, growth = 1024, 300
	path := filepath.Join(t.TempDir(), "gcpemu.log")
	w := appendLog(t, path)
	log := slog.New(slog.NewTextHandler(w, nil))
	var last string
	for round := 0; round < 6; round++ {
		for i := 0; i < 4; i++ {
			last = fmt.Sprintf("round %d line %d %s\n", round, i, strings.Repeat("x", growth-25))
			if _, err := w.WriteString(last); err != nil {
				t.Fatal(err)
			}
		}
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Size() < limit {
			t.Fatalf("round %d: log is %d bytes, want at least %d", round, fi.Size(), limit)
		}
		if err := rotateLog(path, limit, 3, log); err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
		if one := readFile(t, path+".1"); !strings.HasSuffix(one, last) {
			t.Fatalf("round %d: .1 does not end with the newest line %q:\n%s", round, last, one)
		}
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
		fi, err := e.Info()
		if err != nil {
			t.Fatal(err)
		}
		if fi.Size() > limit+4*growth {
			t.Errorf("%s is %d bytes, over the limit plus one check's growth", e.Name(), fi.Size())
		}
	}
	if got, want := strings.Join(names, " "), "gcpemu.log gcpemu.log.1 gcpemu.log.2 gcpemu.log.3"; got != want {
		t.Errorf("files = %s, want %s", got, want)
	}
	if two := readFile(t, path+".2"); !strings.Contains(two, "round 4 ") {
		t.Errorf(".2 does not hold the round before .1's:\n%s", two)
	}
	if three := readFile(t, path+".3"); !strings.Contains(three, "round 3 ") {
		t.Errorf(".3 does not hold round 3:\n%s", three)
	}
	if cur := readFile(t, path); !strings.Contains(cur, "instance log rotated") || !strings.Contains(cur, "level=INFO") {
		t.Errorf("gcpemu.log has no instance log rotated Info record:\n%s", cur)
	}
}

func TestRotateLogTruncatesUnderOpenAppendWriters(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gcpemu.log")
	other := appendLog(t, path) // a subprocess, or the parent's handle
	if _, err := other.Write(bytes.Repeat([]byte("old line\n"), 500)); err != nil {
		t.Fatal(err)
	}
	if err := rotateLog(path, 1024, 3, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}
	if _, err := other.WriteString("after rotation\n"); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, path); got != "after rotation\n" {
		t.Errorf("gcpemu.log = %q, want only the line written after rotation", got)
	}
}

func TestRotateLogTrimsAnOversizedFileAtALineBoundary(t *testing.T) {
	const limit = 1024
	path := filepath.Join(t.TempDir(), "gcpemu.log")
	var b strings.Builder
	for i := 0; b.Len() < 5*limit; i++ {
		fmt.Fprintf(&b, "line %d of an oversized log\n", i)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := rotateLog(path, limit, 3, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}
	one := readFile(t, path+".1")
	if len(one) > limit {
		t.Errorf(".1 is %d bytes, over the %d limit", len(one), limit)
	}
	if !strings.HasPrefix(one, "line ") || !strings.Contains(b.String(), "\n"+one) {
		t.Errorf(".1 does not start at a line boundary:\n%s", one)
	}
	if !strings.HasSuffix(b.String(), one) {
		t.Errorf(".1 does not end with the original's last line")
	}
	if fi, err := os.Stat(path + ".1"); err != nil {
		t.Fatal(err)
	} else if mode := fi.Mode().Perm(); mode != 0o600 && os.PathSeparator == '/' {
		t.Errorf(".1 mode = %o, want 600", mode)
	}
}

// bigLog writes a log over limit at path and returns a logger that writes
// to it as the detached child's does.
func bigLog(t *testing.T, path string, limit int) *slog.Logger {
	t.Helper()
	w := appendLog(t, path)
	if _, err := w.Write(bytes.Repeat([]byte("old line\n"), limit/9+1)); err != nil {
		t.Fatal(err)
	}
	return slog.New(slog.NewTextHandler(w, nil))
}

func TestRotateLogCopyFailureLosesTheLogButTruncates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gcpemu.log")
	log := bigLog(t, path, 1024)
	old := copyLogTail
	copyLogTail = func(src, dst string, size, maxSize int64) error {
		_ = os.WriteFile(dst, []byte("partial"), 0o600)
		return errors.New("no space left on device")
	}
	t.Cleanup(func() { copyLogTail = old })
	if err := rotateLog(path, 1024, 3, log); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ".1"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("partial .1 left behind: %v", err)
	}
	first, _, _ := strings.Cut(readFile(t, path), "\n")
	if !strings.Contains(first, "level=WARN") || !strings.Contains(first, "lost") || !strings.Contains(first, "no space left on device") {
		t.Errorf("first record = %q, want a Warn that the log was lost, and why", first)
	}
}

func TestRotationRetriesOnlyTheTruncateAfterItFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gcpemu.log")
	log := bigLog(t, path, 1024)
	fails := 1
	var beforeRetry string
	old := truncateLog
	truncateLog = func(name string, size int64) error {
		if fails > 0 {
			fails--
			return errors.New("sharing violation")
		}
		b, _ := os.ReadFile(name)
		beforeRetry = string(b)
		return old(name, size)
	}
	t.Cleanup(func() { truncateLog = old })
	stopped := make(chan struct{})
	done := startLogRotation(stopped, path, log, 1024, 3, 10*time.Millisecond)
	defer func() { close(stopped); <-done }()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		cur := readFile(t, path)
		if strings.Contains(cur, "instance log rotated") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("not rotated after a failed truncate:\n%s", cur)
		}
	}
	if !strings.Contains(beforeRetry, "level=WARN") || !strings.Contains(beforeRetry, "sharing violation") {
		t.Errorf("no Warn record for the failed truncate:\n%s", beforeRetry)
	}
	if _, err := os.Stat(path + ".2"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the retry rotated again, shifting .1 to .2: %v", err)
	}
	if one := readFile(t, path+".1"); !strings.HasPrefix(one, "old line\n") || strings.Contains(one, "level=") {
		t.Errorf(".1 does not hold the log as copied before the failed truncate:\n%s", one)
	}
}

func TestRotationStopsWhenDoneCloses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gcpemu.log")
	stop := make(chan struct{})
	close(stop)
	select {
	case <-startLogRotation(stop, path, slog.New(slog.DiscardHandler), 1024, 3, time.Hour):
	case <-time.After(5 * time.Second):
		t.Fatal("rotation did not stop when done closed")
	}
}

// hostDetached starts, as a detached child does, an Instance whose log is
// the file at path, after prepare has run on that file.
func hostDetached(t *testing.T, deterministic bool, prepare func(path string)) (*instance.Instance, string) {
	t.Helper()
	cfg := config.Defaults()
	cfg.Ephemeral = true
	cfg.DataDir = t.TempDir()
	cfg.Services = []string{"dns"}
	cfg.Ports["gateway"] = 0
	cfg.Deterministic = deterministic
	cfg.WaitTimeout = waitTimeout
	path := filepath.Join(cfg.DataDir, instance.LogFile)
	w := appendLog(t, path)
	prepare(path)
	o := &rootOpts{factories: map[string]instance.Factory{"dns": func(*emu.Env) emu.Service { return idleSvc{} }}, instanceLog: path}
	in, err := o.host(context.Background(), &cfg, w, false, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = shutdown(in) })
	return in, path
}

func TestDetachedChildRotatesItsLogAtStartup(t *testing.T) {
	_, path := hostDetached(t, false, func(path string) {
		if err := os.Truncate(path, logMaxSize); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(readFile(t, path), "instance log rotated") {
		t.Fatal("a log at the limit was not rotated at startup")
	}
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Error(err)
	}
}

func TestAdvancingTheEmulatorClockDoesNotRotate(t *testing.T) {
	in, path := hostDetached(t, true, func(string) {})
	// host made the first check, at startup, before it returned.
	if err := os.Truncate(path, logMaxSize+1); err != nil {
		t.Fatal(err)
	}
	if err := in.AdvanceClock(context.Background(), time.Hour); err != nil {
		t.Fatal(err)
	}
	time.Sleep(logCheckInterval / 10)
	if _, err := os.Stat(path + ".1"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("advancing the emulator clock rotated the log: %v", err)
	}
}
