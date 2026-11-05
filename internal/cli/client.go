package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

func clientFor(cfg *config.Config) (*adminClient, error) {
	eps, err := readEndpoints(cfg)
	if err != nil {
		return nil, err
	}
	if eps["gateway"] == "" {
		return nil, fmt.Errorf("no API gateway address recorded")
	}
	return newAdminClient(eps, readAdminToken(cfg)), nil
}

// liveCheck is what asking a data dir's Instance found.
type liveCheck struct {
	// client reaches the Live Instance; nil when there is none.
	client *adminClient
	// err says there is no Live Instance.
	err error
	// reason is what was found in its place, when leftBehind.
	reason error
	// leftBehind reports an endpoints.json with no Live Instance behind it.
	leftBehind bool
	// tokenRefused reports an API gateway that refused the data dir's Admin
	// token, so it wouldn't say which Instance it serves. That is almost
	// always another Instance, as a Live Instance's token is in the data dir.
	tokenRefused bool
}

// checkLive asks whether cfg's data dir has a Live Instance: endpoints.json
// proves nothing, as a crash leaves it behind (#1018). It asks as `stop`
// does, with the Instance ID, because the file's port may have another
// Instance on it (ADR 0064).
func checkLive(cfg *config.Config) liveCheck {
	notLive := func(reason error) liveCheck {
		return liveCheck{err: fmt.Errorf("instance %q is not running: %w", cfg.Instance, reason), reason: reason, leftBehind: true}
	}
	c, err := clientFor(cfg)
	if err != nil {
		if _, statErr := os.Stat(filepath.Join(cfg.InstanceDir(), instance.EndpointsFile)); statErr != nil {
			return liveCheck{err: err}
		}
		return notLive(err)
	}
	addr := strings.TrimPrefix(c.base, "http://")
	_, err = instance.Identify(func(method, path string) (int, []byte, error) {
		code, b, err := c.do(method, path)
		if err != nil {
			return 0, nil, fmt.Errorf("nothing answers at %s", addr)
		}
		return code, b, nil
	}, instance.ReadID(cfg.InstanceDir()), 0)
	if err == nil {
		return liveCheck{client: c}
	}
	var unidentified *instance.UnidentifiedError
	if !errors.As(err, &unidentified) {
		return notLive(err)
	}
	if unidentified.Code == http.StatusUnauthorized {
		live := notLive(fmt.Errorf("the API gateway at %s refused this data dir's Admin token", addr))
		live.tokenRefused = true
		return live
	}
	return notLive(fmt.Errorf("what answers at %s did not say which Instance it serves", addr))
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
