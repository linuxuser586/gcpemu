// Package netplane connects the emulator process to the container networks
// it creates (Section 3.3). Every container the emulator runs is attached to
// an internal "services" network on which the emulator's own endpoints
// (gateway, metadata, DNS, registry, ...) are reachable at the network's
// gateway address through forwarders, and optionally to an "external"
// network that provides internet egress (public IPs).
package netplane

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/linuxuser586/gcpemu/internal/runtime"
)

// Net describes a container network.
type Net struct {
	// Name is the runtime network name.
	Name string
	// Subnet is the IPv4 CIDR.
	Subnet string
	// Gateway is the host-side address on the network.
	Gateway string
}

// Plane manages the services and external networks of one instance.
type Plane struct {
	rt        *runtime.Manager
	endpoints func() map[string]string
	log       *slog.Logger

	mu     sync.Mutex
	svc    *Net
	ext    *Net
	fwd    map[string]*forwarder // endpoint name → forwarder on svc gateway
	closed bool
}

// New returns a plane. endpoints returns the current host endpoint map
// (name → host:port), as recorded by emu.Endpoints.
func New(rt *runtime.Manager, endpoints func() map[string]string, log *slog.Logger) *Plane {
	return &Plane{rt: rt, endpoints: endpoints, log: log, fwd: map[string]*forwarder{}}
}

// Runtime returns the runtime manager.
func (p *Plane) Runtime() *runtime.Manager { return p.rt }

// Services returns the internal services network, creating it on first use.
func (p *Plane) Services(ctx context.Context) (Net, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.svc != nil {
		return *p.svc, nil
	}
	n, err := p.ensure(ctx, "svc", true)
	if err != nil {
		return Net{}, err
	}
	p.svc = &n
	return n, nil
}

// External returns the external (egress-capable) network, creating it on first use.
func (p *Plane) External(ctx context.Context) (Net, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ext != nil {
		return *p.ext, nil
	}
	n, err := p.ensure(ctx, "ext", false)
	if err != nil {
		return Net{}, err
	}
	p.ext = &n
	return n, nil
}

// ensure finds or creates a network named gcpemu-<id>-<suffix>; the runtime
// picks a free subnet, pinned so that containers can ask for fixed
// addresses on it (a Cloud SQL instance's public IP).
func (p *Plane) ensure(ctx context.Context, suffix string, internal bool) (Net, error) {
	name := p.rt.Name(suffix)
	n, err := p.rt.InspectNetwork(ctx, name)
	if errors.Is(err, runtime.ErrNotFound) {
		if _, err := p.rt.CreateNetwork(ctx, runtime.NetworkSpec{
			Name: name, Internal: internal, Labels: p.rt.Labels("core", suffix, "network"), Pin: true,
		}); err != nil {
			return Net{}, err
		}
		n, err = p.rt.InspectNetwork(ctx, name)
	}
	if err != nil {
		return Net{}, err
	}
	if n.Gateway == "" {
		return Net{}, fmt.Errorf("network %s has no IPv4 gateway", name)
	}
	return Net{Name: n.Name, Subnet: n.Subnet, Gateway: n.Gateway}, nil
}

// Addr returns the address at which containers on the services network
// reach the named emulator endpoint ("gateway", "metadata", "dns", "ar",
// ...), starting a forwarder on the services gateway if needed. The port is
// the same as the host endpoint's when it is free.
func (p *Plane) Addr(ctx context.Context, name string) (string, error) {
	svc, err := p.Services(ctx)
	if err != nil {
		return "", err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return "", errors.New("netplane closed")
	}
	if f, ok := p.fwd[name]; ok {
		return f.addr, nil
	}
	target := p.endpoints()[name]
	if target == "" {
		return "", fmt.Errorf("endpoint %q is not running", name)
	}
	host, port, _ := net.SplitHostPort(target)
	ip := svc.Gateway
	// Inside a container (ADR 0003) the emulator is on the services
	// network itself: an endpoint bound to every interface is reachable
	// there directly, others through a forwarder on its own address.
	if self, err := p.rt.SelfIP(ctx, svc.Name); err != nil {
		return "", err
	} else if self != "" {
		if host == "" || host == "0.0.0.0" || host == "::" {
			return net.JoinHostPort(self, port), nil
		}
		ip = self
	}
	f, err := newForwarder(ip, port, target, name == "dns", p.log)
	if err != nil {
		return "", err
	}
	p.fwd[name] = f
	return f.addr, nil
}

