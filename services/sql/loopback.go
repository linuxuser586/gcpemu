package sql

import (
	"context"
	"errors"
	"hash/fnv"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/loopback"
	"github.com/linuxuser586/gcpemu/internal/netplane"
	"github.com/linuxuser586/gcpemu/internal/runtime"
	"github.com/linuxuser586/gcpemu/internal/store"
	"github.com/linuxuser586/gcpemu/services/sql/proxy"
)

// Host addresses on a VM runtime (ADR 0005). The host cannot reach an
// instance's public IP on a bridge network inside the VM, so an instance
// with a public IP reports a loopback address as its PRIMARY address
// instead, on which the emulator serves ports 3307 and 5432 and pipes
// each connection through the VM relay to the container. Every connector
// dials port 3307 and most send no TLS server name, so instances cannot
// share an address: each gets its own 127.0.0.x, allocated from a hash of
// its connection name. A host without loopback aliases (macOS before
// `gcpemu loopback install`) has only 127.0.0.1, which one instance at a
// time holds.
//
// A Containerized Instance on a VM runtime starts no relay, so it
// publishes the instance container's ports 3307 and 5432 on the address
// instead (ADR 0012). Its container can't see the host's aliases, so
// runtime.Manager.LoopbackAliases says whether to try them.
//
// On both paths 3307 is required on the address and 5432 best-effort, so
// that a local PostgreSQL on 127.0.0.1:5432 doesn't block connectors.

// loopbackHost is the one loopback address every host has.
const loopbackHost = "127.0.0.1"

// loopPorts are the ports served on an instance's loopback address.
var loopPorts = []int{proxy.ConnectPort, proxy.PublicPort}

// errNoLoopback refuses a second instance where only 127.0.0.1 is
// usable: on a host without aliases, or in a Containerized Instance that
// doesn't use them.
func errNoLoopback(holder string, containerized bool) error {
	if containerized {
		return apierr.FailedPrecondition("Cloud SQL instance %s already serves host clients on %s:%d, the only loopback address this Containerized Instance uses. "+
			"Add loopback aliases on the host (`%s`, from the native binary on macOS) and, unless the runtime is Docker Desktop, set %s=true to give each instance its own address.",
			holder, loopbackHost, proxy.ConnectPort, loopback.InstallHint, runtime.LoopbackAliasesEnv)
	}
	return apierr.FailedPrecondition("Cloud SQL instance %s already serves host clients on %s:%d, the only loopback address this host has. Run `%s` to give each instance its own address.",
		holder, loopbackHost, proxy.ConnectPort, loopback.InstallHint)
}

// relayOn reports whether the Instance uses the VM relay. The caller has
// had the plane create the services network, which decides it.
func relayOn(plane *netplane.Plane) bool {
	d, ok := plane.RelayDecision()
	return ok && d.On
}

// hostClients reports whether instances serve host clients on a
// loopback address; emutest turns it off (config.NoSQLHostClients).
func (s *Service) hostClients() bool { return !s.env.Config.NoSQLHostClients }

// loopHolder returns the connection name of the instance other than
// project/name that holds addr, or "".
func (s *Service) loopHolder(project, name, addr string) string {
	var holder string
	_ = s.env.Store.View(func(tx store.Tx) error {
		scanJSON(tx, nsInstances, "", func(k string, rec *instanceRecord) {
			if k != instKey(project, name) && rec.LoopIP == addr && rec.Instance != nil {
				holder = rec.Instance.ConnectionName
			}
		})
		return nil
	})
	return holder
}

// containerAliases reports whether a Containerized Instance on a VM
// runtime tries loopback aliases: as rt.LoopbackAliases says, until the
// runtime has refused one.
func (s *Service) containerAliases(rt *runtime.Manager) bool {
	return rt.LoopbackAliases.On && !s.aliasRefused.Load()
}

