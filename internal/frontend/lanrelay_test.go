package frontend_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/linuxuser586/gcpemu/internal/ca"
	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/edgeagent"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/frontend"
	"github.com/linuxuser586/gcpemu/internal/lanbind"
)

// fakeRelay records what the Google front end asks the LAN front-end
// relay to listen on and where it relays to.
type fakeRelay struct {
	mu      sync.Mutex
	ingress string
	addrs   []string
	closed  bool
}

func (r *fakeRelay) Set(_ context.Context, ingress string, addrs []string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ingress, r.addrs = ingress, append([]string(nil), addrs...)
	return nil
}

func (r *fakeRelay) Close(context.Context) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
}

func (r *fakeRelay) state() (string, []string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ingress, r.addrs, r.closed
}

// logBuffer collects log lines from several goroutines.
type logBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// refuse443 binds like an unprivileged process on Linux with
// ip_unprivileged_port_start above 443.
func refuse443(network, addr string) (net.Listener, error) {
	if strings.HasSuffix(addr, ":443") {
		return nil, &net.OpError{Op: "listen", Net: network, Err: os.NewSyscallError("bind", syscall.EACCES)}
	}
	return net.Listen(network, addr)
}

// relayCase is a front end on 127.0.0.1 whose bind of port 443 is refused.
type relayCase struct {
	env   *emu.Env
	ca    *ca.CA
	fe    *frontend.Frontend
	relay *fakeRelay
	logs  *logBuffer
	// gateway counts the requests the gateway stub served.
	gateway atomic.Int64
}

// startRelayCase starts the Google front end with hooks; edit, if set,
// changes them and the configuration first.
func startRelayCase(t *testing.T, edit func(*config.Config, *frontend.LANHooks)) *relayCase {
	t.Helper()
	authority, err := ca.Load(t.TempDir(), "lan")
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	cfg.GFEBind = []string{"127.0.0.1"}
	rc := &relayCase{ca: authority, relay: &fakeRelay{}, logs: &logBuffer{}}
	rc.env = &emu.Env{Config: &cfg, CA: authority, Auth: tokenAuth{}, Endpoints: emu.NewEndpoints(), Log: slog.New(slog.NewTextHandler(rc.logs, nil))}
	hooks := frontend.LANHooks{
		GOOS:    "linux",
		Listen:  refuse443,
		Runtime: func(context.Context) (bool, error) { return false, nil },
		Relay:   func() frontend.LANRelay { return rc.relay },
		Retry:   []time.Duration{time.Millisecond, time.Millisecond},
	}
	if edit != nil {
		edit(&cfg, &hooks)
	}
	rc.fe = frontend.New(rc.env, lanNames)
	frontend.SetLANHooks(rc.fe, hooks)
	lan := lanStubs()
	gw := lan.Gateway
	lan.Gateway = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rc.gateway.Add(1)
		gw.ServeHTTP(w, r)
	})
	rc.fe.StartLAN(lan)
	t.Cleanup(func() { rc.fe.Close() })
	return rc
}

// waitFor fails t unless cond holds within 5s.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !cond(); {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// lanClient is the source address a relayed LAN client appears to have.
var lanClient = &net.TCPAddr{IP: net.ParseIP("192.0.2.10"), Port: 51234}

// relayed dials the relay's ingress as the gfe-lan container does: a
// PROXY v2 header for lanClient → 127.0.0.1:443, then the client's bytes.
func relayed(ingress string) func(ctx context.Context, network, _ string) (net.Conn, error) {
	return func(ctx context.Context, network, _ string) (net.Conn, error) {
		var d net.Dialer
		c, err := d.DialContext(ctx, "tcp", ingress)
		if err != nil {
			return nil, err
		}
		if err := edgeagent.WriteProxyV2(c, lanClient, &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 443}); err != nil {
			c.Close()
			return nil, err
		}
		return c, nil
	}
}

