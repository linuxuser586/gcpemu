package cli

import (
	"bufio"
	"bytes"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/linuxuser586/gcpemu/internal/ca"
	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/instance"
)

// troubledAPIGateways returns API gateways that connect and rule nothing out,
// by name, with the reason each gives. A stall waits out liveCheckTimeout.
func troubledAPIGateways(t *testing.T) map[string]struct{ gw, want string } {
	t.Helper()
	old := liveCheckTimeout
	liveCheckTimeout = 200 * time.Millisecond
	t.Cleanup(func() { liveCheckTimeout = old })
	apiGateway := func(h http.HandlerFunc) string {
		srv := httptest.NewServer(h)
		t.Cleanup(srv.Close)
		return strings.TrimPrefix(srv.URL, "http://")
	}
	release := make(chan struct{})
	stalled := apiGateway(func(http.ResponseWriter, *http.Request) { <-release })
	t.Cleanup(func() { close(release) })
	dropped := apiGateway(func(http.ResponseWriter, *http.Request) { panic(http.ErrAbortHandler) })
	failing := apiGateway(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	return map[string]struct{ gw, want string }{
		"token refused": {instanceAPIGateway(t, "mine", "other"), "refused this data dir's Admin token"},
		"stalled":       {stalled, "did not answer in time"},
		"dropped":       {dropped, "dropped the connection"},
		"5xx":           {failing, "answered 500"},
	}
}

func refuseRunningIn(dir string) error {
	cfg := config.Defaults()
	cfg.DataDir = dir
	return refuseRunning(&cfg)
}

// TestUnconfirmedInstance is ADR 0066: an API gateway that connects and rules
// nothing out leaves no command saying the Instance is not running, and
// refuses what may be done to a Live Instance's data dir.
func TestUnconfirmedInstance(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // rotating updates browser databases there
	const cantTell = `cannot tell whether instance "default" is running: `
	for name, tt := range troubledAPIGateways(t) {
		t.Run(name, func(t *testing.T) {
			dir := dataDir(t, tt.gw)
			endpoints := filepath.Join(dir, instance.EndpointsFile)
			hasWayOut := func(err error, condition string) bool {
				return err != nil && strings.Contains(err.Error(), condition) && strings.Contains(err.Error(), "remove "+endpoints)
			}

			_, err := gcpemu(t, "--data-dir", dir, "reset")
			if err == nil || !strings.Contains(err.Error(), cantTell) || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("reset: err = %v, want it to say %q and %q", err, cantTell, tt.want)
			}

			if err := refuseRunningIn(dir); !hasWayOut(err, "if nothing is using this data dir") {
				t.Errorf("refuseRunning: err = %v, want the way out", err)
			}
			writeDataFile(t, dir, instance.PIDFile, "1234")
			if err := refuseRunningIn(dir); !hasWayOut(err, "if PID 1234 is not a gcpemu using this data dir") {
				t.Errorf("refuseRunning: err = %v, want the way out naming the PID", err)
			}

			if _, err := ca.Load(dir, "default"); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(filepath.Join(dir, ca.CertFile))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := gcpemu(t, "--data-dir", dir, "ca", "rotate"); !hasWayOut(err, "PID 1234") {
				t.Errorf("ca rotate: err = %v, want the way out", err)
			}
			if after, err := os.ReadFile(filepath.Join(dir, ca.CertFile)); err != nil || !bytes.Equal(before, after) {
				t.Errorf("ca rotate rotated on disk (read err %v)", err)
			}

			out, _ := gcpemu(t, "--data-dir", dir, "doctor")
			if !strings.Contains(out, cantTell) || !strings.Contains(out, "(gateway)") || strings.Contains(out, "is not running") {
				t.Errorf("doctor:\n%s", out)
			}

			t.Setenv("GCPEMU_PORTS", "dns=0")
			_, err = gcpemu(t, "--data-dir", dir, "dns", "install", "--dry-run")
			if err == nil || !strings.Contains(err.Error(), cantTell) || strings.Contains(err.Error(), "start the Instance first") {
				t.Errorf("dns install: err = %v, want it to say %q", err, cantTell)
			}
		})
	}
}

// wantNoInstance fails unless what answers at gw is reported as no Instance
// and rules a Live Instance out.
func wantNoInstance(t *testing.T, gw string) {
	t.Helper()
	_, err := gcpemu(t, "--data-dir", dataDir(t, gw), "reset")
	want := `instance "default" is not running: what answers at ` + gw + ` did not say which Instance it serves`
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("err = %v, want it to say %q", err, want)
	}
	if err := refuseRunningIn(dataDir(t, gw)); err != nil {
		t.Errorf("refuseRunning: %v, want it ruled out", err)
	}
}

// TestJSONWithoutAnIDIsNoInstance is ADR 0066: it is something else on the
// port, not another Instance, and it rules a Live Instance out.
func TestJSONWithoutAnIDIsNoInstance(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()
	wantNoInstance(t, strings.TrimPrefix(srv.URL, "http://"))
}

// TestNonHTTPAnswerIsNoInstance is ADR 0066: a server that answers in some
// other protocol is no Instance, though net/http calls it a broken connection.
// It answers only once it has the request: one that speaks first can race the
// client and look like a drop.
func TestNonHTTPAnswerIsNoInstance(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			if _, err := http.ReadRequest(bufio.NewReader(conn)); err == nil {
				_, _ = io.WriteString(conn, "-ERR unknown command\r\n")
			}
			_ = conn.Close()
		}
	}()
	wantNoInstance(t, ln.Addr().String())
}

// TestUnparseableAPIGatewayIsUnreadable is ADR 0066: nothing was asked, so
// the file is what is wrong, and it rules a Live Instance out.
func TestUnparseableAPIGatewayIsUnreadable(t *testing.T) {
	for _, gw := range []string{"bad host:1", "127.0.0.1:notaport"} {
		t.Run(gw, func(t *testing.T) {
			dir := dataDir(t, gw)
			_, err := gcpemu(t, "--data-dir", dir, "reset")
			const want = `instance "default" has an unreadable endpoints.json: `
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("err = %v, want it to say %q", err, want)
			}
			if err := refuseRunningIn(dir); err != nil {
				t.Errorf("refuseRunning: %v, want it ruled out", err)
			}
		})
	}
}

// Test4xxIsNoInstance is ADR 0066: an Instance answers /_emu/v1/info or
// refuses the Admin token, so anything else in the 4xx range is not one.
func Test4xxIsNoInstance(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	if err := refuseRunningIn(dataDir(t, strings.TrimPrefix(srv.URL, "http://"))); err != nil {
		t.Errorf("refuseRunning: %v, want it ruled out", err)
	}
}

// TestForegroundStartAsksTheInstance is ADR 0066: nothing locks a data dir, so
// `start` without --detach has to ask as well.
func TestForegroundStartAsksTheInstance(t *testing.T) {
	_, err := gcpemu(t, "--data-dir", dataDir(t, instanceAPIGateway(t, "mine", "kept")), "start")
	if err == nil || !strings.Contains(err.Error(), "already running") {
		t.Errorf("err = %v, want it to say already running", err)
	}
}
