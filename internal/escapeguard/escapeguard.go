// Package escapeguard is the Escape guard (see CONTEXT.md): a proxy that
// OpenTofu runs in the acceptance tests go through, so that an Escaped
// request fails the test, naming the host, instead of reaching Google.
//
// The guard refuses every request; it never forwards one. A client
// reaches it through HTTPS_PROXY and HTTP_PROXY (Env). Requests to the
// API gateway bypass it, since Go never proxies loopback addresses, so
// the guard only sees traffic that would leave the machine.
package escapeguard

import (
	"fmt"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
)

// invalidUniverse is the universe_domain that `gcpemu tofu-provider`
// prints (providerUniverseDomain in internal/cli/extra.go): the provider
// addresses an API without a *_custom_endpoint as <api>.gcpemu.invalid.
const invalidUniverse = ".gcpemu.invalid"

// Guard is a running Escape guard.
type Guard struct {
	addr string
	mu   sync.Mutex
	seen []string // hosts not yet taken, in order, without duplicates
}

// New starts a guard on a loopback port and stops it when t ends.
func New(t testing.TB) *Guard {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("escape guard: %v", err)
	}
	g := &Guard{addr: ln.Addr().String()}
	srv := &http.Server{Handler: http.HandlerFunc(g.refuse)}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return g
}

// Env returns the variables that send a client's outbound requests to
// the guard. They override any proxy set in the environment, and clear
// NO_PROXY so that no host is exempt (loopback always is).
func (g *Guard) Env() []string {
	u := "http://" + g.addr
	return []string{
		"HTTPS_PROXY=" + u, "https_proxy=" + u,
		"HTTP_PROXY=" + u, "http_proxy=" + u,
		"NO_PROXY=", "no_proxy=",
	}
}

// Take returns the hosts refused since the last call.
func (g *Guard) Take() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := g.seen
	g.seen = nil
	return out
}

func (g *Guard) refuse(w http.ResponseWriter, r *http.Request) {
	// A CONNECT names its target in Host; a proxied plain request in its
	// absolute URL.
	host := r.Host
	if r.Method != http.MethodConnect && r.URL.Host != "" {
		host = r.URL.Host
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	g.mu.Lock()
	if !slices.Contains(g.seen, host) {
		g.seen = append(g.seen, host)
	}
	g.mu.Unlock()
	http.Error(w, "gcpemu escape guard: refused "+Describe(host), http.StatusForbidden)
}

// Describe explains why a request to host escaped.
func Describe(host string) string {
	if api, ok := strings.CutSuffix(host, invalidUniverse); ok {
		return fmt.Sprintf("Escaped request to %s: no *_custom_endpoint in the provider block covers %s.googleapis.com "+
			"(add its setting to providerEndpoints in internal/cli/extra.go)", host, api)
	}
	return "Escaped request to " + host
}
