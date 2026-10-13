package cli

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/gateway"
	"github.com/linuxuser586/gcpemu/internal/instance"
)

// TestAdminTokenCLI is FR-UI-009: gcpemu commands send the Admin token
// from the instance dir, `admin token` prints it and `console` opens a URL
// that carries it.
func TestAdminTokenCLI(t *testing.T) {
	t.Setenv("GCPEMU_ADMIN_TOKEN", "")
	tok := strings.Repeat("t", config.MinAdminTokenLen)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(gateway.AdminTokenHeader) != tok {
			http.Error(w, `{"error":"no token"}`, http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/_emu/v1/info":
			_, _ = io.WriteString(w, `{"console":true}`)
		default:
			_, _ = io.WriteString(w, `{"rules":[]}`)
		}
	}))
	defer srv.Close()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, instance.EndpointsFile), []byte(`{"gateway":"`+strings.TrimPrefix(srv.URL, "http://")+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	tokFile := filepath.Join(dir, instance.AdminTokenFile)
	if err := os.WriteFile(tokFile, []byte(tok), 0o600); err != nil {
		t.Fatal(err)
	}
	var opened string
	old := openBrowser
	openBrowser = func(url string) error { opened = url; return nil }
	t.Cleanup(func() { openBrowser = old })

	run := func(args ...string) (string, error) {
		var out bytes.Buffer
		root := New(nil)
		root.SetOut(&out)
		root.SetArgs(append([]string{"--config", filepath.Join(dir, "none.yaml"), "--data-dir", dir}, args...))
		err := root.Execute()
		return out.String(), err
	}
	if out, err := run("fault", "list"); err != nil || !strings.Contains(out, "rules") {
		t.Errorf("fault list: %q %v", out, err)
	}
	if out, err := run("admin", "token"); err != nil || strings.TrimSpace(out) != tok {
		t.Errorf("admin token: %q %v", out, err)
	}
	if _, err := run("console"); err != nil || opened != srv.URL+"/console/?token="+tok {
		t.Errorf("console: err %v, opened %q", err, opened)
	}

	// GCPEMU_ADMIN_TOKEN serves when there is no token file.
	_ = os.Remove(tokFile)
	t.Setenv("GCPEMU_ADMIN_TOKEN", tok)
	if out, err := run("fault", "list"); err != nil || !strings.Contains(out, "rules") {
		t.Errorf("fault list with GCPEMU_ADMIN_TOKEN: %q %v", out, err)
	}
	if _, err := run("admin", "token"); err == nil || !strings.Contains(err.Error(), "loopback") {
		t.Errorf("admin token without a token file: %v", err)
	}
	if _, err := run("--data-dir", t.TempDir(), "admin", "token"); err == nil || !strings.Contains(err.Error(), "not running") {
		t.Errorf("admin token without an instance: %v", err)
	}
}

func TestConsoleURL(t *testing.T) {
	for _, tc := range []struct{ addr, token, want string }{
		{"127.0.0.1:4510", "", "http://127.0.0.1:4510/console/"},
		{"0.0.0.0:4510", "a+b", "http://127.0.0.1:4510/console/?token=a%2Bb"},
		{"[::]:4510", "t", "http://127.0.0.1:4510/console/?token=t"},
		{"192.0.2.1:4510", "t", "http://192.0.2.1:4510/console/?token=t"},
	} {
		if got := consoleURL(tc.addr, tc.token); got != tc.want {
			t.Errorf("consoleURL(%q, %q) = %q, want %q", tc.addr, tc.token, got, tc.want)
		}
	}
}

func TestPrintAdminToken(t *testing.T) {
	tok := strings.Repeat("p", config.MinAdminTokenLen)
	var out bytes.Buffer
	printAdminToken(&out, &config.Config{}, "0.0.0.0:4510", tok, true)
	if !strings.Contains(out.String(), "http://127.0.0.1:4510/console/?token="+tok) {
		t.Errorf("generated token, console:\n%s", out.String())
	}
	out.Reset()
	printAdminToken(&out, &config.Config{}, "0.0.0.0:4510", tok, false)
	if !strings.Contains(out.String(), "Admin token: "+tok) || strings.Contains(out.String(), "/console/") {
		t.Errorf("generated token, no console:\n%s", out.String())
	}
	out.Reset()
	printAdminToken(&out, &config.Config{AdminToken: tok}, "0.0.0.0:4510", tok, true)
	if strings.Contains(out.String(), tok) || !strings.Contains(out.String(), "GCPEMU_ADMIN_TOKEN") {
		t.Errorf("configured token must not be printed:\n%s", out.String())
	}
	out.Reset()
	printAdminToken(&out, &config.Config{}, "127.0.0.1:4510", "", true)
	if out.Len() != 0 {
		t.Errorf("loopback printed:\n%s", out.String())
	}
}
