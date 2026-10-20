// Package escapeguard is the Escape guard (see CONTEXT.md): a proxy that
// OpenTofu and gcloud runs in the acceptance tests go through, so that an
// Escaped request fails the test, naming the host, instead of reaching
// Google.
//
// The guard never forwards a request off the machine. It refuses every
// request, except that after Tunnel a CONNECT to a host Host mode serves
// goes to the Google front end, which runs on this machine. A client
// reaches it through HTTPS_PROXY and HTTP_PROXY (Env). Requests to the
// API gateway bypass it: Go never proxies loopback addresses, and NO_PROXY
// exempts them for clients such as gcloud that otherwise would. The guard
// only sees traffic that would leave the machine.
package escapeguard

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

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

	tunnelIP string          // the Google front end's IP
	tunneled map[string]bool // the hosts it serves
}

// New starts a guard on a loopback port and stops it when t ends.
func New(t testing.TB) *Guard {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("escape guard: %v", err)
	}
	g := &Guard{addr: ln.Addr().String()}
	srv := &http.Server{Handler: http.HandlerFunc(g.serve)}
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

// Tunnel passes each CONNECT to one of hosts on to ip, at the port the
// client asked for, instead of refusing it. Host mode serves hosts at ip,
// the Google front end's address on this machine, so a client that resolves
// Google hostnames through the guard reaches Host mode without a change
// to /etc/hosts or the system resolver.
func (g *Guard) Tunnel(ip string, hosts []string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.tunnelIP = ip
	g.tunneled = map[string]bool{}
	for _, h := range hosts {
		g.tunneled[h] = true
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

// serve tunnels a CONNECT to a host Tunnel named and refuses every other
// request, recording its host.
func (g *Guard) serve(w http.ResponseWriter, r *http.Request) {
	// A CONNECT names its target in Host; a proxied plain request in its
	// absolute URL.
	host := r.Host
	if r.Method != http.MethodConnect && r.URL.Host != "" {
		host = r.URL.Host
	}
	port := "443"
	if h, p, err := net.SplitHostPort(host); err == nil {
		host, port = h, p
	}
	g.mu.Lock()
	if r.Method == http.MethodConnect && g.tunneled[host] {
		ip := g.tunnelIP
		g.mu.Unlock()
		tunnel(w, net.JoinHostPort(ip, port))
		return
	}
	if !slices.Contains(g.seen, host) {
		g.seen = append(g.seen, host)
	}
	g.mu.Unlock()
	http.Error(w, "gcpemu escape guard: refused "+Describe(host), http.StatusForbidden)
}

// tunnel answers a CONNECT and copies bytes between the client and addr.
func tunnel(w http.ResponseWriter, addr string) {
	up, err := net.DialTimeout("tcp", addr, 10*time.Second)
	if err != nil {
		http.Error(w, "gcpemu escape guard: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer up.Close()
	w.WriteHeader(http.StatusOK)
	conn, buf, err := http.NewResponseController(w).Hijack()
	if err != nil {
		return
	}
	// Closing both ends when either side is done ends the other copy.
	defer conn.Close()
	go func() {
		_, _ = io.Copy(up, buf) // buf holds what the client sent after the CONNECT
		up.Close()
	}()
	_, _ = io.Copy(conn, up)
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
