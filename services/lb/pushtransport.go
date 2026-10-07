package lb

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// PushTransport returns an HTTP transport for in-process clients (Pub/Sub
// push, FR-INT-011) that resolves host names through the emulated Cloud
// DNS, delivers requests addressed to a forwarding rule (or its mapped
// listener) straight to the load balancer's listener, and trusts the
// emulator CA in addition to the system roots, so a push endpoint such as
// https://app.example.test/push reaches the service behind the load
// balancer.
func (s *Service) PushTransport() http.RoundTripper {
	s.ptOnce.Do(func() {
		roots, err := x509.SystemCertPool()
		if err != nil || roots == nil {
			roots = x509.NewCertPool()
		}
		if s.env.CA != nil {
			roots.AddCert(s.env.CA.Certificate())
		}
		s.pt = &http.Transport{
			DialContext:         s.lbDial,
			TLSClientConfig:     &tls.Config{RootCAs: roots},
			ForceAttemptHTTP2:   true,
			MaxIdleConnsPerHost: 64,
			IdleConnTimeout:     90 * time.Second,
		}
	})
	return s.pt
}

// lbDial resolves addr like a client pointed at the emulated DNS would
// and connects to the load balancer listener that serves it.
func (s *Service) lbDial(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	ips := []string{host}
	if net.ParseIP(host) == nil {
		ips = s.resolveA(ctx, host)
	}
	d := net.Dialer{Timeout: 10 * time.Second}
	for _, ip := range ips {
		if l := s.dp.listenerFor(ip, port); l != "" {
			return d.DialContext(ctx, network, l)
		}
	}
	if len(ips) > 0 {
		return d.DialContext(ctx, network, net.JoinHostPort(ips[0], port))
	}
	return d.DialContext(ctx, network, addr)
}

// listenerFor maps a forwarding rule IP (or a mapped listener IP) and port
// to the listener address serving it.
func (d *dataplane) listenerFor(ip, port string) string {
	p, _ := strconv.Atoi(port)
	cfg := d.cfg.Load()
	if fe := cfg.fronts[net.JoinHostPort(ip, port)]; fe != nil {
		return d.listenerAddr(fe.key)
	}
	for _, fe := range cfg.fronts {
		if fe.port != p {
			continue
		}
		if m := d.mappedIP(net.ParseIP(fe.ip)); m != nil && m.String() == ip {
			return d.listenerAddr(fe.key)
		}
	}
	return ""
}

// ptState holds the lazily built push transport.
type ptState struct {
	ptOnce sync.Once
	pt     *http.Transport
}
