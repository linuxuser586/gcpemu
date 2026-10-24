package sql

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	sqladmin "google.golang.org/api/sqladmin/v1beta4"

	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/netplane"
	"github.com/linuxuser586/gcpemu/services/sql/proxy"
)

// connectRouter serves port 3307 of one --sql-bind address for every
// instance. Connectors dial <address>:3307 whatever the instance, so the
// router reads the server name in the TLS ClientHello, the instance's
// dnsName, and relays the connection, ClientHello included, to that
// instance's agent, where TLS ends as on any other path.
type connectRouter struct {
	l  net.Listener
	wg sync.WaitGroup
}

// serveConnect routes each connection l accepts by route(server name) to
// a target opened with dial. It owns l.
func serveConnect(l net.Listener, route func(sni string) (string, bool), dial func(ctx context.Context, network, addr string) (net.Conn, error)) *connectRouter {
	r := &connectRouter{l: l}
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
				sni, hello, err := peekServerName(c)
				if err != nil {
					return
				}
				_ = c.SetReadDeadline(time.Time{})
				target, ok := route(sni)
				if !ok {
					return
				}
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				u, err := dial(ctx, "tcp", target)
				cancel()
				if err != nil {
					return
				}
				defer u.Close()
				if _, err := u.Write(hello); err != nil {
					return
				}
				netplane.Splice(c, u)
			}()
		}
	}()
	return r
}

// Addr is the address served.
func (r *connectRouter) Addr() string { return r.l.Addr().String() }

// Close stops accepting connections; open ones run on.
func (r *connectRouter) Close() error {
	err := r.l.Close()
	r.wg.Wait()
	return err
}

// errHelloRead stops the handshake peekServerName runs once it has the
// ClientHello.
var errHelloRead = errors.New("client hello read")

// peekServerName reads c's TLS ClientHello and returns its server name
// ("" when none is sent) and the bytes read, which the target must get
// first.
func peekServerName(c net.Conn) (string, []byte, error) {
	var buf bytes.Buffer
	var sni string
	var seen bool
	err := tls.Server(&helloConn{r: io.TeeReader(c, &buf)}, &tls.Config{
		GetConfigForClient: func(h *tls.ClientHelloInfo) (*tls.Config, error) {
			sni, seen = h.ServerName, true
			return nil, errHelloRead
		},
	}).Handshake()
	if !seen {
		return "", nil, err
	}
	return sni, buf.Bytes(), nil
}

// helloConn is the read side of a connection, for a handshake that only
// reads the ClientHello: what it would write is dropped.
type helloConn struct {
	net.Conn
	r io.Reader
}

func (h *helloConn) Read(p []byte) (int, error)       { return h.r.Read(p) }
func (h *helloConn) Write(p []byte) (int, error)      { return len(p), nil }
func (h *helloConn) Close() error                     { return nil }
func (h *helloConn) SetDeadline(time.Time) error      { return nil }
func (h *helloConn) SetReadDeadline(time.Time) error  { return nil }
func (h *helloConn) SetWriteDeadline(time.Time) error { return nil }
func (h *helloConn) LocalAddr() net.Addr              { return &net.TCPAddr{} }
func (h *helloConn) RemoteAddr() net.Addr             { return &net.TCPAddr{} }

// connectRoute picks the target of a connection with server name sni from
// targets (dnsName → agent address): the instance whose dnsName it is,
// or with no server name the address's only instance.
func connectRoute(targets map[string]string, sni string) (string, bool) {
	if sni == "" {
		if len(targets) != 1 {
			return "", false
		}
		for _, t := range targets {
			return t, true
		}
	}
	want := normalizeDNSName(sni)
	for name, t := range targets {
		if normalizeDNSName(name) == want {
			return t, true
		}
	}
	return "", false
}

// normalizeDNSName lowercases name and drops its trailing dot.
func normalizeDNSName(name string) string { return strings.ToLower(strings.TrimSuffix(name, ".")) }

