package lb

import (
	"context"
	"net"
	"time"
)

// installDNSMapper makes the emulated DNS answer A queries for forwarding
// rule IPs with the mapped listener IP (FR-LB-002, FR-INT-004).
func (d *dataplane) installDNSMapper() {
	svc, ok := d.s.env.Lookup("dns")
	if !ok {
		return
	}
	if m, ok := svc.(interface {
		SetAddressMapper(func(net.IP, string) net.IP)
	}); ok {
		m.SetAddressMapper(d.dnsAddress)
	}
}

// dnsAddress maps a forwarding rule IP in an answer to a client on
// network. Host clients get the mapped listener. VPC workloads get an
// external rule's own address, which the Edge relay serves to them
// (#187), as on GCP.
func (d *dataplane) dnsAddress(ip net.IP, network string) net.IP {
	if network != "" {
		if cfg := d.cfg.Load(); cfg != nil && cfg.public[ip.String()] {
			return nil
		}
	}
	return d.mappedIP(ip)
}

// dialContext connects to a backend endpoint: through the lb-edge relay
// for addresses only reachable inside container networks (GKE pod IPs),
// through the ContainerDialer otherwise.
func (d *dataplane) dialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, _, _ := net.SplitHostPort(addr)
	if !d.s.options().DirectDial && d.edge != nil && d.edge.covers(host) {
		return d.edge.dial(ctx, addr)
	}
	if c := d.s.env.Containers; c != nil {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		return c.DialContext(ctx, network, addr)
	}
	dl := net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	return dl.DialContext(ctx, network, addr)
}

// DialBackend connects to a backend endpoint exactly as the proxy does
// (through the lb-edge relay for GKE pod IPs). Diagnostics and tests use
// it to reach endpoints the host cannot route to.
func (s *Service) DialBackend(ctx context.Context, addr string) (net.Conn, error) {
	return s.dp.dialContext(ctx, "tcp", addr)
}