// checkLoopback fails early, with FAILED_PRECONDITION, a create or patch
// that would start a second public instance where only 127.0.0.1 is
// usable: on a VM runtime when the host has no loopback aliases, or in a
// Containerized Instance on one that doesn't use them. startContainer
// checks again.
func (s *Service) checkLoopback(ctx context.Context, project, name string) error {
	if !s.hostClients() {
		return nil
	}
	holder := s.loopHolder(project, name, loopbackHost)
	if holder == "" {
		return nil
	}
	rt, plane, err := s.plane(ctx)
	if err != nil {
		return nil // reported by the operation
	}
	if rt.VM.On {
		if s.containerAliases(rt) {
			return nil
		}
		return errNoLoopback(holder, true)
	}
	if _, err := plane.Services(ctx); err != nil || !relayOn(plane) || s.aliases() {
		return nil
	}
	return errNoLoopback(holder, false)
}

// LoopbackAddrs implements emu.LoopbackHolder. It reads only the store,
// so that LB may call it while holding its own lock.
func (s *Service) LoopbackAddrs() []string {
	var out []string
	_ = s.env.Store.View(func(tx store.Tx) error {
		scanJSON(tx, nsInstances, "", func(_ string, rec *instanceRecord) {
			if rec.LoopIP != "" {
				out = append(out, rec.LoopIP)
			}
		})
		return nil
	})
	return out
}

// loopCandidates lists the addresses to try for an instance, in order:
// its previous one, then with aliases 127.0.0.x from x = hash of the
// connection name onwards (x in 2-254), skipping those that other
// instances, LB forwarding rules and GKE LoadBalancer Services hold;
// without them only 127.0.0.1.
func (s *Service) loopCandidates(rec *instanceRecord, aliases bool) []string {
	project, name := rec.Instance.Project, rec.Instance.Name
	if !aliases {
		if h := s.loopHolder(project, name, loopbackHost); h != "" {
			return nil
		}
		return []string{loopbackHost}
	}
	used := map[string]bool{}
	_ = s.env.Store.View(func(tx store.Tx) error {
		scanJSON(tx, nsInstances, "", func(k string, r *instanceRecord) {
			if k != instKey(project, name) && r.LoopIP != "" {
				used[r.LoopIP] = true
			}
		})
		return nil
	})
	for _, peer := range []string{"lb", "gke"} {
		if h, ok := s.peer(peer).(emu.LoopbackHolder); ok {
			for _, a := range h.LoopbackAddrs() {
				used[a] = true
			}
		}
	}
	var out []string
	if rec.LoopIP != "" && rec.LoopIP != loopbackHost && !used[rec.LoopIP] {
		out = append(out, rec.LoopIP)
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(rec.Instance.ConnectionName))
	start := int(h.Sum32() % 253)
	for i := range 253 {
		a := "127.0.0." + strconv.Itoa((start+i)%253+2)
		if !used[a] && a != rec.LoopIP {
			out = append(out, a)
		}
	}
	return out
}

// exposeLoopback serves the instance's loopback address, piping ports
// 3307 and 5432 to its public IP through the VM relay, and records the
// address. An address on which another process holds 3307 is skipped;
// one where it holds only 5432 is served without it.
func (s *Service) exposeLoopback(plane *netplane.Plane, rec *instanceRecord, publicIP string) (string, error) {
	project, name := rec.Instance.Project, rec.Instance.Name
	s.loopMu.Lock()
	defer s.loopMu.Unlock()
	s.closeLoopbackLocked(instKey(project, name))
	aliases := s.aliases()
	cands := s.loopCandidates(rec, aliases)
	if len(cands) == 0 {
		return "", errNoLoopback(s.loopHolder(project, name, loopbackHost), false)
	}
	var errs []error
	for _, addr := range cands {
		ex, err := exposeAll(s.env.Log, plane, addr, publicIP)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if err := s.updateRecord(project, name, func(r *instanceRecord) error {
			r.LoopIP = addr
			return nil
		}); err != nil {
			closeAll(ex)
			return "", err
		}
		if rec.LoopIP != "" && rec.LoopIP != addr {
			s.env.Log.Warn("sql: loopback address taken; moved", "instance", rec.Instance.ConnectionName, "from", rec.LoopIP, "to", addr)
		}
		s.exposed[instKey(project, name)] = ex
		return addr, nil
	}
	if !aliases {
		return "", newOpError("INTERNAL_ERROR", "serving %s for host clients: %v (run `%s` to use other loopback addresses)", loopbackHost, errors.Join(errs...), loopback.InstallHint)
	}
	return "", newOpError("INTERNAL_ERROR", "no free loopback address for host clients: %v", errors.Join(errs...))
}

