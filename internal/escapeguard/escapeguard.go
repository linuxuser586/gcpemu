// Package escapeguard is the Escape guard (see CONTEXT.md): a proxy that
// OpenTofu and gcloud runs in the acceptance tests go through, so that an
// Escaped request fails the test, naming the host, instead of reaching
// Google.
//
// The guard refuses every request; it never forwards one. A client
// reaches it through HTTPS_PROXY and HTTP_PROXY (Env). Requests to the
// API gateway bypass it: Go never proxies loopback addresses, and NO_PROXY
// exempts them for clients such as gcloud that otherwise would. The guard
// only sees traffic that would leave the machine.
package escapeguard

import (
	"fmt"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/linuxuser586/gcpemu/internal/emu"
)

// invalidUniverse is the suffix of hosts in the Invalid universe, which
// `gcpemu tofu-provider` and `gcpemu env` give OpenTofu and gcloud: they
// address an API without an endpoint as <api>.gcpemu.invalid.
const invalidUniverse = "." + emu.InvalidUniverse

// gcloudDescriptorHost is where gcloud fetches the universe descriptor for
// a universe other than Google's. The URL is hard-coded and no endpoint
// override covers it, so it is a side effect of the Invalid universe rather
// than an Escaped request; gcloud falls back to Google's descriptor.
const gcloudDescriptorHost = "storage" + invalidUniverse

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

// loopback is the NO_PROXY list Env sets: the hosts Go never proxies,
// spelled out for Python clients (gcloud), which proxy them too.
const loopback = "127.0.0.1,localhost,::1"

// Env returns the variables that send a client's outbound requests to
// the guard. They override any proxy set in the environment, and set
// NO_PROXY so that only loopback is exempt.
func (g *Guard) Env() []string {
	u := "http://" + g.addr
	return []string{
		"HTTPS_PROXY=" + u, "https_proxy=" + u,
		"HTTP_PROXY=" + u, "http_proxy=" + u,
		"NO_PROXY=" + loopback, "no_proxy=" + loopback,
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

// Describe explains why a request from OpenTofu to host escaped.
func Describe(host string) string {
	if api, ok := strings.CutSuffix(host, invalidUniverse); ok {
		return fmt.Sprintf("Escaped request to %s: no *_custom_endpoint in the provider block covers %s.googleapis.com "+
			"(add its setting to providerEndpoints in internal/cli/extra.go)", host, api)
	}
	return "Escaped request to " + host
}

// DescribeGcloud explains why a request from gcloud to host escaped:
// gcloud sends an API without a CLOUDSDK_API_ENDPOINT_OVERRIDES_<API> to
// <api>.gcpemu.invalid, or to <api>.googleapis.com outside the Invalid
// universe.
func DescribeGcloud(host string) string {
	api, ok := strings.CutSuffix(host, invalidUniverse)
	if !ok {
		api, ok = strings.CutSuffix(host, ".googleapis.com")
	}
	if ok && !strings.Contains(api, ".") {
		return fmt.Sprintf("Escaped request to %s: no CLOUDSDK_API_ENDPOINT_OVERRIDES_%s in the environment "+
			"(the Service serving %s should print it from EnvVars)", host, strings.ToUpper(api), api)
	}
	return "Escaped request to " + host
}

// Gcloud drops from hosts, as Take returns them, the ones gcloud reaches
// in the Invalid universe without escaping: its universe descriptor fetch.
// Only gcloud's hosts may be filtered: from OpenTofu, the same host is a
// storage API with no *_custom_endpoint.
func Gcloud(hosts []string) []string {
	return slices.DeleteFunc(hosts, func(h string) bool { return h == gcloudDescriptorHost })
}
