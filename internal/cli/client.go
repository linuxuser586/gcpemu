package cli

import (
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
	if err != nil {
		return nil, fmt.Errorf("instance %q is not running (no %s in %s)", cfg.Instance, instance.EndpointsFile, cfg.InstanceDir())
	}
	m := map[string]string{}
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return m, nil
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
// proves nothing about what answers there. Only checkLive may use it.
func endpointsClient(cfg *config.Config) (*adminClient, error) {
	eps, err := readEndpoints(cfg)
	if err != nil {
		return nil, err
	}
	if eps["gateway"] == "" {
		return nil, fmt.Errorf("no API gateway address recorded")
	}
	return newAdminClient(eps, readAdminToken(cfg)), nil
}

// liveCheckDialTimeout is how long the check waits for a connection. It is
// shorter than liveCheckTimeout because only a dial that fails says that
// nothing is there: an address that drops packets would otherwise time out
// just as a wedged Instance does (ADR 0066).
const liveCheckDialTimeout = 3 * time.Second

// liveCheckTimeout is how long an answer to the check may take (a variable
// for tests).
var liveCheckTimeout = 10 * time.Second

// liveCheckTransport is the check's own, for liveCheckDialTimeout. One is
// shared as `start --detach` polls the check.
var liveCheckTransport = func() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.DialContext = (&net.Dialer{Timeout: liveCheckDialTimeout}).DialContext
	return t
}()

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
	// stateLeftBehind is an endpoints.json with no Live Instance behind it.
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
// stops. The PID is for a person to check: no command acts on the PID file
// (ADR 0064).
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
		return leftBehind(err)
	}
	addr := strings.TrimPrefix(c.base, "http://")
	asker := *c
	asker.http = &http.Client{Timeout: liveCheckTimeout, Transport: liveCheckTransport}
	var lost error // a connection that formed and gave no answer
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
		var timeout net.Error
		if errors.As(lost, &timeout) && timeout.Timeout() {
			return unconfirmed("the API gateway at %s did not answer in time", addr)
		}
		return unconfirmed("the API gateway at %s dropped the connection", addr)
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
	return leftBehind(fmt.Errorf("what answers at %s did not say which Instance it serves", addr))
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