// exposeAll binds the loop ports on addr: 3307 or nothing, and 5432
// when it is free.
func exposeAll(log *slog.Logger, plane *netplane.Plane, addr, publicIP string) ([]*netplane.Exposed, error) {
	var out []*netplane.Exposed
	for _, port := range loopPorts {
		e, err := plane.Expose(net.JoinHostPort(addr, strconv.Itoa(port)), net.JoinHostPort(publicIP, strconv.Itoa(port)), false)
		if err != nil && port == proxy.PublicPort {
			log.Warn("sql: port 5432 is taken; serving host clients on 3307 only", "addr", addr, "err", err)
			continue
		}
		if err != nil {
			closeAll(out)
			return nil, err
		}
		out = append(out, e)
	}
	return out, nil
}

// loopPublish plans the ports a Containerized Instance on a VM runtime
// publishes for an instance's host clients (ADR 0012): 3307, and 5432
// while it is free, on the first usable of its loopback candidates.
// Port bindings fail only when the container starts, so startContainer
// asks retry what each failure means.
type loopPublish struct {
	aliases  bool
	cands    func(aliases bool) []string // loopCandidates
	noLoop   func() error                // errNoLoopback naming the holder
	addrs    []string
	with5432 bool
	// refused says the runtime refused an alias, so the Instance uses only
	// 127.0.0.1 from now on.
	refused bool
}

func newLoopPublish(aliases bool, cands func(bool) []string, noLoop func() error) (*loopPublish, error) {
	p := &loopPublish{aliases: aliases, cands: cands, noLoop: noLoop}
	return p, p.reset()
}

// reset starts again from the first candidate.
func (p *loopPublish) reset() error {
	p.addrs, p.with5432 = p.cands(p.aliases), true
	if len(p.addrs) == 0 {
		return p.noLoop()
	}
	return nil
}

// addr is the address the next attempt publishes on.
func (p *loopPublish) addr() string { return p.addrs[0] }

// ports are the bindings the next attempt publishes.
func (p *loopPublish) ports() []runtime.Port {
	out := []runtime.Port{{HostIP: p.addr(), HostPort: proxy.ConnectPort, ContainerPort: proxy.ConnectPort}}
	if p.with5432 {
		out = append(out, runtime.Port{HostIP: p.addr(), HostPort: proxy.PublicPort, ContainerPort: proxy.PublicPort})
	}
	return out
}

// retry reports whether err, from starting the container, was about the
// plan's bindings and the plan has changed so that another attempt may
// succeed. It returns an error when no address is left, and false and nil
// when err is about something else.
func (p *loopPublish) retry(err error) (bool, error) {
	msg := strings.ToLower(err.Error())
	addr := p.addr()
	switch {
	case p.aliases && strings.Contains(msg, "assign requested address"):
		// Docker Desktop and Podman machine refuse a missing alias.
		p.aliases, p.refused = false, true
		if err := p.reset(); err != nil {
			return false, err
		}
		return true, nil
	case p.with5432 && mentions(msg, addr, proxy.PublicPort):
		p.with5432 = false
		return true, nil
	case mentions(msg, addr, proxy.ConnectPort):
		if p.addrs = p.addrs[1:]; len(p.addrs) > 0 {
			p.with5432 = true
			return true, nil
		}
		if !p.aliases {
			return false, newOpError("INTERNAL_ERROR", "serving %s:%d for host clients: %v", loopbackHost, proxy.ConnectPort, err)
		}
		return false, newOpError("INTERNAL_ERROR", "no free loopback address for host clients: %v", err)
	}
	return false, nil
}

