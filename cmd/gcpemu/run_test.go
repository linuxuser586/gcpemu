package main_test

import (
	"bufio"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// runChild is the command that `gcpemu run` runs in these tests: the test
// binary itself, so it works on every OS. It returns its exit code.
func runChild(mode string) int {
	switch mode {
	case "true":
		return 0
	case "exit7":
		return 7
	case "sleep": // until a signal ends it
		fmt.Println("sleeping")
		time.Sleep(60 * time.Second)
		return 0
	case "create-bucket": // through the variable `gcpemu env` prints
		resp, err := http.Post("http://"+os.Getenv("STORAGE_EMULATOR_HOST")+"/storage/v1/b?project=run-proj", "application/json", strings.NewReader(`{"name":"run-bucket"}`))
		if err != nil || resp.StatusCode != http.StatusOK {
			fmt.Fprintln(os.Stderr, "create bucket:", resp, err)
			return 3
		}
		return 0
	case "has-bucket":
		resp, err := http.Get("http://" + os.Getenv("STORAGE_EMULATOR_HOST") + "/storage/v1/b/run-bucket")
		if err != nil {
			fmt.Fprintln(os.Stderr, "get bucket:", err)
			return 3
		}
		if resp.StatusCode != http.StatusOK {
			return 4
		}
		return 0
	}
	return 2
}

// runGcpemu runs `gcpemu run` with the test binary as its command, in mode,
// and returns its combined output and exit code.
func runGcpemu(t *testing.T, mode string, args ...string) (string, int) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, append(append([]string{"run"}, args...), "--", exe)...)
	cmd.Env = append(os.Environ(), "CI=", "GCPEMU_INSTANCE=", "GCPEMU_TEST_CHILD="+mode)
	out, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	if err != nil && !errors.As(err, &ee) {
		t.Fatal(err)
	}
	return string(out), cmd.ProcessState.ExitCode()
}

// TestRunExitCode is FR-CI-008: `gcpemu run` exits with its command's
// code and leaves no Instance running.
func TestRunExitCode(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the binary")
	}
	dir := t.TempDir()
	out, code := runGcpemu(t, "exit7", "--data-dir", dir, "--services", "gcs", "--port", "0")
	if code != 7 {
		t.Fatalf("exit code %d, want 7:\n%s", code, out)
	}
	if out, err := gcpemu(t, nil, "status", "--data-dir", dir); err == nil || !strings.Contains(out, "not running") {
		t.Errorf("status after run: %v %s", err, out)
	}
}

// TestRunEphemeralByDefault is FR-CI-008: the command reaches the Instance
// through the variables `gcpemu env` prints, and by default the Instance
// is Ephemeral: its state goes when `run` exits.
func TestRunEphemeralByDefault(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the binary")
	}
	dir := t.TempDir()
	tmp := t.TempDir() // where the Ephemeral Instance keeps its data
	t.Setenv("TMPDIR", tmp)
	t.Setenv("TMP", tmp)
	t.Setenv("TEMP", tmp)
	args := []string{"--data-dir", dir, "--services", "gcs", "--port", "0"}
	if out, code := runGcpemu(t, "create-bucket", args...); code != 0 {
		t.Fatalf("create-bucket: exit %d:\n%s", code, out)
	}
	if _, err := os.Stat(filepath.Join(dir, "state.db")); !os.IsNotExist(err) {
		t.Errorf("state.db left behind: %v", err)
	}
	if left, _ := os.ReadDir(tmp); len(left) > 0 {
		t.Errorf("data left in the temp dir: %v", left)
	}
	if out, code := runGcpemu(t, "has-bucket", args...); code != 4 {
		t.Errorf("has-bucket on a new Ephemeral Instance: exit %d, want 4:\n%s", code, out)
	}
}

// TestRunPersistent is FR-CI-008: with --ephemeral=false a Persistent
// Instance's data survives into the next `run`.
func TestRunPersistent(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the binary")
	}
	args := []string{"--data-dir", t.TempDir(), "--services", "gcs", "--port", "0", "--ephemeral=false"}
	if out, code := runGcpemu(t, "create-bucket", args...); code != 0 {
		t.Fatalf("create-bucket: exit %d:\n%s", code, out)
	}
	if out, code := runGcpemu(t, "has-bucket", args...); code != 0 {
		t.Errorf("has-bucket: exit %d:\n%s", code, out)
	}
}

// TestRunRefusesRunningInstance is FR-CI-008: `run` leaves an Instance of
// the same name that is already running alone, and fails.
func TestRunRefusesRunningInstance(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the binary")
	}
	dir := t.TempDir()
	startDetached(t, dir)
	out, code := runGcpemu(t, "exit7", "--data-dir", dir, "--services", "gcs", "--port", "0")
	if code != 1 || !strings.Contains(out, "already running") {
		t.Errorf("exit %d:\n%s", code, out)
	}
	must(t, nil, "status", "--ready", "--data-dir", dir)
}

// TestRunLogLevel is FR-CI-008: the Instance logs at warn unless
// --log-level says otherwise.
func TestRunLogLevel(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the binary")
	}
	args := []string{"--data-dir", t.TempDir(), "--services", "gcs", "--port", "0"}
	if out, code := runGcpemu(t, "true", args...); code != 0 || strings.Contains(out, "level=INFO") {
		t.Errorf("default: exit %d:\n%s", code, out)
	}
	if out, code := runGcpemu(t, "true", append(args, "--log-level=info")...); code != 0 || !strings.Contains(out, "msg=ready") {
		t.Errorf("--log-level=info: exit %d:\n%s", code, out)
	}
}

// TestRunSIGTERM is FR-CI-008: SIGTERM goes to the command, and `run`
// tears down once it has exited and exits as a shell reports the signal.
func TestRunSIGTERM(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the binary")
	}
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no SIGTERM")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	cmd := exec.Command(bin, "run", "--data-dir", dir, "--services", "gcs", "--port", "0", "--", exe)
	cmd.Env = append(os.Environ(), "CI=", "GCPEMU_INSTANCE=", "GCPEMU_TEST_CHILD=sleep")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	line, _ := bufio.NewReader(stdout).ReadString('\n')
	if line != "sleeping\n" {
		t.Fatalf("command said %q:\n%s", line, stderr.String())
	}
	if err := sigterm(cmd.Process.Pid); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("still running 30 s after SIGTERM")
	}
	if code := cmd.ProcessState.ExitCode(); code != 143 {
		t.Errorf("exit %d, want 143:\n%s", code, stderr.String())
	}
	if out, err := gcpemu(t, nil, "status", "--data-dir", dir); err == nil {
		t.Errorf("status after run: %s", out)
	}
}
