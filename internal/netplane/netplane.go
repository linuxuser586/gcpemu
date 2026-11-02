// Package netplane connects the emulator process to the container networks
// it creates (Section 3.3). Every container the emulator runs is attached to
// an internal "services" network on which the emulator's own endpoints
// (gateway, metadata, DNS, registry, ...) are reachable at the network's
// gateway address through forwarders, and optionally to an "external"
// network that provides internet egress (public IPs). On a VM runtime the
// VM relay (ADR 0005) listens for those endpoints instead, and carries the
// emulator's dials to container addresses (DialContext).
package netplane

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	goruntime "runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/linuxuser586/gcpemu/internal/runtime"
	"github.com/linuxuser586/gcpemu/internal/vmrelay"
)

// Net describes a container network.
type Net struct {
	// Name is the runtime network name.
	Name string
	// Subnet is the IPv4 CIDR.
	Subnet string
	// Gateway is the host-side address on the network.
	Gateway string
	// Bridge is the host interface holding Gateway, as the runtime
	// reports it ("" if it doesn't).
	Bridge string
}

// Plane manages the services and external networks of one instance.
type Plane struct {
	rt        *runtime.Manager
	endpoints func() map[string]string
	// allInterfaces is whether the Instance binds its Service endpoints to
	// every interface; the recorded addresses are loopback either way (#613).
	allInterfaces bool
	log           *slog.Logger

	mu     sync.Mutex
	svc    *Net
	ext    *Net
	fwd    map[string]*forwarder // endpoint name → forwarder on svc gateway
	closed bool
	// decision is made with the services network (vmrelay.Decide).
	decision *vmrelay.Decision
	relay    *vmrelay.Relay
	// relayed maps endpoint names to the addresses the relay listens on
	// for them, which a restarted relay listens on again.
	relayed map[string]string
	// offers are the Offered ports served (Offer): nil when the relay
	// serves one.
	offers map[offerKey]*forwarder
}

// New returns a plane. endpoints returns the current host endpoint map
// (name → host:port), as recorded by emu.Endpoints; bind is the address
// the Instance binds them on (--bind).
func New(rt *runtime.Manager, endpoints func() map[string]string, bind string, log *slog.Logger) *Plane {
	ip := net.ParseIP(bind)
	return &Plane{rt: rt, endpoints: endpoints, allInterfaces: bind == "" || ip != nil && ip.IsUnspecified(), log: log, fwd: map[string]*forwarder{}, relayed: map[string]string{}, offers: map[offerKey]*forwarder{}}
}

// Runtime returns the runtime manager.
func (p *Plane) Runtime() *runtime.Manager { return p.rt }

// Services returns the internal services network, creating it on first
// use, and starts the VM relay if the Instance uses one.
func (p *Plane) Services(ctx context.Context) (Net, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.servicesLocked(ctx)
}

func (p *Plane) servicesLocked(ctx context.Context) (Net, error) {
	if p.svc == nil {
		n, err := p.ensure(ctx, "svc", true)
		if err != nil {
			return Net{}, err
		}
		p.svc = &n
	}
	if p.decision == nil {
		d := vmrelay.Decide(goruntime.GOOS, p.rt.Self() != "", p.svc.Gateway)
		p.decision = &d
		p.log.Info("VM relay "+d.String(), "gateway", p.svc.Gateway)
	}
	if p.decision.On && p.relay == nil {
		if err := p.startRelayLocked(ctx); err != nil {
			return Net{}, err
		}
	}
	return *p.svc, nil
}

// startRelayLocked starts the relay and has it listen again for the
// endpoints a previous relay listened for, on the same addresses: it
// takes the same address on the services network.
func (p *Plane) startRelayLocked(ctx context.Context) error {
	if p.closed {
		return errors.New("netplane closed")
	}
	ext, err := p.externalLocked(ctx)
	if err != nil {
		return err
	}
	r, err := vmrelay.Start(ctx, p.rt, vmrelay.Options{
		Services: p.svc.Name, External: ext.Name, Log: p.log,
		Target: func(name string) string {
			if t := offerTarget(name); t != "" {
				return t
			}
			return p.endpoints()[name]
		},
	})
	if err != nil {
		return err
	}
	for name, addr := range p.relayed {
		got, err := r.Listen(ctx, name, addr, name == "dns")
		if err != nil {
			_ = r.Close(context.Background())
			return fmt.Errorf("VM relay: listen for %s: %w", name, err)
		}
		if got != addr {
			p.log.Warn("VM relay: endpoint moved after a relay restart", "endpoint", name, "was", addr, "now", got)
			p.relayed[name] = got
		}
	}
	p.relay = r
	p.reofferLocked(ctx)
	return nil
}

