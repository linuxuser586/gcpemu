package lb

import (
	"context"
	"crypto/tls"
	"errors"
	"hash/fnv"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"sync"
	"syscall"
	"time"
)

// Listener mapping (FR-LB-002). For each forwarding rule IP:port the proxy
// listens, in order of preference, on:
//
//  1. 127.0.0.x:<port>, x allocated deterministically per forwarding rule
//     IP (Linux routes all of 127/8 to loopback);
//  2. when that port is privileged and a container runtime is available,
//     the lb-edge container (edge.go), which owns one container IP per
//     forwarding rule and relays connections with PROXY protocol v2;
//  3. otherwise 127.0.0.x:<base+n> (base 18080, or the configured "lb"
//     port; 0 = dynamic).
//
// Each mapping is recorded as endpoint "lb:<name>" (printed by `gcpemu env`
// as GCPEMU_LB_<NAME>), and the emulated DNS answers A queries for a
// forwarding rule IP with the mapped listener IP.

type listener struct {
	name  string
	key   string
	https bool
	addr  string // host:port clients use
	mode  string // "loopback", "edge" or "fallback"
	ln    net.Listener
	srv   *http.Server
	// virt receives edge-relayed connections for this frontend.
	virt    *chanListener
	onClose func()
}

func (l *listener) close() {
	if l.srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_ = l.srv.Shutdown(ctx)
		cancel()
		_ = l.srv.Close()
	}
	if l.ln != nil {
		_ = l.ln.Close()
	}
	if l.virt != nil {
		l.virt.Close()
	}
	if l.onClose != nil {
		go l.onClose()
	}
}

// loopIP returns 127.0.0.x for a forwarding rule IP. Callers hold d.mu.
func (d *dataplane) loopIP(ip string) string {
	if x, ok := d.loopX[ip]; ok {
		return "127.0.0." + strconv.Itoa(x)
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(ip))
	x := int(h.Sum32()%252) + 2
	used := map[int]bool{}
	for _, v := range d.loopX {
		used[v] = true
	}
	for used[x] {
		x++
		if x > 254 {
			x = 2
		}
	}
	d.loopX[ip] = x
	return "127.0.0." + strconv.Itoa(x)
}

func (d *dataplane) fallbackBase() int {
	if v, ok := d.s.env.Config.Ports["lb"]; ok {
		return v
	}
	return 18080
}

// listen opens the listener of a frontend. Callers hold d.mu.
func (d *dataplane) listen(fe *frontend) (*listener, error) {
	l := &listener{name: fe.fr.Name, key: fe.key, https: fe.https}
	lip := d.loopIP(fe.ip)
	ln, err := net.Listen("tcp", net.JoinHostPort(lip, strconv.Itoa(fe.port)))
	switch {
	case err == nil:
		l.ln, l.mode = ln, "loopback"
	case errors.Is(err, syscall.EACCES) && d.edge.available(d.ctx):
		addr, verr := d.edge.add(d.ctx, fe)
		if verr != nil {
			d.s.env.Log.Warn("lb: edge container unavailable; using a high port", "forwardingRule", fe.frPath, "err", verr)
			break
		}
		l.virt = newChanListener(addr)
		l.addr, l.mode = addr, "edge"
		key := fe.key
		l.onClose = func() { d.edge.remove(key) }
	}
	if l.ln == nil && l.virt == nil {
		base := d.fallbackBase()
		for i := 0; i < 200; i++ {
			port := 0
			if base != 0 {
				port = base + d.nextPort
				d.nextPort++
			}
			if ln, err = net.Listen("tcp", net.JoinHostPort(lip, strconv.Itoa(port))); err == nil || base == 0 {
				break
			}
		}
		if err != nil {
			return nil, err
		}
		l.ln, l.mode = ln, "fallback"
	}
	if l.ln != nil {
		l.addr = l.ln.Addr().String()
	}
	l.srv = d.server(fe)
	serve := func(ln net.Listener) {
		var err error
		if fe.https {
			err = l.srv.ServeTLS(ln, "", "")
		} else {
			err = l.srv.Serve(ln)
		}
		if err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
			d.s.env.Log.Debug("lb: listener stopped", "forwardingRule", fe.frPath, "err", err)
		}
	}
	if l.ln != nil {
		go serve(l.ln)
	}
	if l.virt != nil {
		go serve(l.virt)
	}
	d.s.env.Endpoints.Set("lb:"+fe.fr.Name, l.addr)
	d.s.env.Log.Info("lb: forwarding rule listening", "forwardingRule", fe.frPath, "ip", fe.ip, "port", fe.port, "listener", l.addr, "mode", l.mode)
	return l, nil
}

// server builds the HTTP server of a frontend: HTTP/1.1 and HTTP/2 (ALPN
// on TLS) with WebSocket upgrades (FR-LB-005).
func (d *dataplane) server(fe *frontend) *http.Server {
	srv := &http.Server{
		Handler:           d.handler(fe.key),
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       fe.keepAlive,
		ErrorLog:          log.New(io.Discard, "", 0),
		ConnContext:       connContext,
	}
	if fe.https {
		srv.TLSConfig = &tls.Config{
			GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) { return d.tlsConfig(fe.key, hello) },
			NextProtos:         []string{"h2", "http/1.1"},
		}
	}
	return srv
}

// listenerAddr returns the mapped listener of a forwarding rule IP:port.
func (d *dataplane) listenerAddr(key string) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	if l := d.listeners[key]; l != nil {
		return l.addr
	}
	return ""
}

// mappedIP returns the listener IP for a forwarding rule IP (nil if none).
func (d *dataplane) mappedIP(ip net.IP) net.IP {
	cfg := d.cfg.Load()
	if cfg == nil || !cfg.byIP[ip.String()] {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, l := range d.listeners {
		h, _, _ := net.SplitHostPort(l.key)
		if h == ip.String() {
			lh, _, _ := net.SplitHostPort(l.addr)
			return net.ParseIP(lh)
		}
	}
	return nil
}

// chanListener is a net.Listener fed with connections by the edge relay.
type chanListener struct {
	addr net.Addr
	ch   chan net.Conn
	once sync.Once
	done chan struct{}
}

func newChanListener(addr string) *chanListener {
	ta, _ := net.ResolveTCPAddr("tcp", addr)
	return &chanListener{addr: ta, ch: make(chan net.Conn), done: make(chan struct{})}
}

func (c *chanListener) Accept() (net.Conn, error) {
	select {
	case conn := <-c.ch:
		return conn, nil
	case <-c.done:
		return nil, net.ErrClosed
	}
}

// deliver hands a connection to the listener's server.
func (c *chanListener) deliver(conn net.Conn) bool {
	select {
	case c.ch <- conn:
		return true
	case <-c.done:
		return false
	}
}

func (c *chanListener) Close() error {
	c.once.Do(func() { close(c.done) })
	return nil
}

func (c *chanListener) Addr() net.Addr { return c.addr }