// TestLANRelay covers #469: when the emulator may not bind port 443 on a
// --gfe-bind address, the LAN front-end relay listens there and relays to
// a loopback ingress, where every request gets the checks a direct
// --gfe-bind listener applies, and the handler sees the LAN client's
// address.
func TestLANRelay(t *testing.T) {
	rc := startRelayCase(t, nil)
	waitFor(t, "the gfe-lan endpoint", func() bool { return rc.env.Endpoints.Get(frontend.LANEndpoint) != "" })
	if got := rc.env.Endpoints.Get(frontend.LANEndpoint); got != "127.0.0.1:443" {
		t.Fatalf("gfe-lan endpoint = %q, want 127.0.0.1:443", got)
	}
	ingress, addrs, _ := rc.relay.state()
	if strings.Join(addrs, ",") != "127.0.0.1:443" {
		t.Fatalf("relay listens on %v, want [127.0.0.1:443]", addrs)
	}
	if !strings.HasPrefix(ingress, "127.0.0.1:") {
		t.Fatalf("relay ingress %q, want one on 127.0.0.1", ingress)
	}
	if logs := rc.logs.String(); !strings.Contains(logs, `msg="Google front end exposed" address=127.0.0.1:443 mode=relay`) {
		t.Errorf("no relay exposure logged:\n%s", logs)
	}
	hc := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		DialContext:       relayed(ingress),
		TLSClientConfig:   &tls.Config{RootCAs: rc.ca.Pool()},
		ForceAttemptHTTP2: true,
	}}
	checkLAN(t, hc)

	req, _ := http.NewRequest("GET", "https://storage.googleapis.com/storage/v1/b/ours/o/x", nil)
	req.Header.Set("Authorization", "Bearer good")
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got := resp.Header.Get("X-Remote-Addr"); got != lanClient.String() {
		t.Errorf("handler saw client %q, want the PROXY header's %s", got, lanClient)
	}

	rc.fe.Close()
	if _, _, closed := rc.relay.state(); !closed {
		t.Error("relay not closed with the front end")
	}
}

// TestLANRelayRequiresProxyHeader: a connection to the ingress that does
// not start with a PROXY v2 header is closed before TLS.
func TestLANRelayRequiresProxyHeader(t *testing.T) {
	rc := startRelayCase(t, nil)
	waitFor(t, "the relay", func() bool { in, _, _ := rc.relay.state(); return in != "" })
	ingress, _, _ := rc.relay.state()
	c, err := net.DialTimeout("tcp", ingress, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(15 * time.Second))
	tc := tls.Client(c, &tls.Config{RootCAs: rc.ca.Pool(), ServerName: "storage.googleapis.com"})
	if err := tc.Handshake(); err == nil {
		t.Fatal("TLS handshake without a PROXY header succeeded")
	} else if errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatal("connection without a PROXY header left open")
	}
	if n := rc.gateway.Load(); n != 0 {
		t.Errorf("gateway served %d requests", n)
	}
}

// TestLANRelayRetriesRuntime: a transient failure of the first runtime
// check is tried again before the address is skipped.
func TestLANRelayRetriesRuntime(t *testing.T) {
	var calls atomic.Int64
	rc := startRelayCase(t, func(_ *config.Config, h *frontend.LANHooks) {
		h.Runtime = func(context.Context) (bool, error) {
			if calls.Add(1) == 1 {
				return false, errors.New("runtime socket not ready")
			}
			return false, nil
		}
	})
	waitFor(t, "the gfe-lan endpoint", func() bool { return rc.env.Endpoints.Get(frontend.LANEndpoint) == "127.0.0.1:443" })
}

