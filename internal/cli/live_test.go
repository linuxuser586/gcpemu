package cli

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/gateway"
	"github.com/linuxuser586/gcpemu/internal/instance"
)

// deadAPIGateway returns an address where nothing listens.
func deadAPIGateway(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

// instanceAPIGateway returns the address of an API gateway serving the
// Instance id, which asks for token when token is not "".
func instanceAPIGateway(t *testing.T, id, token string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if token != "" && r.Header.Get(gateway.AdminTokenHeader) != token {
			http.Error(w, `{"error":"admin token required"}`, http.StatusUnauthorized)
			return
		}
		_, _ = fmt.Fprintf(w, `{"id":%q}`, id)
	}))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

// notAnInstance returns the address of a web server that is no Instance.
func notAnInstance(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "<html>hello</html>")
	}))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

// dataDir returns a data dir of the Instance "mine" whose endpoints.json
// names gw, and which holds the Admin token "kept".
func dataDir(t *testing.T, gw string) string {
	t.Helper()
	t.Setenv("GCPEMU_ADMIN_TOKEN", "")
	dir := t.TempDir()
	for name, body := range map[string]string{
		instance.IDFile:         "mine",
		instance.AdminTokenFile: "kept",
		instance.EndpointsFile:  `{"gateway":"` + gw + `","dns":"127.0.0.1:5999"}`,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// TestAdminTokenAsksTheInstance is #1018: a token left behind is never
// printed, however the gateway in endpoints.json fails to be this data
// dir's Instance.
func TestAdminTokenAsksTheInstance(t *testing.T) {
	t.Run("Live Instance", func(t *testing.T) {
		out, err := gcpemu(t, "--data-dir", dataDir(t, instanceAPIGateway(t, "mine", "kept")), "admin", "token")
		if err != nil || strings.TrimSpace(out) != "kept" {
			t.Errorf("admin token = %q, %v", out, err)
		}
	})
	for name, tt := range map[string]struct{ gw, want string }{
		"nothing answers":  {deadAPIGateway(t), "nothing answers at"},
		"another Instance": {instanceAPIGateway(t, "theirs", ""), "another Instance took the port"},
		"token refused":    {instanceAPIGateway(t, "mine", "other"), "refused this data dir's Admin token"},
		"not an Instance":  {notAnInstance(t), "did not say which Instance"},
	} {
		t.Run(name, func(t *testing.T) {
			out, err := gcpemu(t, "--data-dir", dataDir(t, tt.gw), "admin", "token")
			if err == nil || strings.Contains(out, "kept") {
				t.Fatalf("admin token = %q, %v, want no token and an error", out, err)
			}
			if !strings.Contains(err.Error(), "is not running") || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want it to say not running and %q", err, tt.want)
			}
		})
	}
}

// TestNoInstanceIDSkipsTheComparison is #1018, as for `stop`: a data dir
// without an instance-id file has no ID to compare, so an Instance that
// answers is taken as its own.
func TestNoInstanceIDSkipsTheComparison(t *testing.T) {
	dir := dataDir(t, instanceAPIGateway(t, "any", ""))
	if err := os.Remove(filepath.Join(dir, instance.IDFile)); err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	cfg.DataDir = dir
	if live := checkLive(&cfg); live.err != nil {
		t.Errorf("checkLive: %v", live.err)
	}
}

// TestDNSPortAsksTheInstance is #1018: only a Live Instance's dns port
// replaces the configured one.
func TestDNSPortAsksTheInstance(t *testing.T) {
	for name, tt := range map[string]struct {
		gw   string
		want int
	}{
		"Live Instance":     {instanceAPIGateway(t, "mine", "kept"), 5999},
		"no endpoints.json": {"", 5353},
		"nothing answers":   {deadAPIGateway(t), 5353},
		"another Instance":  {instanceAPIGateway(t, "theirs", ""), 5353},
		"token refused":     {instanceAPIGateway(t, "mine", "other"), 5353},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := config.Defaults()
			cfg.DataDir = dataDir(t, tt.gw)
			if tt.gw == "" {
				if err := os.Remove(filepath.Join(cfg.DataDir, instance.EndpointsFile)); err != nil {
					t.Fatal(err)
				}
			}
			if got := dnsPort(&cfg); got != tt.want {
				t.Errorf("dnsPort = %d, want %d", got, tt.want)
			}
		})
	}
}

// TestDNSInstallNeedsALiveInstanceForAPickedPort is #1018: a port an
// Instance left behind is not one Resolver routes can go to.
func TestDNSInstallNeedsALiveInstanceForAPickedPort(t *testing.T) {
	t.Setenv("GCPEMU_PORTS", "dns=0")
	_, err := gcpemu(t, "--data-dir", dataDir(t, deadAPIGateway(t)), "dns", "install", "--dry-run")
	if err == nil || !strings.Contains(err.Error(), "start the Instance first") {
		t.Errorf("err = %v, want it to say start the Instance first", err)
	}
}