// lanCaller reports whether the request in ctx came from another machine
// on the LAN, and the address of served (the --sql-bind addresses an
// instance is served on) it should be told: the one it arrived at, else
// the first, or "" when there is none. It came through the LAN when the
// --gfe-bind listeners marked it (emu.WithLANCaller), or when it reached
// one of lanIPs (the --sql-bind and --bind addresses) that isn't loopback.
func lanCaller(ctx context.Context, served, lanIPs []string) (string, bool) {
	ip, ok := emu.LANCaller(ctx)
	if !ok {
		a, _ := ctx.Value(http.LocalAddrContextKey).(*net.TCPAddr)
		if a == nil || a.IP.IsLoopback() {
			return "", false
		}
		ip = a.IP.String()
		if v4 := a.IP.To4(); v4 != nil {
			ip = v4.String()
		}
		if !slices.Contains(lanIPs, ip) {
			return "", false
		}
	}
	switch {
	case slices.Contains(served, ip):
		return ip, true
	case len(served) > 0:
		return served[0], true
	}
	return "", true
}

// lanIPAddresses is what a LAN caller is told of an instance's addresses:
// the --sql-bind address lanIP as PRIMARY, or nothing when the instance
// isn't served on the LAN ("").
func lanIPAddresses(lanIP string) []*sqladmin.IpMapping {
	if lanIP == "" {
		return nil
	}
	return []*sqladmin.IpMapping{{Type: "PRIMARY", IpAddress: lanIP}}
}

// --sql-bind (Config.SQLBind): every instance with a public IP is also
// served on each configured address, so that psql and the connectors on
// another machine reach it (#620, ADR 0011, ADR 0028):
//
//   - Postgres on a port of its own, its loopback host port when that is
//     free there and kept across restarts (instanceRecord.LANPort),
//     recorded as endpoint "sql-lan:<connection name>";
//   - port 3307, shared by every instance and routed by the TLS server
//     name connectors send, the instance's dnsName (connectRouter).
//
// Both relay TCP to the instance's agent, so its checks apply as on any
// other path: Postgres's password authentication for BUILT_IN users and
// IAM database authentication for IAM users, which needs a token the
// Instance issued, whatever the IAM enforcement level. The agent sees the
// connection come from the host, so authorized networks treat it as a
// host client's.
//
// A caller that arrived through the LAN is told the --sql-bind address as
// the instance's PRIMARY address (lanCaller), and no address for an
// instance the LAN can't reach.

// instanceLAN is how one instance is served on the --sql-bind addresses.
type instanceLAN struct {
	exposed  []*netplane.Exposed
	ips      []string // the --sql-bind addresses served
	endpoint string   // its Service endpoint's name
	dnsName  string
	connect  string // the agent's connector address
}

// serveLAN serves the instance on every --sql-bind address, Postgres
// trying its previous port first, else hostPort, and returns the port to
// try next time. It replaces the instance's previous listeners. An address
// it cannot serve is logged and skipped.
func (s *Service) serveLAN(ctx context.Context, plane *netplane.Plane, rec *instanceRecord, publicIP string, hostPort int) int {
	in := rec.Instance
	port := rec.LANPort
	if port == 0 {
		port = hostPort
	}
	key := instKey(in.Project, in.Name)
	s.lanMu.Lock()
	defer s.lanMu.Unlock()
	s.closeLANLocked(key)
	binds := s.env.Config.SQLBind
	if len(binds) == 0 || publicIP == "" {
		return port
	}
	l := &instanceLAN{dnsName: in.DnsName, connect: net.JoinHostPort(publicIP, strconv.Itoa(proxy.ConnectPort))}
	var addrs []string
	for _, ip := range binds {
		ex, err := plane.Expose(net.JoinHostPort(ip, strconv.Itoa(port)), net.JoinHostPort(publicIP, strconv.Itoa(proxy.PublicPort)), true)
		if err != nil {
			s.env.Log.Warn("sql: cannot serve the instance on --sql-bind address", "instance", in.ConnectionName, "address", ip, "err", s.env.ExplainBind(ctx, err))
			continue
		}
		if len(addrs) == 0 {
			_, port = netplane.HostPort(ex.Addr())
		}
		l.exposed = append(l.exposed, ex)
		l.ips = append(l.ips, ip)
		addrs = append(addrs, ex.Addr())
		s.routeLocked(ctx, plane, ip)
	}
	if len(addrs) > 0 {
		l.endpoint = "sql-lan:" + in.ConnectionName
		s.env.Endpoints.Set(l.endpoint, strings.Join(addrs, ","))
		s.env.Log.Info("sql: instance exposed", "instance", in.ConnectionName, "address", strings.Join(addrs, ","))
	}
	s.lan[key] = l
	return port
}

