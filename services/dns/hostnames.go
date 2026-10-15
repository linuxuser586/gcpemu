package dns

import (
	"context"
	"strings"

	mdns "github.com/miekg/dns"
)

// Host names (ADR 0014): GKE answers host queries for Kubernetes Service
// names under svc.cluster.local and gke.internal, which no zone holds.
// Queries from a VPC network (GKE nodes resolve those names with CoreDNS)
// and Resolve never reach it. A name under one of its suffixes is never
// forwarded: a host resolver that sends the suffix here would send it
// straight back.

// hostNamer is the installed function and the suffixes it owns.
type hostNamer struct {
	suffixes []string
	f        func(ctx context.Context, name string, qtype uint16) ([]mdns.RR, int)
}

// SetHostNames installs f to answer host queries for names under suffixes
// (lower case, fully qualified): f returns the answer and its rcode, and a
// CNAME it returns is chased like one from a zone. A nil f removes it.
func (s *Service) SetHostNames(suffixes []string, f func(ctx context.Context, name string, qtype uint16) ([]mdns.RR, int)) {
	if f == nil {
		s.hostNames.Store(nil)
		return
	}
	s.hostNames.Store(&hostNamer{suffixes: suffixes, f: f})
}

// owns reports whether name, lower case, lies under one of h's suffixes.
func (h *hostNamer) owns(name string) bool {
	for _, sfx := range h.suffixes {
		if name == sfx || strings.HasSuffix(name, "."+sfx) {
			return true
		}
	}
	return false
}

// answerHost answers a query from the host: from the installed host names
// for a name they own, from the zones otherwise, chasing CNAMEs between
// them.
func (s *Service) answerHost(ctx context.Context, q mdns.Question) *mdns.Msg {
	h := s.hostNames.Load()
	name := strings.ToLower(mdns.Fqdn(q.Name))
	if h == nil || !h.owns(name) {
		return s.answer(ctx, "", q)
	}
	out := &mdns.Msg{}
	out.Authoritative = true
	seen := map[string]bool{}
	for hop := 0; ; hop++ {
		rrs, rcode := h.f(ctx, name, q.Qtype)
		out.Answer = append(out.Answer, rrs...)
		out.Rcode = rcode
		target := ""
		if n := len(rrs); n > 0 {
			if c, ok := rrs[n-1].(*mdns.CNAME); ok {
				target = strings.ToLower(c.Target)
			}
		}
		if rcode != mdns.RcodeSuccess || target == "" || q.Qtype == mdns.TypeCNAME || q.Qtype == mdns.TypeANY {
			return out
		}
		seen[name] = true
		if seen[target] || hop >= maxCNAMEChain {
			return out
		}
		name = target
		if !h.owns(name) {
			rest := s.answer(ctx, "", mdns.Question{Name: name, Qtype: q.Qtype, Qclass: mdns.ClassINET})
			out.Answer = append(out.Answer, rest.Answer...)
			out.Rcode = rest.Rcode
			return out
		}
	}
}
