// Package frontend is the emulator's "Google front end": a TLS terminator
// that lets unmodified clients reach emulated services by their real
// hostnames (storage.googleapis.com, pubsub.googleapis.com,
// LOCATION-docker.pkg.dev, ...) over HTTPS, HTTP/2 and gRPC (FR-INT-007,
// FR-CORE-043).
//
// # Design
//
// The frontend is one TCP listener in the emulator process (endpoint
// "frontend", dynamic port unless --port frontend=N). For each connection
// it completes a TLS handshake with a certificate issued by the
// instance's local CA (env.CA) for the requested SNI, offering ALPN "h2"
// and "http/1.1", then pipes the decrypted byte stream unchanged to:
//
//   - the Artifact Registry listener for LOCATION-docker.pkg.dev, or
//   - the gateway for every other name.
//
// Both backends speak HTTP/1.1 and prior-knowledge h2c on the same port,
// so whichever protocol the client negotiated works end to end, including
// gRPC, streaming and trailers, and the Host / :authority header arrives
// untouched; the gateway's host routing (emu.Router Mount hosts) then
// picks the service. Only names the emulator serves get a certificate:
// the gateway's mounted hosts and LOCATION-docker.pkg.dev. A connection
// without SNI (a client dialing an IP address, such as kube-apiserver
// calling an admission webhook) gets a certificate for "localhost" and
// every IP address of the host, which includes the container networks'
// gateway addresses.
//
// Port 443 is reached from containers through small TCP relays rather
// than by binding it on the host (which needs root): the GKE node agent
// listens on 169.254.169.254:443 in every node, and the host-mode
// container (internal/hostmode) listens on its own address. Both resolve
// the served names to themselves with the DNS Relay in this package.
package frontend

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/linuxuser586/gcpemu/internal/emu"
)

// Endpoint is the frontend's name in emu.Endpoints (and netplane.Addr).
const Endpoint = "frontend"

// registrySuffix marks Artifact Registry Docker hosts (LOCATION-docker.pkg.dev).
const registrySuffix = "-docker.pkg.dev"

// Normalize lower-cases a DNS name and strips the trailing dot.
func Normalize(name string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(name)), ".")
}

// IsRegistryHost reports whether name is an Artifact Registry Docker host,
// LOCATION-docker.pkg.dev.
func IsRegistryHost(name string) bool {
	n := Normalize(name)
	loc, ok := strings.CutSuffix(n, registrySuffix)
	return ok && loc != "" && !strings.Contains(loc, ".")
}

// Serves reports whether name is served by the frontend given the
// gateway's mounted hosts.
func Serves(hosts []string, name string) bool {
	n := Normalize(name)
	if IsRegistryHost(n) {
		return true
	}
	for _, h := range hosts {
		if h == n {
			return true
		}
	}
	return false
}

// Frontend is the TLS-terminating front end.
type Frontend struct {
	env   *emu.Env
	hosts func() []string

	mu     sync.Mutex
	ln     net.Listener
	conns  map[net.Conn]struct{}
	closed bool
	wg     sync.WaitGroup
}

// New returns a frontend serving the names hosts returns (the gateway's
// mounted hosts) plus LOCATION-docker.pkg.dev.
func New(env *emu.Env, hosts func() []string) *Frontend {
	return &Frontend{env: env, hosts: hosts, conns: map[net.Conn]struct{}{}}
}

// Hosts returns the exact hostnames served (registry hosts match by
// pattern; see IsRegistryHost).
func (f *Frontend) Hosts() []string {
	if f.hosts == nil {
		return nil
	}
	out := append([]string(nil), f.hosts()...)
	sort.Strings(out)
	return out
}

// Serves reports whether the frontend serves name.
func (f *Frontend) Serves(name string) bool { return Serves(f.Hosts(), name) }

// Start opens the listener on the configured bind address and records the
// "frontend" endpoint.
func (f *Frontend) Start() error {
	addr := net.JoinHostPort(f.env.Config.Bind, strconv.Itoa(f.env.Config.Port(Endpoint)))
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("frontend listen %s: %w", addr, err)
	}
	f.mu.Lock()
	f.ln = l
	f.mu.Unlock()
	f.env.Endpoints.Set(Endpoint, l.Addr().String())
	f.wg.Add(1)
	go f.serve(l)
	return nil
}