// routeLocked serves port 3307 of the --sql-bind address ip, unless it
// already does. Callers hold lanMu.
func (s *Service) routeLocked(ctx context.Context, plane *netplane.Plane, ip string) {
	if _, ok := s.routers[ip]; ok {
		return
	}
	addr := net.JoinHostPort(ip, strconv.Itoa(proxy.ConnectPort))
	l, err := net.Listen("tcp", addr)
	if err != nil {
		s.env.Log.Warn("sql: cannot serve the connectors' port on --sql-bind address; the Cloud SQL connectors can't reach instances there", "address", addr, "err", s.env.ExplainBind(ctx, err))
		return
	}
	s.routers[ip] = serveConnect(l, func(sni string) (string, bool) { return s.connectTarget(ip, sni) }, plane.DialContext)
}

// connectTarget is the agent address of the instance served on ip whose
// dnsName is sni (connectRoute).
func (s *Service) connectTarget(ip, sni string) (string, bool) {
	s.lanMu.Lock()
	targets := map[string]string{}
	for _, l := range s.lan {
		if slices.Contains(l.ips, ip) {
			targets[l.dnsName] = l.connect
		}
	}
	s.lanMu.Unlock()
	return connectRoute(targets, sni)
}

// closeLAN stops serving the instance on the --sql-bind addresses.
func (s *Service) closeLAN(project, name string) {
	s.lanMu.Lock()
	defer s.lanMu.Unlock()
	s.closeLANLocked(instKey(project, name))
}

func (s *Service) closeLANLocked(key string) {
	l, ok := s.lan[key]
	if !ok {
		return
	}
	closeAll(l.exposed)
	if l.endpoint != "" {
		s.env.Endpoints.Delete(l.endpoint)
	}
	delete(s.lan, key)
}

// closeAllLAN stops serving every instance, and port 3307, on the
// --sql-bind addresses.
func (s *Service) closeAllLAN() {
	s.lanMu.Lock()
	defer s.lanMu.Unlock()
	for k := range s.lan {
		s.closeLANLocked(k)
	}
	for ip, r := range s.routers {
		_ = r.Close()
		delete(s.routers, ip)
	}
}

// forCaller is in as the caller in ctx is told it: with the --sql-bind
// address for a caller that came through the LAN (lanIPAddresses).
func (s *Service) forCaller(ctx context.Context, in *sqladmin.DatabaseInstance) *sqladmin.DatabaseInstance {
	cfg := s.env.Config
	if len(cfg.SQLBind) == 0 {
		return in
	}
	lanIPs := cfg.SQLBind
	if !config.IsLoopback(cfg.Bind) {
		lanIPs = append(slices.Clip(lanIPs), cfg.Bind)
	}
	s.lanMu.Lock()
	var served []string
	if l, ok := s.lan[instKey(in.Project, in.Name)]; ok {
		served = l.ips
	}
	s.lanMu.Unlock()
	ip, ok := lanCaller(ctx, served, lanIPs)
	if !ok {
		return in
	}
	c := *in
	c.IpAddresses = lanIPAddresses(ip)
	return &c
}