// TestRefuseRunningAsksTheInstance is #1018: only a Live Instance is
// already running. A refused token also stops a start, without saying so:
// wrongly refusing is one clear error, and wrongly going on could put two
// Instances on one data dir.
func TestRefuseRunningAsksTheInstance(t *testing.T) {
	for name, tt := range map[string]struct{ gw, want string }{
		"Live Instance":     {instanceAPIGateway(t, "mine", "kept"), "already running"},
		"no endpoints.json": {"", ""},
		"nothing answers":   {deadAPIGateway(t), ""},
		"another Instance":  {instanceAPIGateway(t, "theirs", ""), ""},
		"token refused":     {instanceAPIGateway(t, "mine", "other"), "cannot tell whether"},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := config.Defaults()
			cfg.DataDir = dataDir(t, tt.gw)
			if tt.gw == "" {
				if err := os.Remove(filepath.Join(cfg.DataDir, instance.EndpointsFile)); err != nil {
					t.Fatal(err)
				}
			}
			err := refuseRunning(&cfg)
			switch {
			case tt.want == "" && err != nil:
				t.Errorf("err = %v, want none", err)
			case tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)):
				t.Errorf("err = %v, want it to say %q", err, tt.want)
			}
		})
	}
}

// TestDoctorAsksTheInstance is #1018: an Instance that left endpoints.json
// behind does not stop doctor checking the ports it would need.
func TestDoctorAsksTheInstance(t *testing.T) {
	const skipped = "skipping port checks"
	portChecked := func(out string) bool { return strings.Contains(out, "(gateway)") }

	t.Run("Live Instance", func(t *testing.T) {
		var asked atomic.Bool
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/_emu/v1/build/daemon" {
				asked.Store(true)
			}
			_, _ = io.WriteString(w, `{"id":"mine"}`)
		}))
		defer srv.Close()
		out, _ := gcpemu(t, "--data-dir", dataDir(t, strings.TrimPrefix(srv.URL, "http://")), "doctor")
		if !strings.Contains(out, skipped) || portChecked(out) || strings.Contains(out, "left behind") {
			t.Errorf("doctor:\n%s", out)
		}
		if !asked.Load() {
			t.Error("doctor did not run its running-Instance checks")
		}
	})
	t.Run("no endpoints.json", func(t *testing.T) {
		dir := dataDir(t, deadAPIGateway(t))
		if err := os.Remove(filepath.Join(dir, instance.EndpointsFile)); err != nil {
			t.Fatal(err)
		}
		out, _ := gcpemu(t, "--data-dir", dir, "doctor")
		if strings.Contains(out, skipped) || !portChecked(out) || strings.Contains(out, "left behind") {
			t.Errorf("doctor:\n%s", out)
		}
	})
	for name, tt := range map[string]struct{ gw, want string }{
		"nothing answers":  {deadAPIGateway(t), "nothing answers at"},
		"another Instance": {instanceAPIGateway(t, "theirs", ""), "another Instance took the port"},
		"token refused":    {instanceAPIGateway(t, "mine", "other"), "refused this data dir's Admin token"},
	} {
		t.Run(name, func(t *testing.T) {
			out, _ := gcpemu(t, "--data-dir", dataDir(t, tt.gw), "doctor")
			if strings.Contains(out, skipped) || !portChecked(out) {
				t.Errorf("doctor did not check ports:\n%s", out)
			}
			var line string
			for l := range strings.Lines(out) {
				if strings.Contains(l, tt.want) {
					line = l
				}
			}
			if !strings.HasPrefix(line, "[--  ] ") || !strings.Contains(line, instance.EndpointsFile) {
				t.Errorf("doctor has no [--  ] line saying %q:\n%s", tt.want, out)
			}
		})
	}
	t.Run("the file alone is no problem", func(t *testing.T) {
		out, err := gcpemu(t, "--data-dir", dataDir(t, deadAPIGateway(t)), "doctor")
		if strings.Contains(out, "[FAIL]") {
			t.Skipf("this host has problems of its own:\n%s", out)
		}
		if err != nil {
			t.Errorf("doctor: %v\n%s", err, out)
		}
	})
	t.Run("a port conflict is reported", func(t *testing.T) {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer l.Close()
		t.Setenv("GCPEMU_PORTS", fmt.Sprintf("gateway=%d", l.Addr().(*net.TCPAddr).Port))
		out, err := gcpemu(t, "--data-dir", dataDir(t, deadAPIGateway(t)), "doctor")
		if err == nil || !strings.Contains(out, "[FAIL] port") || !strings.Contains(out, "(gateway) free") {
			t.Errorf("err = %v, want the held gateway port reported:\n%s", err, out)
		}
	})
}
