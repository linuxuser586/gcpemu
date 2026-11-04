package cli

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/linuxuser586/gcpemu/internal/instance"
)

// hungInstance is the data dir of an Instance that accepts a shutdown and
// never finishes it, with pidContents in its PID file.
func hungInstance(t *testing.T, pidContents string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/_emu/v1/shutdown" {
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	dir := writeEndpoints(t, strings.TrimPrefix(srv.URL, "http://"))
	if err := os.WriteFile(filepath.Join(dir, instance.PIDFile), []byte(pidContents), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// runStop runs `gcpemu stop` on dir, with a stopTimeout short enough for a
// test to wait out.
func runStop(t *testing.T, dir string) error {
	t.Helper()
	was := stopTimeout
	stopTimeout = 300 * time.Millisecond
	t.Cleanup(func() { stopTimeout = was })
	root := New(nil)
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	root.SetArgs([]string{"stop", "--config", filepath.Join(dir, "none.yaml"), "--data-dir", dir})
	return root.Execute()
}

// TestStopTimeoutNamesPID is #999: a stop that gives up says which process
// it was still waiting on. This test's own process plays the one that
// will not exit, and so also shows that stop leaves it running.
func TestStopTimeoutNamesPID(t *testing.T) {
	pid := os.Getpid()
	err := runStop(t, hungInstance(t, strconv.Itoa(pid)))
	if err == nil {
		t.Fatal("stop succeeded with the process still running")
	}
	if want := fmt.Sprintf("PID %d is still running", pid); !strings.Contains(err.Error(), want) {
		t.Errorf("error = %q, want it to contain %q", err, want)
	}
}

// TestStopTimeoutWithoutPID is #999: with no PID to name, the error names
// the PID file that stop saw remain, and not a "PID 0".
func TestStopTimeoutWithoutPID(t *testing.T) {
	err := runStop(t, hungInstance(t, "not a pid"))
	if err == nil {
		t.Fatal("stop succeeded with the PID file still there")
	}
	if want := instance.PIDFile + " remains"; !strings.Contains(err.Error(), want) {
		t.Errorf("error = %q, want it to contain %q", err, want)
	}
	if strings.Contains(err.Error(), "PID 0") {
		t.Errorf("error = %q names a PID it never had", err)
	}
}