// Close stops all forwarders. Networks are left to runtime cleanup.
func (p *Plane) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	for _, f := range p.fwd {
		f.close()
	}
	p.fwd = map[string]*forwarder{}
}

// forwarder relays TCP (and optionally UDP) from a services-network
// address to a host endpoint.
type forwarder struct {
	addr   string
	tcp    net.Listener
	udp    net.PacketConn
	target string
	log    *slog.Logger
	wg     sync.WaitGroup
}

func newForwarder(ip, port, target string, withUDP bool, log *slog.Logger) (*forwarder, error) {
	l, err := net.Listen("tcp", net.JoinHostPort(ip, port))
	if err != nil {
		if l, err = net.Listen("tcp", net.JoinHostPort(ip, "0")); err != nil {
			return nil, err
		}
	}
	f := &forwarder{addr: l.Addr().String(), tcp: l, target: target, log: log}
	if withUDP {
		_, p, _ := net.SplitHostPort(f.addr)
		if f.udp, err = net.ListenPacket("udp", net.JoinHostPort(ip, p)); err != nil {
			l.Close()
			return nil, fmt.Errorf("udp forwarder on %s:%s: %w", ip, p, err)
		}
		f.wg.Add(1)
		go f.serveUDP()
	}
	f.wg.Add(1)
	go f.serveTCP()
	return f, nil
}

func (f *forwarder) serveTCP() {
	defer f.wg.Done()
	for {
		c, err := f.tcp.Accept()
		if err != nil {
			return
		}
		go func() {
			defer c.Close()
			u, err := net.DialTimeout("tcp", f.target, 5*time.Second)
			if err != nil {
				f.log.Debug("forwarder dial failed", "target", f.target, "err", err)
				return
			}
			defer u.Close()
			done := make(chan struct{}, 2)
			go func() { _, _ = io.Copy(u, c); closeWrite(u); done <- struct{}{} }()
			go func() { _, _ = io.Copy(c, u); closeWrite(c); done <- struct{}{} }()
			<-done
			<-done
		}()
	}
}

func closeWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	}
}

// serveUDP relays datagrams (DNS) with one upstream socket per query.
func (f *forwarder) serveUDP() {
	defer f.wg.Done()
	buf := make([]byte, 65535)
	for {
		n, from, err := f.udp.ReadFrom(buf)
		if err != nil {
			return
		}
		q := append([]byte(nil), buf[:n]...)
		go func() {
			u, err := net.Dial("udp", f.target)
			if err != nil {
				return
			}
			defer u.Close()
			_ = u.SetDeadline(time.Now().Add(10 * time.Second))
			if _, err := u.Write(q); err != nil {
				return
			}
			resp := make([]byte, 65535)
			m, err := u.Read(resp)
			if err != nil {
				return
			}
			_, _ = f.udp.WriteTo(resp[:m], from)
		}()
	}
}

func (f *forwarder) close() {
	f.tcp.Close()
	if f.udp != nil {
		f.udp.Close()
	}
	f.wg.Wait()
}

// HostPort splits "ip:port" into its parts with a numeric port.
func HostPort(addr string) (string, int) {
	h, p, _ := net.SplitHostPort(addr)
	n, _ := strconv.Atoi(p)
	return h, n
}

// Contains reports whether ip lies inside cidr.
func Contains(cidr, ip string) bool {
	_, n, err := net.ParseCIDR(cidr)
	if err != nil {
		return false
	}
	return n.Contains(net.ParseIP(strings.TrimSpace(ip)))
}