// Relay returns the running VM relay, or nil.
func (p *Plane) Relay() *vmrelay.Relay {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.relay
}

// RelayDecision reports whether the Instance uses the VM relay, once the
// services network exists.
func (p *Plane) RelayDecision() (vmrelay.Decision, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.decision == nil {
		return vmrelay.Decision{}, false
	}
	return *p.decision, true
}

// External returns the external (egress-capable) network, creating it on first use.
func (p *Plane) External(ctx context.Context) (Net, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.externalLocked(ctx)
}

func (p *Plane) externalLocked(ctx context.Context) (Net, error) {
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
// addresses on it (a Cloud SQL instance's public IP). A network created by
// someone else between the inspect and the create is used as found (#588).
func (p *Plane) ensure(ctx context.Context, suffix string, internal bool) (Net, error) {
	name := p.rt.Name(suffix)
	n, err := p.rt.InspectNetwork(ctx, name)
	if errors.Is(err, runtime.ErrNotFound) {
		if _, err := p.rt.CreateNetwork(ctx, runtime.NetworkSpec{
			Name: name, Internal: internal, Labels: p.rt.Labels("core", suffix, "network"), Pin: true,
		}); err != nil && !runtime.IsAlreadyExists(err) {
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
	return Net{Name: n.Name, Subnet: n.Subnet, Gateway: n.Gateway, Bridge: n.Bridge}, nil
}

// Addr returns the address at which containers on the services network
// reach the named emulator endpoint ("gateway", "metadata", "dns", "ar",
// ...), starting a forwarder on the services gateway, or having the VM
// relay listen, if needed. The port is the same as the host endpoint's
// when it is free.
func (p *Plane) Addr(ctx context.Context, name string) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	svc, err := p.servicesLocked(ctx)
	if err != nil {
		return "", err
	}
	if p.closed {
		return "", errors.New("netplane closed")
	}
	if f, ok := p.fwd[name]; ok {
		return f.addr, nil
	}
	if a, ok := p.relayed[name]; ok && p.relay != nil {
		return a, nil
	}
	target := p.endpoints()[name]
	if target == "" {
		return "", fmt.Errorf("endpoint %q is not running", name)
	}
	_, port, _ := net.SplitHostPort(target)
	if p.relay != nil {
		a, err := p.relay.Listen(ctx, name, net.JoinHostPort(p.relay.IP(), port), name == "dns")
		if err != nil {
			return "", fmt.Errorf("VM relay: listen for %s: %w", name, err)
		}
		p.relayed[name] = a
		return a, nil
	}
	ip := svc.Gateway
	// Inside a container (ADR 0003) the emulator is on the services
	// network itself: an endpoint bound to every interface is reachable
	// there directly, others through a forwarder on its own address.
	if self, err := p.rt.SelfIP(ctx, svc.Name); err != nil {
		return "", err
	} else if self != "" {
		if p.allInterfaces {
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

// DialContext is the ContainerDialer: it connects to an address on one of
// the Instance's container networks, through the VM relay when there is
// one and directly otherwise. Addresses elsewhere are always dialed
// directly.
func (p *Plane) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	p.mu.Lock()
	r := p.relay
	restart := r == nil && p.decision != nil && p.decision.On && !p.closed
	p.mu.Unlock()
	if restart { // after Reset
		if _, err := p.Services(ctx); err != nil {
			return nil, err
		}
		r = p.Relay()
	}
	if host, _, err := net.SplitHostPort(addr); err == nil && r != nil && r.Covers(host) {
		return r.DialContext(ctx, network, addr)
	}
	d := net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	return d.DialContext(ctx, network, addr)
}

// Reset removes the VM relay; the next use of the plane starts a new one,
// which listens for the same endpoints on the same addresses.
func (p *Plane) Reset(ctx context.Context) error {
	p.mu.Lock()
	r := p.relay
	p.relay = nil
	p.mu.Unlock()
	if r == nil {
		return nil
	}
	return r.Close(ctx)
}

// Close stops all forwarders and removes the VM relay. Networks are left
// to runtime cleanup.
func (p *Plane) Close() {
	p.mu.Lock()
	p.closed = true
	for _, f := range p.fwd {
		f.close()
	}
	p.fwd = map[string]*forwarder{}
	for k, f := range p.offers {
		if f != nil {
			f.close()
		}
		delete(p.offers, k)
	}
	r := p.relay
	p.relay = nil
	p.mu.Unlock()
	if r != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := r.Close(ctx); err != nil {
			p.log.Warn("removing the VM relay", "err", err)
		}
	}
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
		if f.udp, err = ListenUDP(context.Background(), ip, p); err != nil {
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
			Splice(c, u)
		}()
	}
}

// Splice copies both ways until both directions end.
func Splice(a, b net.Conn) {
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(b, a); closeWrite(b); done <- struct{}{} }()
	go func() { _, _ = io.Copy(a, b); closeWrite(a); done <- struct{}{} }()
	<-done
	<-done
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
	if f.tcp != nil {
		f.tcp.Close()
	}
	if f.udp != nil {
		f.udp.Close()
	}
	f.wg.Wait()
}

// Exposed is a host port that Expose serves.
type Exposed struct {
	l  net.Listener
	wg sync.WaitGroup
	// name names what the port serves in a warning that its target
	// cannot be reached, logged once until a dial succeeds again (down).
	name string
	down atomic.Bool
}

// Expose binds addr, a loopback ip:port on the host, or a free port on the
// same IP when addr is taken and fallback is set, and pipes each
// connection it accepts to target, a container address, through
// DialContext. On a VM runtime this is how host clients reach a container
// port that appears while the Instance runs, which the runtime cannot
// publish then. While target cannot be reached, connections are accepted
// and closed, and a warning naming name, if set, and target is logged
// once; without a name, each failure is logged at Debug.
func (p *Plane) Expose(name, addr, target string, fallback bool) (*Exposed, error) {
	l, err := listenFallback(addr, fallback)
	if err != nil {
		return nil, err
	}
	return p.serveConn(l, name, p.dialTargets(func() []string { return []string{target} })), nil
}

// ExposeConn binds addr as Expose does and pipes each connection c it
// accepts to the connection dial opens for it, which may write to it
// first; dial sets its own timeouts.
func (p *Plane) ExposeConn(addr string, fallback bool, dial func(ctx context.Context, c net.Conn) (net.Conn, error)) (*Exposed, error) {
	l, err := listenFallback(addr, fallback)
	if err != nil {
		return nil, err
	}
	return p.serveConn(l, "", dial), nil
}

// listenFallback listens on addr, or with fallback on a free port of the
// same IP when addr is taken.
func listenFallback(addr string, fallback bool) (net.Listener, error) {
	l, err := net.Listen("tcp", addr)
	if err != nil && fallback {
		host, _, _ := net.SplitHostPort(addr)
		l, err = net.Listen("tcp", net.JoinHostPort(host, "0"))
	}
	return l, err
}

// Serve pipes each connection l accepts to the first of targets, container
// addresses, that DialContext reaches; targets is read for each
// connection. It owns l.
func (p *Plane) Serve(l net.Listener, targets func() []string) *Exposed {
	return p.serveConn(l, "", p.dialTargets(targets))
}

// dialTargets dials the first of targets that DialContext reaches.
func (p *Plane) dialTargets(targets func() []string) func(ctx context.Context, c net.Conn) (net.Conn, error) {
	return func(ctx context.Context, _ net.Conn) (net.Conn, error) {
		ts := targets()
		err := fmt.Errorf("no target")
		for _, target := range ts {
			dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			u, derr := p.DialContext(dctx, "tcp", target)
			cancel()
			if err = derr; err == nil {
				return u, nil
			}
		}
		return nil, fmt.Errorf("targets %v: %w", ts, err)
	}
}

// ServeDial pipes each connection l accepts to the connection dial opens
// for it; dial sets its own timeouts. It owns l.
func (p *Plane) ServeDial(l net.Listener, dial func(ctx context.Context) (net.Conn, error)) *Exposed {
	return p.serveConn(l, "", func(ctx context.Context, _ net.Conn) (net.Conn, error) { return dial(ctx) })
}

// serveConn is ServeDial with dial given the connection accepted, and the
// port named name (Exposed.name).
func (p *Plane) serveConn(l net.Listener, name string, dial func(ctx context.Context, c net.Conn) (net.Conn, error)) *Exposed {
	e := &Exposed{l: l, name: name}
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				u, err := dial(context.Background(), c)
				switch {
				case err == nil:
					e.down.Store(false)
				case name != "" && !e.down.Swap(true):
					p.log.Warn("exposed port: cannot reach its target; closing its connections until it is back", "for", name, "addr", l.Addr(), "err", err)
					return
				default:
					p.log.Debug("exposed port: dial failed", "addr", l.Addr(), "err", err)
					return
				}
				defer u.Close()
				Splice(c, u)
			}()
		}
	}()
	return e
}

// Addr is the host address bound.
func (e *Exposed) Addr() string { return e.l.Addr().String() }

// Close stops accepting connections; open ones run on.
func (e *Exposed) Close() error {
	err := e.l.Close()
	e.wg.Wait()
	return err
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
