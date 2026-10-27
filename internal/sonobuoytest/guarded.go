package sonobuoytest

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"time"

	"github.com/linuxuser586/gcpemu/internal/escapeguard"
)

// Guarded runs sonobuoy behind the Escape guard (see CONTEXT.md), so that
// a request it sends off the machine is refused and named.
type Guarded struct {
	Bin   string   // sonobuoy
	Env   []string // its environment, before the guard's
	Guard *escapeguard.Guard
	// APIServer is the cluster's API server ("https://10.0.0.2:6443" or
	// "10.0.0.2"), on this machine, which the guard lets through.
	APIServer string
}

// exec runs sonobuoy with args within timeout, returning its output and
// the hosts the guard refused.
func (g Guarded) exec(timeout time.Duration, args ...string) (string, []string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, g.Bin, args...)
	var exempt []string
	if h := g.apiServerHost(); h != "" {
		exempt = append(exempt, h)
	}
	cmd.Env = append(slices.Clip(g.Env), g.Guard.Env(exempt...)...)
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		err = fmt.Errorf("timed out after %v: %w", timeout, err)
	}
	return string(out), g.Guard.Take(), err
}

// Sonobuoy is exec as Quick.Sonobuoy, calling errorf for each request the
// guard refused. gke-gcloud-auth-plugin, in a kubeconfig from gcloud,
// runs gcloud under it, so gcloud's universe descriptor fetch is not an
// escape.
func (g Guarded) Sonobuoy(errorf func(format string, args ...any)) func(time.Duration, ...string) (string, error) {
	return func(timeout time.Duration, args ...string) (string, error) {
		out, escaped, err := g.exec(timeout, args...)
		for _, h := range escapeguard.Gcloud(escaped) {
			errorf("sonobuoy %s: Escaped request to %s", args[0], h)
		}
		return out, err
	}
}

// canaryHost is the API server Canary's kubeconfig names: not on this
// machine, so the guard must refuse it.
const canaryHost = "kube-conformance.gcpemu.invalid"

// Canary asks for the server version of a cluster at canaryHost, writing
// its kubeconfig in dir, and returns an error unless the guard refuses
// the request. Without it, a sonobuoy that ignored HTTPS_PROXY would pass
// every run with the guard never seeing a request.
func (g Guarded) Canary(dir string) error {
	cfg := filepath.Join(dir, "kubeconfig")
	if err := os.WriteFile(cfg, []byte(`apiVersion: v1
kind: Config
clusters:
- name: canary
  cluster:
    server: https://`+canaryHost+`
contexts:
- name: canary
  context: {cluster: canary, user: canary}
current-context: canary
users:
- name: canary
  user: {token: canary}
`), 0o600); err != nil {
		return err
	}
	out, escaped, _ := g.exec(time.Minute, "version", "--kubeconfig", cfg)
	if !slices.Equal(escaped, []string{canaryHost}) {
		return fmt.Errorf("the Escape guard refused %v, want [%s]; sonobuoy said:\n%s", escaped, canaryHost, out)
	}
	return nil
}

// apiServerHost is APIServer's host, without scheme or port.
func (g Guarded) apiServerHost() string {
	if u, err := url.Parse(g.APIServer); err == nil && u.Host != "" {
		return u.Hostname()
	}
	if h, _, err := net.SplitHostPort(g.APIServer); err == nil {
		return h
	}
	return g.APIServer
}
