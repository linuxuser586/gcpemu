package lb

import (
	"context"
	"fmt"
	"net"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/loopback"
)

// --lb-bind (Config.LBBind): every forwarding rule listener also serves on
// <address>:<the rule's real port> for each configured address, so that
// another device (a phone on the LAN) can reach the load balancer while
// the admin API, the gateway and every other data plane stay on --bind.
// So does every frontend another Service registers (SetLANFrontends): a
// GKE LoadBalancer Service port (#713).
//
// The emulator listens there itself when it may bind the port. When the
// port is privileged (ip_unprivileged_port_start above it), the lb-lan
// container, on the host network, listens instead and relays each
// connection to the edge ingress with a PROXY v2 header, exactly like
// lb-edge (the edgeagent package runs in both, and manages lb-lan as a
// LANRelay). It does not on macOS, where the
// frontend's wildcard listener on a privileged port serves the
// --lb-bind addresses too, nor on a VM runtime, whose host network is the
// VM's.
//
// An address a frontend's listener covers itself (its own address, such as
// 127.0.0.1 in single-loopback mode, or any address on its port for a
// wildcard listener) is served by that listener, and claimed first.
//
// Two frontends on one port cannot share an address: the one with the
// lowest key ("ip:port" for a forwarding rule, so that rules sort before
// other Services' frontends) gets it, taking it over from a higher one
// that holds it. The others' failures are listed on the admin API
// (admin.go), which `doctor` prints, and retried on each change. Each
// frontend's addresses are recorded as endpoint "lb-lan:<name>", which
// `gcpemu status` and `doctor` print.

// lanBinding is one claimed --lb-bind address:port.
type lanBinding struct {
	key   string    // frontend key
	l     *listener // the frontend listener it serves
	ln    net.Listener
	relay bool // served by the lb-lan container
}

// lanFailure is a frontend that cannot serve an --lb-bind address, and
// why.
type lanFailure struct {
	key, what, addr, err string
}

// lanFronts returns every frontend --lb-bind serves, by key: forwarding
// rules' listeners and other Services' frontends. Callers hold d.mu.
func (d *dataplane) lanFronts() map[string]*listener {
	out := make(map[string]*listener, len(d.listeners))
	for k, l := range d.listeners {
		out[k] = l
	}
	for _, fes := range d.ext {
		for k, l := range fes {
			out[k] = l
		}
	}
	return out
}

// lanPort is the port l serves on the --lb-bind addresses: its forwarding
// rule's real port, or the other Service's.
func (l *listener) lanPort() string {
	if l.ext != nil {
		return strconv.Itoa(l.ext.Port)
	}
	_, port, _ := net.SplitHostPort(l.key)
	return port
}

// lanWhat names what l serves, for messages.
func (l *listener) lanWhat() string {
	if l.ext != nil {
		return l.ext.What
	}
	return "forwarding rule " + l.name
}

