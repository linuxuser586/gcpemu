package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/gateway"
	"github.com/linuxuser586/gcpemu/internal/instance"
)

// adminClient talks to a running instance's admin API.
type adminClient struct {
	base      string
	endpoints map[string]string
	http      *http.Client
	// token is the Admin token, "" when the instance is on loopback.
	token string
}

// readAdminToken returns the running instance's Admin token: from its
// instance dir, else GCPEMU_ADMIN_TOKEN, else "" (FR-UI-009).
func readAdminToken(cfg *config.Config) string {
	if b, err := os.ReadFile(filepath.Join(cfg.InstanceDir(), instance.AdminTokenFile)); err == nil {
		return strings.TrimSpace(string(b))
	}
	return os.Getenv("GCPEMU_ADMIN_TOKEN")
}

func readEndpoints(cfg *config.Config) (map[string]string, error) {
	b, err := os.ReadFile(filepath.Join(cfg.InstanceDir(), instance.EndpointsFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("instance %q has no %s in %s", cfg.Instance, instance.EndpointsFile, cfg.InstanceDir())
	}
	if err != nil {
		return nil, unreadableEndpoints(cfg, err)
	}
	m := map[string]string{}
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, unreadableEndpoints(cfg, err)
	}
	return m, nil
}

func unreadableEndpoints(cfg *config.Config, reason error) error {
	return fmt.Errorf("instance %q has an unreadable %s: %w", cfg.Instance, instance.EndpointsFile, reason)
}

// clientFor returns a client for cfg's Live Instance, and otherwise the
// check's error, so that no command acts on whatever took the port in a
// left-behind endpoints.json (#1024). A command that needs more than that
// error calls checkLive; none goes around the check.
func clientFor(cfg *config.Config) (*adminClient, error) {
	live := checkLive(cfg)
	return live.client, live.err
}

// endpointsClient returns a client for the address in endpoints.json, which
// proves nothing about what answers there. Only checkLive may use it. An
// address no request can go to is an unreadable file, as nothing can be asked.
func endpointsClient(cfg *config.Config) (*adminClient, error) {
	eps, err := readEndpoints(cfg)
	if err != nil {
		return nil, err
	}
	if eps["gateway"] == "" {
		return nil, fmt.Errorf("instance %q has no API gateway address in %s", cfg.Instance, instance.EndpointsFile)
	}
	c := newAdminClient(eps, readAdminToken(cfg))
	u, err := url.Parse(c.base)
	if err != nil {
		return nil, unreadableEndpoints(cfg, err)
	}
	if u.Hostname() == "" {
		return nil, unreadableEndpoints(cfg, fmt.Errorf("no host in API gateway address %q", eps["gateway"]))
	}
	return c, nil
}

// liveCheckDialTimeout is how long the check waits for a connection. It is
// shorter than liveCheckTimeout because only a dial that fails says that
// nothing is there: an address that drops packets would otherwise time out
// just as an Instance that connects and never answers does (ADR 0066).
const liveCheckDialTimeout = 3 * time.Second

// liveCheckTimeout is how long an answer to the check may take (a variable
// for tests).
var liveCheckTimeout = 10 * time.Second

// liveCheckTransport is the check's own, for liveCheckDialTimeout. One is
// shared as `start --detach` polls the check.
var liveCheckTransport = func() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	dial := (&net.Dialer{Timeout: liveCheckDialTimeout}).DialContext
	t.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		c, err := dial(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		return newCheckConn(c), nil
	}
	return t
}()

// errNotHTTP and errUnasked are what a checkConn fails a read with. net/http
// has no type for a server that isn't HTTP, and when one speaks before the
// request goes out it logs that and reports a broken connection (ADR 0066).
var (
	errNotHTTP = errors.New("first bytes are not HTTP")
	errUnasked = errors.New("answered before the request went out")
)

// httpPrefix is how every HTTP/1.x answer begins.
const httpPrefix = "HTTP/"

