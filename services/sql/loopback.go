package sql

import (
	"context"
	"errors"
	"hash/fnv"
	"net"
	"strconv"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/loopback"
	"github.com/linuxuser586/gcpemu/internal/netplane"
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

// loopbackHost is the one loopback address every host has.
const loopbackHost = "127.0.0.1"

// loopPorts are the ports served on an instance's loopback address.
var loopPorts = []int{proxy.ConnectPort, proxy.PublicPort}

// errNoLoopback refuses a second instance on a host without aliases.
func errNoLoopback(holder string) error {
	return apierr.FailedPrecondition("Cloud SQL instance %s already serves host clients on %s:%d, the only loopback address this host has. Run `%s` to give each instance its own address.",
		holder, loopbackHost, proxy.ConnectPort, loopback.InstallHint)
}

// relayOn reports whether the Instance uses the VM relay. The caller has
// had the plane create the services network, which decides it.
func relayOn(plane *netplane.Plane) bool {
	d, ok := plane.RelayDecision()
	return ok && d.On
}

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

// checkLoopback fails early, with FAILED_PRECONDITION, a create or patch
// that would start a second public instance on a VM runtime when the
// host has no loopback aliases. startContainer checks again.
func (s *Service) checkLoopback(ctx context.Context, project, name string) error {
	holder := s.loopHolder(project, name, loopbackHost)
	if holder == "" {
		return nil
	}
	_, plane, err := s.plane(ctx)
	if err != nil {
		return nil // reported by the operation
	}
	if _, err := plane.Services(ctx); err != nil || !relayOn(plane) || s.aliases() {
		return nil
	}
	return errNoLoopback(holder)
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
// address. An address on which another process holds either port is
// skipped.
func (s *Service) exposeLoopback(plane *netplane.Plane, rec *instanceRecord, publicIP string) (string, error) {
	project, name := rec.Instance.Project, rec.Instance.Name
	s.loopMu.Lock()
	defer s.loopMu.Unlock()
	s.closeLoopbackLocked(instKey(project, name))
	aliases := s.aliases()
	cands := s.loopCandidates(rec, aliases)
	if len(cands) == 0 {
		return "", errNoLoopback(s.loopHolder(project, name, loopbackHost))
	}
	var errs []error
	for _, addr := range cands {
		ex, err := exposeAll(plane, addr, publicIP)
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

// exposeAll binds every loop port on addr, or none.
func exposeAll(plane *netplane.Plane, addr, publicIP string) ([]*netplane.Exposed, error) {
	var out []*netplane.Exposed
	for _, port := range loopPorts {
		e, err := plane.Expose(net.JoinHostPort(addr, strconv.Itoa(port)), net.JoinHostPort(publicIP, strconv.Itoa(port)), false)
		if err != nil {
			closeAll(out)
			return nil, err
		}
		out = append(out, e)
	}
	return out, nil
}

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