// Addr returns the listener address ("" before Start).
func (f *Frontend) Addr() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ln == nil {
		return ""
	}
	return f.ln.Addr().String()
}

// Close stops the listener and open connections.
func (f *Frontend) Close() error {
	f.mu.Lock()
	f.closed = true
	if f.ln != nil {
		f.ln.Close()
	}
	for c := range f.conns {
		c.Close()
	}
	f.mu.Unlock()
	f.wg.Wait()
	return nil
}

func (f *Frontend) track(c net.Conn, add bool) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if add {
		if f.closed {
			return false
		}
		f.conns[c] = struct{}{}
	} else {
		delete(f.conns, c)
	}
	return true
}

func (f *Frontend) serve(l net.Listener) {
	defer f.wg.Done()
	for {
		c, err := l.Accept()
		if err != nil {
			return
		}
		if !f.track(c, true) {
			c.Close()
			return
		}
		f.wg.Add(1)
		go func() {
			defer f.wg.Done()
			defer f.track(c, false)
			f.handle(c)
		}()
	}
}

// TLSConfig returns the server TLS configuration (exported for tests and
// for in-process listeners that want the same certificates).
func (f *Frontend) TLSConfig() *tls.Config {
	return &tls.Config{
		MinVersion:     tls.VersionTLS12,
		NextProtos:     []string{"h2", "http/1.1"},
		GetCertificate: f.certificate,
	}
}

// certificate issues (cached) certificates for served names.
func (f *Frontend) certificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	if f.env.CA == nil {
		return nil, errors.New("frontend: no certificate authority")
	}
	name := Normalize(hello.ServerName)
	if name == "" {
		return f.env.CA.ServerCert(ipNames()...)
	}
	if !f.Serves(name) {
		return nil, fmt.Errorf("frontend: %s is not served by this emulator", name)
	}
	return f.env.CA.ServerCert(name)
}

// ipNames returns "localhost" and every IP address of the host.
func ipNames() []string {
	out := []string{"localhost", "127.0.0.1", "::1"}
	addrs, _ := net.InterfaceAddrs()
	seen := map[string]bool{"127.0.0.1": true, "::1": true}
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok {
			s := n.IP.String()
			if !seen[s] && !n.IP.IsLinkLocalUnicast() {
				seen[s] = true
				out = append(out, s)
			}
		}
	}
	sort.Strings(out[3:])
	return out
}

// backend returns the address that serves name.
func (f *Frontend) backend(name string) string {
	if IsRegistryHost(name) {
		if a := f.env.Endpoints.Get("ar"); a != "" {
			return a
		}
	}
	return f.env.Endpoints.Get("gateway")
}

func (f *Frontend) handle(raw net.Conn) {
	defer raw.Close()
	tc := tls.Server(raw, f.TLSConfig())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	err := tc.HandshakeContext(ctx)
	cancel()
	if err != nil {
		f.env.Log.Debug("frontend handshake failed", "remote", raw.RemoteAddr().String(), "err", err)
		return
	}
	target := f.backend(tc.ConnectionState().ServerName)
	if target == "" {
		return
	}
	up, err := net.DialTimeout("tcp", target, 5*time.Second)
	if err != nil {
		f.env.Log.Warn("frontend: backend unreachable", "target", target, "err", err)
		return
	}
	if !f.track(up, true) {
		up.Close()
		return
	}
	defer f.track(up, false)
	Pipe(tc, up)
}

// Pipe copies between client and server in both directions. When the
// server side ends, the client gets a half-close and the connection is
// torn down shortly after; a client half-close is passed on and the
// server may keep answering.
func Pipe(client, server net.Conn) {
	up := make(chan struct{})
	go func() {
		_, _ = io.Copy(server, client)
		closeWrite(server)
		close(up)
	}()
	_, _ = io.Copy(client, server)
	closeWrite(client)
	select {
	case <-up:
	case <-time.After(5 * time.Second):
	}
	client.Close()
	server.Close()
}

func closeWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	}
}

// Relay accepts TCP connections on l and pipes each to target (used by
// the in-container 443 relays). It returns when l is closed.
func Relay(l net.Listener, target string) error {
	for {
		c, err := l.Accept()
		if err != nil {
			return err
		}
		go func() {
			u, err := net.DialTimeout("tcp", target, 10*time.Second)
			if err != nil {
				c.Close()
				return
			}
			Pipe(c, u)
		}()
	}
}