// syncLAN binds the --lb-bind addresses of every frontend. Callers hold
// d.mu.
func (d *dataplane) syncLAN() {
	binds := d.s.env.Config.LBBind
	if len(binds) == 0 {
		return
	}
	if d.lan == nil {
		d.lan = map[string]*lanBinding{}
		d.lanErr = map[string]string{}
	}
	d.lanDown = map[string]lanFailure{}
	fronts := d.lanFronts()
	for addr, b := range d.lan {
		if fronts[b.key] != b.l {
			d.releaseLAN(addr, b)
		}
	}
	keys := make([]string, 0, len(fronts))
	for k := range fronts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var fresh []string
	for _, key := range keys {
		l := fronts[key]
		for _, ip := range binds {
			addr := net.JoinHostPort(ip, l.lanPort())
			if d.lan[addr] == nil && l.covers(addr) {
				d.lan[addr] = &lanBinding{key: key, l: l}
				fresh = append(fresh, addr)
			}
		}
	}
	for _, key := range keys {
		l := fronts[key]
		for _, ip := range binds {
			addr := net.JoinHostPort(ip, l.lanPort())
			if b := d.lan[addr]; b != nil {
				if b.key == key {
					continue
				}
				// A listener covering it holds it; a lower key wins it.
				if b.ln == nil && !b.relay || b.key < key {
					d.lanFailed(addr, l, fmt.Errorf("already serves %s", b.l.lanWhat()))
					continue
				}
				d.releaseLAN(addr, b)
			}
			b, err := d.bindLAN(l, addr)
			if err != nil {
				d.lanFailed(addr, l, err)
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
			d.lanFailed(addr, d.lan[addr].l, fmt.Errorf("lb-lan relay: %w", err))
			delete(d.lan, addr)
		}
	}
	for k := range d.lanErr {
		if _, ok := d.lanDown[k]; !ok {
			delete(d.lanErr, k)
		}
	}
	for _, addr := range fresh {
		if b := d.lan[addr]; b != nil {
			mode := "direct"
			switch {
			case b.relay:
				mode = "relay"
			case b.ln == nil:
				mode = "listener"
			}
			d.s.env.Log.Info("lb: exposed on --lb-bind address", "frontend", b.l.lanWhat(), "address", addr, "mode", mode)
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

// LANFrontendAddrs implements emu.LANFrontendLister.
func (s *Service) LANFrontendAddrs() []emu.LANFrontendAddrs { return s.dp.lanFrontendAddrs() }

// lanFrontendAddrs lists each forwarding rule, under its Service endpoint,
// and each other Service's Load balancing frontend that names one, with
// the --lb-bind addresses its Load balancing frontends claimed, or why
// they claimed none.
func (d *dataplane) lanFrontendAddrs() []emu.LANFrontendAddrs {
	cfg := d.cfg.Load()
	d.mu.Lock()
	defer d.mu.Unlock()
	claimed := map[string][]string{} // frontend key → addresses
	for addr, b := range d.lan {
		claimed[b.key] = append(claimed[b.key], addr)
	}
	failed := map[string][]string{} // frontend key → why not
	for _, f := range d.lanDown {
		failed[f.key] = append(failed[f.key], f.addr+": "+f.err)
	}
	byName := map[string]*emu.LANFrontendAddrs{}
	why := map[string][]string{}
	add := func(name, project, what, key string, reasons ...string) {
		e := byName[name]
		if e == nil {
			e = &emu.LANFrontendAddrs{Name: name, Project: project, What: what}
			byName[name] = e
		}
		e.Addrs = append(e.Addrs, claimed[key]...)
		why[name] = append(why[name], append(failed[key], reasons...)...)
	}
	if cfg != nil {
		for key, fe := range cfg.fronts {
			var reasons []string
			if r, ok := d.unavailable[key]; ok {
				reasons = append(reasons, "no listener: "+r)
			}
			add(endpointKey(fe), fe.project, "forwarding rule "+fe.frPath, key, reasons...)
		}
	}
	for _, fes := range d.ext {
		for key, l := range fes {
			if l.ext.Endpoint != "" {
				add(l.ext.Endpoint, l.ext.Project, l.ext.What+" port "+l.lanPort(), key)
			}
		}
	}
	out := make([]emu.LANFrontendAddrs, 0, len(byName))
	for name, e := range byName {
		sort.Strings(e.Addrs)
		e.Addrs = slices.Compact(e.Addrs)
		if len(e.Addrs) == 0 {
			reasons := slices.Compact(slices.Sorted(slices.Values(why[name])))
			switch {
			case len(d.s.env.Config.LBBind) == 0:
				e.Error = "its Instance has no --lb-bind"
			case len(reasons) > 0:
				e.Error = strings.Join(reasons, "; ")
			default:
				e.Error = "not served on any --lb-bind address"
			}
		}
		out = append(out, *e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// releaseLAN stops serving the --lb-bind address addr on b. Callers hold
// d.mu.
func (d *dataplane) releaseLAN(addr string, b *lanBinding) {
	if b.ln != nil {
		b.ln.Close()
	}
	delete(d.lan, addr)
}

// SetLANFrontends implements emu.LANFrontends. A frontend registered
// again unchanged keeps the addresses it holds.
func (s *Service) SetLANFrontends(owner string, fes []emu.LANFrontend) {
	d := s.dp
	d.mu.Lock()
	defer d.mu.Unlock()
	old := d.ext[owner]
	cur := map[string]*listener{}
	for _, fe := range fes {
		if l := old[fe.Key]; l != nil && sameFrontend(*l.ext, fe) {
			cur[fe.Key] = l
			continue
		}
		l := &listener{name: fe.Name, key: fe.Key, addr: fe.Addr, ext: &fe, serve: fe.Serve}
		if fe.Wildcard {
			l.mode = "wildcard"
		}
		cur[fe.Key] = l
	}
	for k, l := range old {
		if cur[k] != l {
			l.closeRelayed()
		}
	}
	if d.ext == nil {
		d.ext = map[string]map[string]*listener{}
	}
	if len(cur) == 0 {
		delete(d.ext, owner)
	} else {
		d.ext[owner] = cur
	}
	if !d.closed && d.ctx != nil {
		d.syncLAN()
	}
}

// sameFrontend reports whether a and b serve alike.
func sameFrontend(a, b emu.LANFrontend) bool {
	return a.Key == b.Key && a.Name == b.Name && a.What == b.What && a.Port == b.Port &&
		a.Addr == b.Addr && a.Wildcard == b.Wildcard && a.Sig == b.Sig
}

// LANClaims implements emu.LANFrontends.
func (s *Service) LANClaims(owner string) map[string][]string {
	d := s.dp
	d.mu.Lock()
	defer d.mu.Unlock()
	out := map[string][]string{}
	for addr, b := range d.lan {
		if l := d.ext[owner][b.key]; l != nil && l == b.l {
			out[b.key] = append(out[b.key], addr)
		}
	}
	for _, addrs := range out {
		sort.Strings(addrs)
	}
	return out
}

// closeRelayed stops serving the connections relayed to another
// Service's frontend; relayed opens them again on next use.
func (l *listener) closeRelayed() {
	if l.virt != nil {
		l.virt.Close()
		l.virt = nil
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
		return nil, d.s.env.ExplainBind(d.ctx, err)
	case loopback.Excluded:
		return nil, loopback.ExcludedError(err, "tcp")
	case loopback.PrivilegedWildcard:
		return nil, fmt.Errorf("%w (privileged port: only the frontend with the wildcard listener on it is served on --lb-bind addresses)", err)
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

// lanFailed records that l cannot serve the --lb-bind address addr, and
// logs it once.
func (d *dataplane) lanFailed(addr string, l *listener, err error) {
	k := addr + " " + l.key
	d.lanDown[k] = lanFailure{key: l.key, what: l.lanWhat(), addr: addr, err: err.Error()}
	if d.lanErr[k] == err.Error() {
		return
	}
	d.lanErr[k] = err.Error()
	d.s.env.Log.Warn("lb: cannot expose on --lb-bind address", "frontend", l.lanWhat(), "address", addr, "err", err)
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
		d.releaseLAN(addr, b)
	}
	for _, fes := range d.ext {
		for _, l := range fes {
			l.closeRelayed()
		}
	}
	d.relay.Close(ctx)
}
