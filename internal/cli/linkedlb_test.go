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
)

// frontAdmin fakes the Admin API of a front Instance delegating to office,
// which tells it one endpoint at an --lb-bind address and one without,
// and to lab, which is unreachable.
func frontAdmin(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/_emu/v1/info":
			_, _ = io.WriteString(w, `{"instance":"laptop","endpoints":{"gateway":"127.0.0.1:4000"}}`)
		case "/_emu/v1/ready":
			_, _ = io.WriteString(w, `{"ready":true,"services":{}}`)
		case "/_emu/v1/env":
			if r.URL.Query().Get("notes") != "true" {
				_, _ = io.WriteString(w, `{"GCPEMU_GATEWAY":"127.0.0.1:4000"}`)
				return
			}
			_, _ = io.WriteString(w, `{"vars":{"GCPEMU_GATEWAY":"127.0.0.1:4000","GCPEMU_LB_WEB":"192.168.1.5:8080"},`+
				`"notes":["forwarding rule api on link \"office\" gets no variable: its Instance has no --lb-bind"]}`)
		case "/_emu/v1/links/lb-endpoints":
			_, _ = io.WriteString(w, `{"links":[`+
				`{"link":"office","endpoints":[{"name":"lb:p/global/web","project":"p","what":"forwarding rule web","address":"192.168.1.5:8080"},`+
				`{"name":"lb:p/global/api","project":"p","what":"forwarding rule api","error":"its Instance has no --lb-bind"}]},`+
				`{"link":"lab","endpoints":[],"error":"connection refused"}]}`)
		default:
			_, _ = io.WriteString(w, `{}`)
		}
	}))
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "endpoints.json"), []byte(`{"gateway":"`+strings.TrimPrefix(srv.URL, "http://")+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// runCLI runs gcpemu args against the Instance in dir, returning stdout
// and stderr.
func runCLI(t *testing.T, dir string, args ...string) (string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	root := New(nil)
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs(append(args, "--config", filepath.Join(dir, "none.yaml"), "--data-dir", dir))
	if err := root.Execute(); err != nil {
		t.Fatalf("gcpemu %v: %v", args, err)
	}
	return out.String(), errOut.String()
}

// `gcpemu env` prints the Linked Instances' variables on stdout and why
// some got none on stderr, so that `eval $(gcpemu env)` keeps working
// (#715).
func TestEnvPrintsLinkedNotesOnStderr(t *testing.T) {
	out, errOut := runCLI(t, frontAdmin(t), "env")
	if !strings.Contains(out, "export GCPEMU_LB_WEB='192.168.1.5:8080'") || strings.Contains(out, "office") {
		t.Errorf("stdout:\n%s\nwant the variables only", out)
	}
	if !strings.Contains(errOut, `# gcpemu: forwarding rule api on link "office" gets no variable: its Instance has no --lb-bind`) {
		t.Errorf("stderr:\n%s\nwant the note as a comment", errOut)
	}
}

// `gcpemu status` lists the Linked Instances' load balancing endpoints,
// attributed to their link, and an unreachable link (#715).
func TestStatusListsLinkedLBEndpoints(t *testing.T) {
	out, _ := runCLI(t, frontAdmin(t), "status")
	for _, want := range []string{
		"linked load balancing endpoints:",
		"office   lb:p/global/web 192.168.1.5:8080",
		"office   lb:p/global/api (none: its Instance has no --lb-bind)",
		"lab      unreachable: connection refused",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("status lacks %q:\n%s", want, out)
		}
	}
}
