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

// TestStatusReady is NFR-PORT-004: `gcpemu status --ready` is the OCI
// image's HEALTHCHECK, so it fails until every service is ready, and plain
// `status` does not.
func TestStatusReady(t *testing.T) {
	ready := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/_emu/v1/info":
			_, _ = io.WriteString(w, `{"instance":"default"}`)
		case "/_emu/v1/ready":
			if !ready {
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = io.WriteString(w, `{"ready":false,"services":{"gcs":{"ready":true},"containers":{"ready":false,"reason":"gcpemu-x-sql (exited)"}}}`)
				return
			}
			_, _ = io.WriteString(w, `{"ready":true,"services":{"gcs":{"ready":true}}}`)
		default:
			_, _ = io.WriteString(w, `{"containers":[]}`)
		}
	}))
	defer srv.Close()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "endpoints.json"), []byte(`{"gateway":"`+strings.TrimPrefix(srv.URL, "http://")+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) error {
		root := New(nil)
		root.SetOut(io.Discard)
		root.SetArgs(append([]string{"status", "--config", filepath.Join(dir, "none.yaml"), "--data-dir", dir}, args...))
		return root.Execute()
	}
	if err := run(); err != nil {
		t.Errorf("status while not ready: %v", err)
	}
	for _, args := range [][]string{{"--ready"}, {"--ready", "--json"}} {
		// The error names what is down, e.g. containers the runtime stopped (#318).
		err := run(args...)
		if err == nil || !strings.Contains(err.Error(), "not ready") || !strings.Contains(err.Error(), "containers: gcpemu-x-sql (exited)") {
			t.Errorf("status %v while not ready: %v", args, err)
		}
	}
	ready = true
	for _, args := range [][]string{{"--ready"}, {"--ready", "--json"}} {
		if err := run(args...); err != nil {
			t.Errorf("status %v when ready: %v", args, err)
		}
	}
	if err := run("--ready", "--data-dir", t.TempDir()); err == nil || !strings.Contains(err.Error(), "has no endpoints.json") {
		t.Errorf("status --ready without an instance: %v", err)
	}
}

// `gcpemu status` lists the Kubernetes Services that hold Loopback aliases
// (ADR 0014).
func TestPrintHostServices(t *testing.T) {
	var out strings.Builder
	printHostServices(&out, []byte(hostServicesBody))
	want := "kubernetes services for host clients:\n" +
		"  web.default.svc.c1.l.p.gke.internal (web.default.svc.cluster.local) at 127.0.0.7: 8080 9090\n" +
		"  api.default.svc.c1.l.p.gke.internal at 127.0.0.8\n"
	if out.String() != want {
		t.Errorf("got:\n%s\nwant:\n%s", out.String(), want)
	}
	out.Reset()
	printHostServices(&out, []byte(`{"services":[]}`))
	if out.String() != "" {
		t.Errorf("none: %q", out.String())
	}
}

// TestStatusGKENodeLimit: plain `gcpemu status` prints the Node limit when
// info.gke is present, and nothing for it on an info body without it
// (#1065); --json carries info unchanged.
func TestStatusGKENodeLimit(t *testing.T) {
	for _, tc := range []struct {
		name, info, want string
	}{
		{"gke", `{"instance":"default","endpoints":{"gke":"127.0.0.1:1"},"gke":{"nodeLimit":8}}`, "gke      node limit 8\n"},
		{"older", `{"instance":"default","endpoints":{"gke":"127.0.0.1:1"}}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/_emu/v1/info":
					_, _ = io.WriteString(w, tc.info)
				case "/_emu/v1/ready":
					_, _ = io.WriteString(w, `{"ready":true,"services":{"gke":{"ready":true}}}`)
				default:
					_, _ = io.WriteString(w, `{"containers":[]}`)
				}
			}))
			defer srv.Close()
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "endpoints.json"), []byte(`{"gateway":"`+strings.TrimPrefix(srv.URL, "http://")+`"}`), 0o600); err != nil {
				t.Fatal(err)
			}
			run := func(args ...string) string {
				var out strings.Builder
				root := New(nil)
				root.SetOut(&out)
				root.SetArgs(append([]string{"status", "--config", filepath.Join(dir, "none.yaml"), "--data-dir", dir}, args...))
				if err := root.Execute(); err != nil {
					t.Fatal(err)
				}
				return out.String()
			}
			out := run()
			if tc.want != "" {
				// After the service endpoints, before the containers.
				if !strings.Contains(out, "  gke      127.0.0.1:1\n"+tc.want+"containers:") {
					t.Errorf("status:\n%s", out)
				}
			} else if strings.Contains(out, "node limit") {
				t.Errorf("status without info.gke:\n%s", out)
			}
			js := run("--json")
			if strings.Contains(js, `"gke":{"nodeLimit":8}`) != (tc.want != "") {
				t.Errorf("status --json: %s", js)
			}
		})
	}
}
