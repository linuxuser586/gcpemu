package lb

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strings"

	"github.com/linuxuser586/gcpemu/internal/loopback"
)

// --lb-bind (Config.LBBind): every forwarding rule listener also serves on
// <address>:<the rule's real port> for each configured address, so that
// another device (a phone on the LAN) can reach the load balancer while
// the admin API, the gateway and every other data plane stay on --bind.
//
// The emulator listens there itself when it may bind the port. When the
// port is privileged (ip_unprivileged_port_start above it), the lb-lan
// container, on the host network, listens instead and relays each
// connection to the edge ingress with a PROXY v2 header, exactly like
// lb-edge (the edgeagent package runs in both, and manages lb-lan as a
// LANRelay). It does not on macOS, where the
// forwarding rule's wildcard listener on a privileged port serves the
// --lb-bind addresses too, nor on a VM runtime, whose host network is the
// VM's.
//
// An address a rule's listener covers itself (its own address, such as
// 127.0.0.1 in single-loopback mode, or any address on its port for a
// wildcard listener) is served by that listener, and claimed first.
//
// Two forwarding rules on one port cannot share an address: the one with
// the lowest "ip:port" key gets it. Each frontend's addresses are recorded
// as endpoint "lb-lan:<name>", which `gcpemu status` and `doctor` print.

// lanBinding is one claimed --lb-bind address:port.
type lanBinding struct {
	key   string    // frontend key
	l     *listener // the frontend listener it serves
	ln    net.Listener
	relay bool // served by the lb-lan container
}

// syncLAN binds the --lb-bind addresses of every listener. Callers hold d.mu.
func (d *dataplane) syncLAN() {
	binds := d.s.env.Config.LBBind
	if len(binds) == 0 {
		return
	}
	if d.lan == nil {
		d.lan = map[string]*lanBinding{}
		d.lanErr = map[string]string{}
	}
	for addr, b := range d.lan {
		if d.listeners[b.key] != b.l {
			if b.ln != nil {
				b.ln.Close()
			}
			delete(d.lan, addr)
		}
	}
	keys := make([]string, 0, len(d.listeners))
	for k := range d.listeners {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var fresh []string
	for _, key := range keys {
		l := d.listeners[key]
		_, port, _ := net.SplitHostPort(key)
		for _, ip := range binds {
			addr := net.JoinHostPort(ip, port)
			if d.lan[addr] == nil && l.covers(addr) {
				d.lan[addr] = &lanBinding{key: key, l: l}
				fresh = append(fresh, addr)
			}
		}
	}
	for _, key := range keys {
		l := d.listeners[key]
		_, port, _ := net.SplitHostPort(key)
		for _, ip := range binds {
			addr := net.JoinHostPort(ip, port)
			if b := d.lan[addr]; b != nil {
				if b.key != key {
					d.lanFailed(addr, key, fmt.Errorf("already serves forwarding rule %s", b.l.name))
				}
				continue
			}
			b, err := d.bindLAN(l, addr)
			if err != nil {
				d.lanFailed(addr, key, err)
				continue
			}
			d.lan[addr] = b
			fresh = append(fresh, addr)
		}
	}
	var relayed []string
	for addr, b := range d.lan {
		if b.relay {
			relayed = append(relayed, addr)
		}
	}
	if err := d.setRelay(relayed); err != nil {
		// Not exposed after all: retried on the next change.
		for _, addr := range relayed {
			d.lanFailed(addr, d.lan[addr].key, fmt.Errorf("lb-lan relay: %w", err))
			delete(d.lan, addr)
		}
	}
	for _, addr := range fresh {
		if b := d.lan[addr]; b != nil {
			delete(d.lanErr, addr+" "+b.key)
			mode := "direct"
			switch {
			case b.relay:
				mode = "relay"
			case b.ln == nil:
				mode = "listener"
			}
			d.s.env.Log.Info("lb: forwarding rule exposed", "forwardingRule", b.l.name, "address", addr, "mode", mode)
		}
	}
	byName := map[string][]string{}
	for addr, b := range d.lan {
		byName[b.l.name] = append(byName[b.l.name], addr)
	}
	for _, n := range d.s.env.Endpoints.Names() {
		if name, ok := strings.CutPrefix(n, "lb-lan:"); ok && byName[name] == nil {
			d.s.env.Endpoints.Delete(n)
		}
	}
	for name, addrs := range byName {
		sort.Strings(addrs)
		d.s.env.Endpoints.Set("lb-lan:"+name, strings.Join(addrs, ","))
	}
}

// bindLAN serves l on addr, directly or through the lb-lan relay.
func (d *dataplane) bindLAN(l *listener, addr string) (*lanBinding, error) {
	b := &lanBinding{key: l.key, l: l}
	ln, err := net.Listen("tcp", addr)
	if err == nil {
		b.ln = ln
		go l.serve(ln)
		return b, nil
	}
	switch loopback.RefusedOn(d.goos, err) {
	case loopback.NotRefused:
		return nil, err
	case loopback.Excluded:
		return nil, loopback.ExcludedError(err, "tcp")
	case loopback.PrivilegedWildcard:
		return nil, fmt.Errorf("%w (privileged port: only the first forwarding rule on it is served on --lb-bind addresses)", err)
	}
	if !d.edge.available(d.ctx) {
		return nil, fmt.Errorf("%w (privileged port and no container runtime to relay it; lower net.ipv4.ip_unprivileged_port_start)", err)
	}
	if d.vmRuntime() {
		return nil, fmt.Errorf("%w (privileged port, and lb-lan on a VM runtime would listen in the VM; lower net.ipv4.ip_unprivileged_port_start)", err)
	}
	l.relayed()
	b.relay = true
	return b, nil
}

// vmRuntime reports whether the container runtime runs in a VM (the VM
// relay's decision), where the host network is the VM's.
func (d *dataplane) vmRuntime() bool {
	np, err := d.s.env.Containers.Netplane(d.ctx)
	if err != nil {
		return false
	}
	if _, err := np.Services(d.ctx); err != nil {
		return false
	}
	dec, ok := np.RelayDecision()
	return ok && dec.VM
}

// lanFailed logs a --lb-bind failure once per address and frontend.
func (d *dataplane) lanFailed(addr, key string, err error) {
	k := addr + " " + key
	if d.lanErr[k] == err.Error() {
		return
	}
	d.lanErr[k] = err.Error()
	name := key
	if l := d.listeners[key]; l != nil {
		name = l.name
	}
	d.s.env.Log.Warn("lb: cannot expose forwarding rule on --lb-bind address", "forwardingRule", name, "address", addr, "err", err)
}

// setRelay makes the lb-lan relay listen on exactly addrs. Callers hold
// d.mu.
func (d *dataplane) setRelay(addrs []string) error {
	var ingress string
	if len(addrs) > 0 {
		var err error
		if ingress, err = d.edge.ingressAddr(); err != nil {
			return err
		}
	}
	return d.relay.Set(d.ctx, ingress, addrs)
}

// closeLAN closes every --lb-bind listener and the relay. Callers hold d.mu.
func (d *dataplane) closeLAN(ctx context.Context) {
	for addr, b := range d.lan {
		if b.ln != nil {
			b.ln.Close()
		}
		delete(d.lan, addr)
	}
	d.relay.Close(ctx)
}
