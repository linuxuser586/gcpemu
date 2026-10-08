package cli

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestConsoleCmd is FR-UI-002: `gcpemu console` opens the running
// instance's console, and explains why when the instance does not serve it.
func TestConsoleCmd(t *testing.T) {
	served := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if served {
			_, _ = io.WriteString(w, `{"console":true}`)
		} else {
			_, _ = io.WriteString(w, `{"console":false}`)
		}
	}))
	defer srv.Close()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "endpoints.json"), []byte(`{"gateway":"`+strings.TrimPrefix(srv.URL, "http://")+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var opened string
	old := openBrowser
	openBrowser = func(url string) error { opened = url; return nil }
	t.Cleanup(func() { openBrowser = old })

	run := func(args ...string) error {
		root := New(nil)
		root.SetOut(io.Discard)
		root.SetArgs(append([]string{"console", "--config", filepath.Join(dir, "none.yaml"), "--data-dir", dir}, args...))
		return root.Execute()
	}
	if err := run(); err != nil || opened != srv.URL+"/console/" {
		t.Errorf("console: err %v, opened %q", err, opened)
	}
	served = false
	if err := run(); err == nil || !strings.Contains(err.Error(), "does not serve the Web console") {
		t.Errorf("console when not served: %v", err)
	}
	if err := run("--data-dir", t.TempDir()); err == nil || !strings.Contains(err.Error(), "not running") {
		t.Errorf("console without an instance: %v", err)
	}
}