// TestLANNoRelay: without a usable non-VM runtime, inside a container,
// off Linux, or with --port gfe-lan set, a refused bind is skipped with
// the sysctl advice as before, and no relay is made; nor is one when 443
// can be bound.
func TestLANNoRelay(t *testing.T) {
	const advice = "sudo sysctl net.ipv4.ip_unprivileged_port_start=443"
	for _, tc := range []struct {
		name   string
		edit   func(*config.Config, *frontend.LANHooks)
		direct bool // served directly instead
	}{
		{name: "443 bindable", direct: true, edit: func(_ *config.Config, h *frontend.LANHooks) {
			h.Listen = func(network, addr string) (net.Listener, error) {
				return net.Listen(network, strings.TrimSuffix(addr, ":443")+":0")
			}
		}},
		{name: "VM runtime", edit: func(_ *config.Config, h *frontend.LANHooks) {
			h.Runtime = func(context.Context) (bool, error) { return true, nil }
		}},
		{name: "no runtime", edit: func(_ *config.Config, h *frontend.LANHooks) {
			h.Runtime = func(context.Context) (bool, error) { return false, emu.ErrNoRuntime }
		}},
		{name: "runtime never reachable", edit: func(_ *config.Config, h *frontend.LANHooks) {
			h.Runtime = func(context.Context) (bool, error) { return false, errors.New("runtime socket not ready") }
		}},
		{name: "in a container", edit: func(_ *config.Config, h *frontend.LANHooks) { h.InContainer = true }},
		{name: "macOS", edit: func(_ *config.Config, h *frontend.LANHooks) { h.GOOS = "darwin" }},
		{name: "--port gfe-lan", edit: func(c *config.Config, h *frontend.LANHooks) {
			c.Ports["gfe-lan"] = 8443
			h.Listen = func(network, _ string) (net.Listener, error) {
				return nil, &net.OpError{Op: "listen", Net: network, Err: os.NewSyscallError("bind", syscall.EACCES)}
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var made atomic.Bool
			rc := startRelayCase(t, func(c *config.Config, h *frontend.LANHooks) {
				tc.edit(c, h)
				relay := h.Relay
				h.Relay = func() frontend.LANRelay { made.Store(true); return relay() }
			})
			if tc.direct {
				if got := rc.env.Endpoints.Get(frontend.LANEndpoint); !strings.HasPrefix(got, "127.0.0.1:") {
					t.Fatalf("gfe-lan endpoint = %q", got)
				}
				if logs := rc.logs.String(); !strings.Contains(logs, "mode=direct") {
					t.Errorf("no direct exposure logged:\n%s", logs)
				}
			} else {
				waitFor(t, "the warning", func() bool { return strings.Contains(rc.logs.String(), advice) })
				if got := rc.env.Endpoints.Get(frontend.LANEndpoint); got != "" {
					t.Errorf("gfe-lan endpoint = %q, want none", got)
				}
			}
			rc.fe.Close()
			if made.Load() {
				t.Error("a relay was made")
			}
		})
	}
}

// TestLANNotLocal is #497: a --gfe-bind address that isn't the Instance's
// is skipped; in a container the warning names the cause and the fix
// instead of the privileged port, and on the host it is unchanged.
func TestLANNotLocal(t *testing.T) {
	notLocal := func(network, addr string) (net.Listener, error) {
		return nil, &net.OpError{Op: "listen", Net: network, Err: os.NewSyscallError("bind", errNotLocal)}
	}
	for _, tc := range []struct {
		where       lanbind.Where
		want, avoid string
	}{
		{lanbind.Bridge, "not an address of this container", "ip_unprivileged_port_start"},
		{lanbind.Unconfirmed, "may not be an address of this container", "ip_unprivileged_port_start"},
		{lanbind.Host, "ip_unprivileged_port_start", "container"},
	} {
		authority, err := ca.Load(t.TempDir(), "lan")
		if err != nil {
			t.Fatal(err)
		}
		cfg := config.Defaults()
		cfg.GFEBind = []string{"192.0.2.1"}
		logs := &logBuffer{}
		env := &emu.Env{Config: &cfg, CA: authority, Auth: tokenAuth{}, Endpoints: emu.NewEndpoints(), Log: slog.New(slog.NewTextHandler(logs, nil)),
			LANBindWhere: func(context.Context) lanbind.Where { return tc.where }}
		fe := frontend.New(env, lanNames)
		frontend.SetLANHooks(fe, frontend.LANHooks{GOOS: "linux", InContainer: tc.where != lanbind.Host, Listen: notLocal})
		fe.StartLAN(lanStubs())
		fe.Close()
		log := logs.String()
		if !strings.Contains(log, "level=WARN") || !strings.Contains(log, tc.want) || strings.Contains(log, tc.avoid) {
			t.Errorf("%v: want %q and not %q in:\n%s", tc.where, tc.want, tc.avoid, log)
		}
		if tc.where != lanbind.Host && !strings.Contains(log, "--network host") {
			t.Errorf("%v: log lacks the fix:\n%s", tc.where, log)
		}
		if env.Endpoints.Get(frontend.LANEndpoint) != "" {
			t.Errorf("%v: %s served", tc.where, frontend.LANEndpoint)
		}
	}
}
