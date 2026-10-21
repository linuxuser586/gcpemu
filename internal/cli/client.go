package cli

import (
	"encoding/json"
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