// mentions reports whether msg names addr:port, and not a longer address
// or port that starts or ends with it (127.0.0.1:54321).
func mentions(msg, addr string, port int) bool {
	hp := net.JoinHostPort(addr, strconv.Itoa(port))
	for i := 0; ; {
		j := strings.Index(msg[i:], hp)
		if j < 0 {
			return false
		}
		start, end := i+j, i+j+len(hp)
		if (start == 0 || !isAddrByte(msg[start-1])) && (end == len(msg) || !isAddrByte(msg[end])) {
			return true
		}
		i = start + 1
	}
}

func isAddrByte(b byte) bool { return b >= '0' && b <= '9' || b == '.' }

func closeAll(ex []*netplane.Exposed) {
	for _, e := range ex {
		_ = e.Close()
	}
}

// closeLoopback stops serving an instance's loopback address; the
// instance keeps holding it.
func (s *Service) closeLoopback(project, name string) {
	s.loopMu.Lock()
	defer s.loopMu.Unlock()
	s.closeLoopbackLocked(instKey(project, name))
}

func (s *Service) closeLoopbackLocked(key string) {
	closeAll(s.exposed[key])
	delete(s.exposed, key)
}

// closeAllLoopback stops serving every instance's loopback address.
func (s *Service) closeAllLoopback() {
	s.loopMu.Lock()
	defer s.loopMu.Unlock()
	for k := range s.exposed {
		s.closeLoopbackLocked(k)
	}
}

// hostInfo is how host clients reach a public instance, for `gcpemu
// doctor`: GET /_emu/v1/sql/hosts on the admin API.
type hostInfo struct {
	Instance string `json:"instance"`
	Address  string `json:"address,omitempty"`
	// Loopback says the address stands in for the public IP, which the
	// host can't reach (a VM runtime).
	Loopback bool `json:"loopback,omitempty"`
	// Missing says why an instance has no address.
	Missing string `json:"missing,omitempty"`
}

// serveHosts is GET /_emu/v1/sql/hosts.
func (s *Service) serveHosts(w http.ResponseWriter, r *http.Request) {
	loop := false
	if s.hostClients() && s.env.Containers != nil {
		if rt, plane, err := s.plane(r.Context()); err == nil {
			loop = rt.VM.On || relayOn(plane)
		}
	}
	writeJSON(w, r, s.serveHostsBody(loop))
}

// serveHostsBody is the body of GET /_emu/v1/sql/hosts: the instances,
// and whether the runtime has refused a loopback alias, so that only
// 127.0.0.1 is used whatever the decision said.
func (s *Service) serveHostsBody(loop bool) map[string]any {
	return map[string]any{"instances": s.hostsReport(loop), "aliasesRefused": s.aliasRefused.Load()}
}

// hostsReport lists the running public instances' host addresses: their
// loopback addresses when loop, otherwise their public IPs.
func (s *Service) hostsReport(loop bool) []hostInfo {
	out := []hostInfo{}
	_ = s.env.Store.View(func(tx store.Tx) error {
		scanJSON(tx, nsInstances, "", func(_ string, rec *instanceRecord) {
			in := rec.Instance
			if in == nil || in.Settings == nil || in.Settings.IpConfiguration == nil || !in.Settings.IpConfiguration.Ipv4Enabled ||
				in.Settings.ActivationPolicy == "NEVER" {
				return
			}
			h := hostInfo{Instance: in.ConnectionName, Loopback: loop}
			switch {
			case in.State != "RUNNABLE":
				h.Missing = "the instance is " + in.State
			case loop && rec.LoopIP == "":
				h.Missing = "it holds no loopback address; restarting it tries again"
			case loop:
				h.Address = rec.LoopIP
			case rec.PublicIP == "":
				h.Missing = "it has no public IP"
			default:
				h.Address = rec.PublicIP
			}
			out = append(out, h)
		})
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Instance < out[j].Instance })
	return out
}
