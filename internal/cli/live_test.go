package cli

import (
	"bytes"
	"errors"
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

	"github.com/linuxuser586/gcpemu/internal/ca"
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

// childPID is the process countingAPIGateway's Instance reports.
const childPID = 4242

// countingAPIGateway returns the address of an API gateway serving the Instance
// id, which asks for token when token is not "", and a count of the
// requests that reached it other than the one asking which Instance it is.
func countingAPIGateway(t *testing.T, id, token string) (string, *atomic.Int64) {
	t.Helper()
	acted := &atomic.Int64{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/_emu/v1/info" {
			acted.Add(1)
		}
		if token != "" && r.Header.Get(gateway.AdminTokenHeader) != token {
			http.Error(w, `{"error":"admin token required"}`, http.StatusUnauthorized)
			return
		}
		_, _ = fmt.Fprintf(w, `{"id":%q,"pid":%d}`, id, childPID)
	}))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://"), acted
}

// TestCommandsReportOneReason is #1024's table: whatever keeps a command
// from a Live Instance, it fails with the check's reason and sends nothing
// more. The other tests here take the rest of the commands through it.
func TestCommandsReportOneReason(t *testing.T) {
	t.Run("Live Instance", func(t *testing.T) {
		gw, acted := countingAPIGateway(t, "mine", "kept")
		out, err := gcpemu(t, "--data-dir", dataDir(t, gw), "reset")
		if err != nil || !strings.Contains(out, "state reset") || acted.Load() != 1 {
			t.Errorf("reset = %q, %v, with %d requests, want it reset", out, err, acted.Load())
		}
	})
	t.Run("no endpoints.json", func(t *testing.T) {
		dir := dataDir(t, deadAPIGateway(t))
		if err := os.Remove(filepath.Join(dir, instance.EndpointsFile)); err != nil {
			t.Fatal(err)
		}
		_, err := gcpemu(t, "--data-dir", dir, "reset")
		const want = `instance "default" has no endpoints.json in `
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want it to say %q", err, want)
		}
	})
	t.Run("unreadable endpoints.json", func(t *testing.T) {
		dir := dataDir(t, deadAPIGateway(t))
		if err := os.WriteFile(filepath.Join(dir, instance.EndpointsFile), []byte("{"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := gcpemu(t, "--data-dir", dir, "reset")
		const want = `instance "default" has an unreadable endpoints.json: `
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want it to say %q", err, want)
		}
	})
	t.Run("no API gateway recorded", func(t *testing.T) {
		dir := dataDir(t, deadAPIGateway(t))
		if err := os.WriteFile(filepath.Join(dir, instance.EndpointsFile), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := gcpemu(t, "--data-dir", dir, "reset")
		const want = `instance "default" has no API gateway address in endpoints.json`
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want it to say %q", err, want)
		}
	})
	theirs, theirsActed := countingAPIGateway(t, "theirs", "")
	web := notAnInstance(t)
	refused, refusedActed := countingAPIGateway(t, "mine", "other")
	for name, tt := range map[string]struct {
		gw, want string
		acted    *atomic.Int64
	}{
		"nothing answers":  {deadAPIGateway(t), `instance "default" is not running: nothing answers at `, nil},
		"another Instance": {theirs, `instance "default" is not running: the API gateway serves Instance theirs, not mine: another Instance took the port`, theirsActed},
		"no Instance":      {web, `instance "default" is not running: what answers at ` + web + ` did not say which Instance it serves`, nil},
		"token refused":    {refused, `cannot tell whether instance "default" is running: the API gateway at ` + refused + ` refused this data dir's Admin token`, refusedActed},
	} {
		t.Run(name, func(t *testing.T) {
			out, err := gcpemu(t, "--data-dir", dataDir(t, tt.gw), "reset")
			if err == nil || strings.Contains(out, "state reset") {
				t.Fatalf("reset = %q, %v, want an error", out, err)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want it to say %q", err, tt.want)
			}
			if tt.acted != nil && tt.acted.Load() != 0 {
				t.Errorf("reset reached the API gateway with %d requests after the check", tt.acted.Load())
			}
		})
	}
}

