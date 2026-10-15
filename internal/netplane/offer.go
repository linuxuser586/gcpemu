package netplane

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"syscall"
)

// The inward half of the Host attachment (ADR 0014): VPC workloads reach
// Offered ports at the Host address of a VPC container network, and each
// connection or datagram is forwarded to 127.0.0.1 on the host. With the
// VM relay, the relay holds the Host address and listens there; without
// it, the Host address is the network's gateway and the emulator binds
// the Offered port there itself.

// ErrInContainer is returned when the emulator runs in a container (ADR
// 0003): its loopback is not the host's, so it has no Host attachment.
var ErrInContainer = errors.New("the Host attachment needs the emulator on the host; it runs in a container")

// offerKey names one Offered port at one Host address.
type offerKey struct {
	addr string
	port int
	udp  bool
}

func (k offerKey) proto() string {
	if k.udp {
		return "udp"
	}
	return "tcp"
}

// listenAddr is where the Offered port is served.
func (k offerKey) listenAddr() string { return net.JoinHostPort(k.addr, strconv.Itoa(k.port)) }

// offerPrefix starts the relay endpoint names of Offered ports.
const offerPrefix = "offer:"

// relayName is the relay endpoint name of k.
func (k offerKey) relayName() string { return offerPrefix + k.listenAddr() + "/" + k.proto() }

// offerTarget returns the host address an Offered port's relay endpoint
// name forwards to, or "" for other names.
func offerTarget(name string) string {
	rest, ok := strings.CutPrefix(name, offerPrefix)
	if !ok {
		return ""
	}
	addr, _, _ := strings.Cut(rest, "/")
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return ""
	}
	return net.JoinHostPort("127.0.0.1", port)
}

// HostAddress returns the Host address of the container network n: the
// address the VM relay holds on it, joining it first if needed, or n's
// gateway where the host routes to it.
func (p *Plane) HostAddress(ctx context.Context, n Net) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, err := p.servicesLocked(ctx); err != nil {
		return "", err
	}
	if p.rt.Self() != "" {
		return "", ErrInContainer
	}
	if p.relay == nil {
		return n.Gateway, nil
	}
	ip, err := p.relayIP(ctx, n.Name)
	if err == nil && ip == "" {
		if err = p.rt.AttachMember(ctx, n.Name, false); err == nil {
			ip, err = p.relayIP(ctx, n.Name)
		}
	}
	if err == nil && ip == "" {
		err = fmt.Errorf("the VM relay holds no address on %s", n.Name)
	}
	return ip, err
}

func (p *Plane) relayIP(ctx context.Context, network string) (string, error) {
	ct, err := p.rt.InspectContainer(ctx, p.relay.ID())
	if err != nil {
		return "", err
	}
	return ct.IPs[network], nil
}

// Offer serves port, over UDP when udp and TCP otherwise, at hostAddr (a
// HostAddress), forwarding to the same port on the host's 127.0.0.1. It
// is idempotent, and a restarted relay offers it again. Where the host
// routes to the network, a port that a host process holds on the
// wildcard address cannot be offered: the error names that bind.
func (p *Plane) Offer(ctx context.Context, hostAddr string, port int, udp bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, err := p.servicesLocked(ctx); err != nil {
		return err
	}
	if p.closed {
		return errors.New("netplane closed")
	}
	if p.rt.Self() != "" {
		return ErrInContainer
	}
	k := offerKey{addr: hostAddr, port: port, udp: udp}
	if _, ok := p.offers[k]; ok {
		return nil
	}
	if p.relay != nil {
		if err := p.relay.Offer(ctx, k.relayName(), k.listenAddr(), udp); err != nil {
			return fmt.Errorf("VM relay: offer %d/%s at %s: %w", port, k.proto(), hostAddr, err)
		}
		p.offers[k] = nil
		return nil
	}
	f, err := offerForwarder(k, p.log)
	if err != nil {
		return err
	}
	p.offers[k] = f
	return nil
}

// Withdraw stops serving an Offered port; connections already forwarded
// run on.
func (p *Plane) Withdraw(ctx context.Context, hostAddr string, port int, udp bool) error {
	k := offerKey{addr: hostAddr, port: port, udp: udp}
	p.mu.Lock()
	defer p.mu.Unlock()
	f, ok := p.offers[k]
	switch {
	case !ok:
		return nil
	case f != nil:
		f.close()
	case p.relay != nil:
		// Forgotten only once the relay stopped listening, so that a
		// retry reaches it.
		if err := p.relay.Withdraw(ctx, k.relayName()); err != nil {
			return err
		}
	}
	delete(p.offers, k)
	return nil
}

// reofferLocked has a newly started relay serve the Offered ports again;
// one it cannot serve is forgotten.
func (p *Plane) reofferLocked(ctx context.Context) {
	for k, f := range p.offers {
		if f != nil {
			continue
		}
		if err := p.relay.Offer(ctx, k.relayName(), k.listenAddr(), k.udp); err != nil {
			// Forgotten, so that offering it again serves it.
			delete(p.offers, k)
			p.log.Warn("VM relay: an Offered port could not be served again", "addr", k.listenAddr(), "proto", k.proto(), "err", err)
		}
	}
}

// offerForwarder binds k on the host and forwards to 127.0.0.1.
func offerForwarder(k offerKey, log *slog.Logger) (*forwarder, error) {
	f := &forwarder{addr: k.listenAddr(), target: net.JoinHostPort("127.0.0.1", strconv.Itoa(k.port)), log: log}
	var err error
	if k.udp {
		f.udp, err = net.ListenPacket("udp", f.addr)
	} else {
		f.tcp, err = net.Listen("tcp", f.addr)
	}
	if err != nil {
		if errors.Is(err, syscall.EADDRINUSE) {
			if held := wildcardHolder(k.port, k.udp); held != "" {
				return nil, fmt.Errorf("port %d/%s is held on %s by a process on the host, which VPC workloads would reach at %s without an offer; have it listen on 127.0.0.1:%d instead", k.port, k.proto(), held, f.addr, k.port)
			}
		}
		return nil, fmt.Errorf("offer %d/%s at %s: %w", k.port, k.proto(), k.addr, err)
	}
	f.wg.Add(1)
	if k.udp {
		go f.serveUDP()
	} else {
		go f.serveTCP()
	}
	return f, nil
}
