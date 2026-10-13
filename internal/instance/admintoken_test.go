package instance

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/frontend"
	"github.com/linuxuser586/gcpemu/internal/gateway"
)

// adminSvc adds a service-registered admin route, like certs' ca/rotate.
type adminSvc struct{ fakeSvc }

func (s *adminSvc) Register(r emu.Router) error {
	if err := s.fakeSvc.Register(r); err != nil {
		return err
	}
	r.Handle("POST /_emu/v1/fake/rotate", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"status":"rotated"}`)
	}))
	return nil
}

func startAdminToken(t *testing.T, bind, token string) (*Instance, string, *syncBuffer) {
	t.Helper()
	t.Setenv("CI", "")
	cfg := config.Defaults()
	cfg.Ephemeral = true
	cfg.DataDir = t.TempDir()
	cfg.Services = []string{"dns"}
	cfg.Ports["gateway"] = 0
	cfg.Ports[frontend.Endpoint] = 0
	cfg.Bind = bind
	cfg.AdminToken = token
	cfg.LogFormat = "text"
	log := &syncBuffer{}
	in, err := New(&cfg, map[string]Factory{"dns": func(*emu.Env) emu.Service { return &adminSvc{} }}, log)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := in.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = in.Shutdown(ctx) })
	_, port, _ := net.SplitHostPort(in.Env.Endpoints.Get("gateway"))
	return in, "http://127.0.0.1:" + port, log
}

// noRedirect is a client that returns redirects instead of following them.
var noRedirect = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

func call(t *testing.T, c *http.Client, method, u string, h http.Header) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(method, u, nil)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range h {
		req.Header[k] = v
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(b)
}

// TestAdminToken is FR-UI-009 (ADR 0006): beyond loopback, the Admin API
// and the Web console need the Admin token; public APIs and probes do not.
func TestAdminToken(t *testing.T) {
	in, base, log := startAdminToken(t, "0.0.0.0", "")
	tok := in.AdminToken()
	if len(tok) < config.MinAdminTokenLen {
		t.Fatalf("admin token %q is too short", tok)
	}
	path := filepath.Join(in.Dir, AdminTokenFile)
	// Windows does not enforce file modes (ADR 0008).
	if fi, err := os.Stat(path); err != nil || runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Fatalf("token file: %v %v", fi, err)
	}
	if b, _ := os.ReadFile(path); string(b) != tok {
		t.Errorf("token file = %q, want the admin token", b)
	}
	if got := in.EnvVars()["GCPEMU_ADMIN_TOKEN"]; got != tok {
		t.Errorf("EnvVars GCPEMU_ADMIN_TOKEN = %q", got)
	}
	header := http.Header{gateway.AdminTokenHeader: {tok}}
	cookie := http.Header{"Cookie": {gateway.AdminTokenCookie + "=" + tok}}
	wrong := http.Header{gateway.AdminTokenHeader: {strings.Repeat("x", len(tok))}}

	for _, tc := range []struct {
		method, path string
		h            http.Header
		want         int
	}{
		{"GET", "/_emu/v1/info", nil, 401},
		{"POST", "/_emu/v1/fake/rotate", nil, 401},
		{"GET", "/console/", nil, 401},
		{"GET", "/console", nil, 401},
		{"GET", "/_emu/v1/info", wrong, 401},
		{"POST", "/_emu/v1/health", nil, 401},
		{"GET", "/_emu/v1/health", nil, 200},
		{"GET", "/_emu/v1/ready", nil, 200},
		{"GET", "/dns/v1/projects/p/managedZones", nil, 200},
		{"GET", "/_emu/v1/info", header, 200},
		{"POST", "/_emu/v1/fake/rotate", header, 200},
		{"GET", "/console/", header, 200},
		{"GET", "/_emu/v1/info", cookie, 200},
		{"GET", "/console/gcs", cookie, 200},
	} {
		resp, body := call(t, noRedirect, tc.method, base+tc.path, tc.h)
		if resp.StatusCode != tc.want {
			t.Errorf("%s %s (%v): %d %s, want %d", tc.method, tc.path, tc.h != nil, resp.StatusCode, body, tc.want)
		}
		if resp.StatusCode == 401 {
			if strings.Contains(body, tok) {
				t.Errorf("%s %s: 401 body leaks the token", tc.method, tc.path)
			}
			if strings.HasPrefix(tc.path, "/_emu/") && !strings.Contains(body, gateway.AdminTokenHeader) {
				t.Errorf("%s %s: 401 body does not name the header: %s", tc.method, tc.path, body)
			}
			if strings.HasPrefix(tc.path, "/console") && !strings.Contains(body, "gcpemu admin token") {
				t.Errorf("%s %s: 401 page does not explain itself: %s", tc.method, tc.path, body)
			}
		}
	}

	t.Run("console URL sets the cookie", func(t *testing.T) {
		resp, _ := call(t, noRedirect, "GET", base+"/console/gcs?project=p1&token="+url.QueryEscape(tok), nil)
		if resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("status %d, want 303", resp.StatusCode)
		}
		if loc := resp.Header.Get("Location"); loc != "/console/gcs?project=p1" {
			t.Errorf("Location = %q", loc)
		}
		var c *http.Cookie
		for _, k := range resp.Cookies() {
			if k.Name == gateway.AdminTokenCookie {
				c = k
			}
		}
		if c == nil || c.Value != tok || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.Path != "/" {
			t.Errorf("cookie = %+v", c)
		}

		resp, _ = call(t, noRedirect, "GET", base+"/console/?token=bad", nil)
		if resp.StatusCode != 401 || len(resp.Cookies()) != 0 {
			t.Errorf("bad token: %d, cookies %v", resp.StatusCode, resp.Cookies())
		}
	})

	t.Run("Google front end", func(t *testing.T) {
		_, port, _ := net.SplitHostPort(in.Env.Endpoints.Get(frontend.Endpoint))
		c := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: in.Env.CA.Pool()}}}
		fe := "https://127.0.0.1:" + port + "/_emu/v1/info"
		if resp, body := call(t, c, "GET", fe, nil); resp.StatusCode != 401 {
			t.Errorf("without token: %d %s", resp.StatusCode, body)
		}
		if resp, body := call(t, c, "GET", fe, header); resp.StatusCode != 200 {
			t.Errorf("with token: %d %s", resp.StatusCode, body)
		}
	})

	if strings.Contains(log.String(), tok) {
		t.Errorf("log contains the admin token:\n%s", log.String())
	}
	_ = in.Shutdown(context.Background())
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("token file left after shutdown: %v", err)
	}
}

func TestAdminTokenConfigured(t *testing.T) {
	tok := strings.Repeat("k", config.MinAdminTokenLen)
	in, base, _ := startAdminToken(t, "0.0.0.0", tok)
	if in.AdminToken() != tok {
		t.Fatalf("AdminToken = %q, want the configured one", in.AdminToken())
	}
	if resp, body := call(t, noRedirect, "GET", base+"/_emu/v1/info", http.Header{gateway.AdminTokenHeader: {tok}}); resp.StatusCode != 200 {
		t.Errorf("configured token: %d %s", resp.StatusCode, body)
	}

	cfg := config.Defaults()
	cfg.Bind = "0.0.0.0"
	cfg.AdminToken = "short"
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "GCPEMU_ADMIN_TOKEN") {
		t.Errorf("short token: %v", err)
	}
}

func TestAdminTokenLoopback(t *testing.T) {
	t.Setenv("CI", "")
	dir := t.TempDir()
	stale := filepath.Join(dir, AdminTokenFile)
	if err := os.WriteFile(stale, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	cfg.Ephemeral = true
	cfg.DataDir = dir
	cfg.Services = []string{"dns"}
	cfg.Ports["gateway"] = 0
	in, err := New(&cfg, map[string]Factory{"dns": func(*emu.Env) emu.Service { return &adminSvc{} }}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := in.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer in.Shutdown(ctx)
	if in.AdminToken() != "" {
		t.Errorf("AdminToken = %q on loopback", in.AdminToken())
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("stale token file kept: %v", err)
	}
	if _, ok := in.EnvVars()["GCPEMU_ADMIN_TOKEN"]; ok {
		t.Error("EnvVars has GCPEMU_ADMIN_TOKEN on loopback")
	}
	base := "http://" + in.Env.Endpoints.Get("gateway")
	for _, p := range []string{"/_emu/v1/info", "/console/"} {
		if resp, body := call(t, noRedirect, "GET", base+p, nil); resp.StatusCode != 200 {
			t.Errorf("GET %s on loopback: %d %s", p, resp.StatusCode, body)
		}
	}
}