// checkConn judges what a server sends first, so that what the check finds
// does not depend on whether that beats the request. It fails the read rather
// than passing the bytes on, as net/http logs what it wasn't expecting.
type checkConn struct {
	net.Conn
	// asked is set before the request goes out, as the answer can beat a
	// flag set afterwards.
	asked atomic.Bool
	// seen is how much of httpPrefix has arrived. net/http reads a
	// connection from one goroutine.
	seen int
	// verdict is the read's errNotHTTP or errUnasked, once it gave one.
	// net/http closes the connection then, and may report the request's
	// write failing on that rather than the read (#1162).
	verdict atomic.Pointer[error]
}

func newCheckConn(c net.Conn) net.Conn { return &checkConn{Conn: c} }

// judge records err as the verdict and returns it.
func (c *checkConn) judge(err error) error {
	c.verdict.CompareAndSwap(nil, &err)
	return err
}

// Write reports the read's verdict in place of its own failure, so the check
// finds the same thing whichever side net/http reports.
func (c *checkConn) Write(b []byte) (int, error) {
	c.asked.Store(true)
	if v := c.verdict.Load(); v != nil {
		return 0, *v
	}
	n, err := c.Conn.Write(b)
	if v := c.verdict.Load(); err != nil && v != nil {
		return n, *v
	}
	return n, err
}

func (c *checkConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if n == 0 || c.seen == len(httpPrefix) {
		return n, err
	}
	first := b[:min(n, len(httpPrefix)-c.seen)]
	if !strings.HasPrefix(httpPrefix[c.seen:], string(first)) {
		return 0, c.judge(errNotHTTP)
	}
	if !c.asked.Load() {
		return 0, c.judge(errUnasked)
	}
	c.seen += len(first)
	return n, err
}

// liveState is what asking a data dir's Instance settled. The zero value
// rules nothing out.
type liveState int

const (
	// stateUnconfirmed is an Unconfirmed Instance: what answered neither
	// confirms a Live Instance nor rules one out.
	stateUnconfirmed liveState = iota
	// stateLive is a Live Instance.
	stateLive
	// stateNoEndpoints is a data dir with no endpoints.json.
	stateNoEndpoints
	// stateLeftBehind is an endpoints.json with no Live Instance behind it,
	// or one unreadable or with no usable address.
	stateLeftBehind
)

// liveCheck is what asking a data dir's Instance found.
type liveCheck struct {
	state liveState
	// client reaches the Live Instance; nil for any other state.
	client *adminClient
	// err says why no Live Instance was confirmed.
	err error
}

// ruledOut reports that cfg's data dir has no Live Instance, so a command may
// do without one (ADR 0066).
func (l liveCheck) ruledOut() bool {
	return l.state == stateNoEndpoints || l.state == stateLeftBehind
}

// refusal is err with the way out, for a command an Unconfirmed Instance
// stops. The PID is for a person to check: the check never acts on it
// (ADR 0066).
func (l liveCheck) refusal(cfg *config.Config) error {
	endpoints := filepath.Join(cfg.InstanceDir(), instance.EndpointsFile)
	if b, err := os.ReadFile(filepath.Join(cfg.InstanceDir(), instance.PIDFile)); err == nil {
		if pid := strings.TrimSpace(string(b)); pid != "" {
			return fmt.Errorf("%w; if PID %s is not a gcpemu using this data dir, remove %s", l.err, pid, endpoints)
		}
	}
	return fmt.Errorf("%w; if nothing is using this data dir, remove %s", l.err, endpoints)
}