// TestAnotherInstanceIsNotActedOn is #1024: from a data dir whose
// endpoints.json names another Instance, no command reports on that Instance
// or sends it anything but the check.
func TestAnotherInstanceIsNotActedOn(t *testing.T) {
	const want = `instance "default" is not running: the API gateway serves Instance theirs, not mine: another Instance took the port`
	for name, args := range map[string][]string{
		"status":        {"status"},
		"wait":          {"wait", "--for", "gke/cluster=p/z/c", "--timeout", "1s"},
		"adminRequest":  {"fault", "list"},
		"env":           {"env"},
		"hosts":         {"hosts"},
		"logs requests": {"logs", "--requests"},
		"time advance":  {"time", "advance", "1h"},
		"console":       {"console", "--print"},
	} {
		t.Run(name, func(t *testing.T) {
			gw, acted := countingAPIGateway(t, "theirs", "")
			out, err := gcpemu(t, append([]string{"--data-dir", dataDir(t, gw)}, args...)...)
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("err = %v, want it to say %q", err, want)
			}
			if strings.Contains(out, "theirs") && !strings.Contains(out, "Error:") {
				t.Errorf("output reports the other Instance:\n%s", out)
			}
			if acted.Load() != 0 {
				t.Errorf("%d requests reached the other Instance after the check", acted.Load())
			}
		})
	}
}

// TestStatusAndWaitSayWhy is #1024: they return the check's reason, not a
// bare "not running".
func TestStatusAndWaitSayWhy(t *testing.T) {
	for name, args := range map[string][]string{
		"status": {"status"},
		"wait":   {"wait", "--for", "gke/cluster=p/z/c", "--timeout", "1s"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := gcpemu(t, append([]string{"--data-dir", dataDir(t, deadAPIGateway(t))}, args...)...)
			if err == nil || !strings.Contains(err.Error(), "is not running: nothing answers at") {
				t.Errorf("err = %v, want the reason", err)
			}
		})
	}
}

