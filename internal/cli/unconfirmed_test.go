package cli

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

// TestNonHTTPAnswerIsNoInstance is ADR 0066: a server that answers the request
// in some other protocol is no Instance.
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

// bannerServer returns a server that speaks on accept.
func bannerServer(t *testing.T, banner string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				_, _ = io.WriteString(conn, banner)
				_, _ = io.Copy(io.Discard, conn)
			}()
		}
	}()
	return ln.Addr().String()
}

// heldConn holds the request back until a read returns, so the answer always
// beats it.
type heldConn struct {
	net.Conn
	once sync.Once
	read chan struct{}
}

func (c *heldConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	c.once.Do(func() { close(c.read) })
	return n, err
}

func (c *heldConn) Write(b []byte) (int, error) {
	<-c.read
	return c.Conn.Write(b)
}

// holdRequests makes the check's connections heldConns for the test.
func holdRequests(t *testing.T) {
	t.Helper()
	old := liveCheckTransport
	t.Cleanup(func() { liveCheckTransport = old })
	liveCheckTransport = old.Clone()
	liveCheckTransport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		c, err := old.DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		return &heldConn{Conn: c, read: make(chan struct{})}, nil
	}
}

// captureLog returns what net/http's logging would put on a person's stderr.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return &buf
}

// TestSpeakingFirstIsNoInstance is ADR 0066: a server that speaks first is
// no Instance whichever side wins, and net/http's complaint stays off stderr.
func TestSpeakingFirstIsNoInstance(t *testing.T) {
	logged := captureLog(t)
	gw := bannerServer(t, "SSH-2.0-OpenSSH_9.6\r\n")
	for range 100 { // the race is the point: it is lost about 1 time in 300 unloaded
		wantNoInstance(t, gw)
		if t.Failed() {
			break
		}
	}
	holdRequests(t)
	wantNoInstance(t, gw)
	if logged.Len() != 0 {
		t.Errorf("logged %q", logged)
	}
}

// TestHTTPBeforeTheRequestRulesNothingOut is ADR 0066: it may be a real HTTP
// server, so it stays Unconfirmed, and the reason is the check's own words.
func TestHTTPBeforeTheRequestRulesNothingOut(t *testing.T) {
	logged := captureLog(t)
	holdRequests(t)
	gw := bannerServer(t, "HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n")
	err := refuseRunningIn(dataDir(t, gw))
	want := "the API gateway at " + gw + " answered before it was asked"
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("refuseRunning: %v, want it to say %q", err, want)
	}
	if logged.Len() != 0 {
		t.Errorf("logged %q", logged)
	}
}

// TestBadAdminTokenIsNotADrop: the request never left, so the message gives the
// cause rather than blame the API gateway. It rules nothing out.
func TestBadAdminTokenIsNotADrop(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	dir := dataDir(t, strings.TrimPrefix(srv.URL, "http://"))
	writeDataFile(t, dir, instance.AdminTokenFile, "bad\x7ftoken")
	err := refuseRunningIn(dir)
	if err == nil || !strings.Contains(err.Error(), "gave no answer: ") || strings.Contains(err.Error(), "dropped") {
		t.Errorf("refuseRunning: %v, want the cause", err)
	}
}

// TestCheckConn is the first bytes' rule, read in pieces as TCP may deliver
// them.
func TestCheckConn(t *testing.T) {
	for _, tt := range []struct {
		name   string
		asked  bool
		reads  []string
		want   error
		passed string
	}{
		{name: "HTTP answer", asked: true, reads: []string{"HTTP/1.1 200 OK\r\n"}, passed: "HTTP/1.1 200 OK\r\n"},
		{name: "split HTTP answer", asked: true, reads: []string{"HT", "TP/1.1"}, passed: "HTTP/1.1"},
		{name: "not HTTP", asked: true, reads: []string{"-ERR\r\n"}, want: errNotHTTP},
		{name: "split, not HTTP", asked: true, reads: []string{"HT", "XP/"}, want: errNotHTTP, passed: "HT"},
		{name: "banner", reads: []string{"SSH-2.0\r\n"}, want: errNotHTTP},
		{name: "unasked HTTP", reads: []string{"HTTP/1.1 200 OK\r\n"}, want: errUnasked},
		{name: "later bytes are the body's", asked: true, reads: []string{"HTTP/1.1 200 OK\r\n", "anything"}, passed: "HTTP/1.1 200 OK\r\nanything"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			client, server := net.Pipe()
			defer func() { _ = server.Close() }()
			c := newCheckConn(client)
			go func() {
				if tt.asked {
					_, _ = io.ReadFull(server, make([]byte, 1))
				}
				for _, r := range tt.reads {
					_, _ = io.WriteString(server, r)
				}
				_ = server.Close()
			}()
			if tt.asked {
				if _, err := c.Write([]byte("G")); err != nil {
					t.Fatal(err)
				}
			}
			var got bytes.Buffer
			buf := make([]byte, 64)
			var err error
			for err == nil {
				var n int
				n, err = c.Read(buf)
				got.Write(buf[:n])
			}
			if tt.want == nil && err != io.EOF || tt.want != nil && !errors.Is(err, tt.want) {
				t.Errorf("err = %v, want %v", err, tt.want)
			}
			if got.String() != tt.passed {
				t.Errorf("passed %q, want %q", got.String(), tt.passed)
			}
		})
	}
}

// TestUnparseableAPIGatewayIsUnreadable is ADR 0066: nothing was asked, so
// the file is what is wrong, and it rules a Live Instance out.
func TestUnparseableAPIGatewayIsUnreadable(t *testing.T) {
	for _, gw := range []string{"bad host:1", "127.0.0.1:notaport", "/", "/x", "?x", "#x", "@", "user@", ":"} {
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