// checkLive asks whether cfg's data dir has a Live Instance: endpoints.json
// proves nothing, as a crash leaves it behind (#1018). It asks as `stop`
// does, with the Instance ID, because the file's port may have another
// Instance on it (ADR 0064).
func checkLive(cfg *config.Config) liveCheck {
	leftBehind := func(reason error) liveCheck {
		return liveCheck{state: stateLeftBehind, err: fmt.Errorf("instance %q is not running: %w", cfg.Instance, reason)}
	}
	unconfirmed := func(format string, args ...any) liveCheck {
		return liveCheck{state: stateUnconfirmed, err: fmt.Errorf("cannot tell whether instance %q is running: %s", cfg.Instance, fmt.Sprintf(format, args...))}
	}
	c, err := endpointsClient(cfg)
	if err != nil {
		if _, statErr := os.Stat(filepath.Join(cfg.InstanceDir(), instance.EndpointsFile)); statErr != nil {
			return liveCheck{state: stateNoEndpoints, err: err}
		}
		return liveCheck{state: stateLeftBehind, err: err}
	}
	addr := strings.TrimPrefix(c.base, "http://")
	noInstance := leftBehind(fmt.Errorf("what answers at %s did not say which Instance it serves", addr))
	asker := *c
	asker.http = &http.Client{Timeout: liveCheckTimeout, Transport: liveCheckTransport}
	var lost error // why asking gave no answer
	_, err = instance.Identify(func(method, path string) (int, []byte, error) {
		code, b, err := asker.do(method, path)
		if err == nil {
			return code, b, nil
		}
		var op *net.OpError
		if errors.As(err, &op) && op.Op == "dial" {
			return 0, nil, fmt.Errorf("nothing answers at %s", addr)
		}
		lost = err
		return 0, nil, err
	}, instance.ReadID(cfg.InstanceDir()), 0)
	if lost != nil {
		switch {
		case errors.Is(lost, errNotHTTP):
			return noInstance
		case errors.Is(lost, errUnasked):
			return unconfirmed("the API gateway at %s answered before it was asked", addr)
		}
		var timeout net.Error
		if errors.As(lost, &timeout) && timeout.Timeout() {
			return unconfirmed("the API gateway at %s did not answer in time", addr)
		}
		for _, drop := range []error{io.EOF, io.ErrUnexpectedEOF, syscall.ECONNRESET, syscall.EPIPE} {
			if errors.Is(lost, drop) {
				return unconfirmed("the API gateway at %s dropped the connection", addr)
			}
		}
		// Not a drop: a dead proxy or a bad Admin token never reached it.
		var failed *url.Error
		if errors.As(lost, &failed) {
			lost = failed.Err
		}
		return unconfirmed("the API gateway at %s gave no answer: %v", addr, lost)
	}
	if err == nil {
		return liveCheck{state: stateLive, client: c}
	}
	var unidentified *instance.UnidentifiedError
	switch {
	case !errors.As(err, &unidentified):
		return leftBehind(err)
	case unidentified.Code == http.StatusUnauthorized:
		// Almost always another Instance, as a Live Instance's token is in the
		// data dir, but it can be a Live Instance.
		return unconfirmed("the API gateway at %s refused this data dir's Admin token", addr)
	case unidentified.Code >= http.StatusInternalServerError:
		return unconfirmed("the API gateway at %s answered %d", addr, unidentified.Code)
	}
	return noInstance
}

// newAdminClient returns a client for the Admin API on the gateway in eps,
// which an older gcpemu may have recorded as an unspecified address.
func newAdminClient(eps map[string]string, token string) *adminClient {
	return &adminClient{base: "http://" + emu.Dialable(eps["gateway"]), endpoints: eps, http: &http.Client{Timeout: 10 * time.Second}, token: token}
}

// newRequest builds an admin API request carrying the Admin token.
func (c *adminClient) newRequest(method, path string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequest(method, c.base+path, body)
	if err != nil {
		return nil, err
	}
	if c.token != "" {
		req.Header.Set(gateway.AdminTokenHeader, c.token)
	}
	return req, nil
}

// consoleURL is the Web console's URL; with an Admin token it carries the
// token, which the console swaps for a cookie on first load.
func (c *adminClient) consoleURL() string {
	return consoleURL(strings.TrimPrefix(c.base, "http://"), c.token)
}

// consoleURL is the Web console's URL on the gateway at addr. An
// unspecified address (--bind 0.0.0.0) becomes 127.0.0.1, which browsers
// open; from another machine, use this host's name instead.
func consoleURL(addr, token string) string {
	u := "http://" + emu.Dialable(addr) + "/console/"
	if token != "" {
		u += "?" + gateway.AdminTokenQuery + "=" + url.QueryEscape(token)
	}
	return u
}

func (c *adminClient) do(method, path string) (int, []byte, error) {
	req, err := c.newRequest(method, path, nil)
	if err != nil {
		return 0, nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return resp.StatusCode, b, err
}

func (c *adminClient) get(path string) ([]byte, error) {
	code, b, err := c.do(http.MethodGet, path)
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %d %s", path, code, b)
	}
	return b, nil
}