// TestRefusedTokenNeverSaysNotRunning is #1024: an API gateway that refuses
// the data dir's Admin token may be a Live Instance, so no command says the
// Instance is not running.
func TestRefusedTokenNeverSaysNotRunning(t *testing.T) {
	const want = `cannot tell whether instance "default" is running: the API gateway at `
	gw := instanceAPIGateway(t, "mine", "other")
	t.Run("admin token", func(t *testing.T) {
		out, err := gcpemu(t, "--data-dir", dataDir(t, gw), "admin", "token")
		if err == nil || strings.Contains(out, "kept") {
			t.Fatalf("admin token = %q, %v, want no token and an error", out, err)
		}
		if !strings.Contains(err.Error(), want) || strings.Contains(err.Error(), "is not running") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("status", func(t *testing.T) {
		_, err := gcpemu(t, "--data-dir", dataDir(t, gw), "status")
		if err == nil || !strings.Contains(err.Error(), want) || strings.Contains(err.Error(), "is not running") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("doctor", func(t *testing.T) {
		out, _ := gcpemu(t, "--data-dir", dataDir(t, gw), "doctor")
		if !strings.Contains(out, want) || strings.Contains(out, "is not running") {
			t.Errorf("doctor:\n%s", out)
		}
	})
}

// TestCARotateAsksTheInstance is #1024: only a Live Instance rotates through
// the Admin API, and a CA that may be in use is never rotated on disk. With
// no Live Instance the CA is this data dir's own and nothing is using it.
func TestCARotateAsksTheInstance(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // rotating updates browser databases there
	const live, onDisk, fails = "running instance", "on next start", ""
	dropsPost := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			panic(http.ErrAbortHandler) // the Instance went away after the check
		}
		_, _ = io.WriteString(w, `{"id":"mine"}`)
	}))
	defer dropsPost.Close()
	theirs, theirsActed := countingAPIGateway(t, "theirs", "")
	refused, refusedActed := countingAPIGateway(t, "mine", "other")
	mine, mineActed := countingAPIGateway(t, "mine", "kept")
	for name, tt := range map[string]struct {
		gw, want  string
		acted     *atomic.Int64
		wantActed int64
	}{
		"Live Instance":      {gw: mine, want: live, acted: mineActed, wantActed: 1},
		"no endpoints.json":  {want: onDisk},
		"nothing answers":    {gw: deadAPIGateway(t), want: onDisk},
		"another Instance":   {gw: theirs, want: onDisk, acted: theirsActed},
		"no Instance":        {gw: notAnInstance(t), want: onDisk},
		"token refused":      {gw: refused, want: fails, acted: refusedActed},
		"Live, then no POST": {gw: strings.TrimPrefix(dropsPost.URL, "http://"), want: fails},
	} {
		t.Run(name, func(t *testing.T) {
			dir := dataDir(t, tt.gw)
			if tt.gw == "" {
				if err := os.Remove(filepath.Join(dir, instance.EndpointsFile)); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := ca.Load(dir, "default"); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(filepath.Join(dir, ca.CertFile))
			if err != nil {
				t.Fatal(err)
			}
			out, err := gcpemu(t, "--data-dir", dir, "ca", "rotate")
			after, readErr := os.ReadFile(filepath.Join(dir, ca.CertFile))
			if readErr != nil {
				t.Fatal(readErr)
			}
			if rotated := !bytes.Equal(before, after); rotated != (tt.want == onDisk) {
				t.Errorf("rotated on disk = %v, want %v", rotated, tt.want == onDisk)
			}
			if tt.want == fails {
				if err == nil {
					t.Errorf("ca rotate = %q, want an error", out)
				}
			} else if err != nil || !strings.Contains(out, tt.want) {
				t.Errorf("ca rotate = %q, %v, want it to say %q", out, err, tt.want)
			}
			if tt.acted != nil && tt.acted.Load() != tt.wantActed {
				t.Errorf("%d requests after the check, want %d", tt.acted.Load(), tt.wantActed)
			}
		})
	}
}

// fakeChild is a detached child that stands in for os.Process.
type fakeChild struct {
	signalErr error
	killed    bool
}

func (c *fakeChild) Signal(os.Signal) error { return c.signalErr }
func (c *fakeChild) Kill() error            { c.killed = true; return nil }

// TestStopChildAsksOnlyItsOwnInstance is #1024: where a child takes no
// signal, as on Windows, `start --detach` asks it to shut down only when
// it is the data dir's Live Instance, and otherwise kills it.
func TestStopChildAsksOnlyItsOwnInstance(t *testing.T) {
	noSignals := errors.New("not supported by windows")
	mine, mineActed := countingAPIGateway(t, "mine", "kept")
	theirs, theirsActed := countingAPIGateway(t, "theirs", "")
	refuses := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			http.Error(w, "no", http.StatusInternalServerError)
			return
		}
		_, _ = fmt.Fprintf(w, `{"id":"mine","pid":%d}`, childPID)
	}))
	defer refuses.Close()
	for name, tt := range map[string]struct {
		gw         string
		acted      *atomic.Int64
		pid        int
		signalErr  error
		wantActed  int64
		wantKilled bool
	}{
		"signalled":        {gw: mine, acted: mineActed, pid: childPID},
		"own Instance":     {gw: mine, acted: mineActed, pid: childPID, signalErr: noSignals, wantActed: 1},
		"another process":  {gw: mine, acted: mineActed, pid: childPID + 1, signalErr: noSignals, wantKilled: true},
		"another Instance": {gw: theirs, acted: theirsActed, pid: childPID, signalErr: noSignals, wantKilled: true},
		"nothing answers":  {gw: deadAPIGateway(t), pid: childPID, signalErr: noSignals, wantKilled: true},
		"shutdown refused": {gw: strings.TrimPrefix(refuses.URL, "http://"), pid: childPID, signalErr: noSignals, wantKilled: true},
	} {
		t.Run(name, func(t *testing.T) {
			if tt.acted != nil {
				tt.acted.Store(0)
			}
			cfg := config.Defaults()
			cfg.DataDir = dataDir(t, tt.gw)
			child := &fakeChild{signalErr: tt.signalErr}
			stopChild(&cfg, child, tt.pid)
			if child.killed != tt.wantKilled {
				t.Errorf("killed = %v, want %v", child.killed, tt.wantKilled)
			}
			if tt.acted != nil && tt.acted.Load() != tt.wantActed {
				t.Errorf("%d shutdown requests, want %d", tt.acted.Load(), tt.wantActed)
			}
		})
	}
}
