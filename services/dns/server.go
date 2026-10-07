package dns

import (
	"context"
	"errors"
	"net"
	"strings"
	"time"

	mdns "github.com/miekg/dns"
)

// Authoritative data plane (FR-DNS-003/004). Every zone in the store is
// served, public and private alike: network-binding enforcement for private
// zones arrives with GKE (FR-INT-005), so for now host queries see private
// zones too (a private zone wins over a public one with the same name).
// Names outside every zone are forwarded to the host resolver unless
// --dns-no-forward is set, in which case the answer is REFUSED.

// Errors returned by Resolve.
var (
	ErrNXDomain = errors.New("dns: name does not exist")
	ErrRefused  = errors.New("dns: query refused")
	ErrServFail = errors.New("dns: server failure")
)

const maxCNAMEChain = 8

// Resolve answers a query the way the data plane would and returns the
// answer section. A name that does not exist yields ErrNXDomain; a name
// outside every zone is forwarded (ErrRefused when forwarding is off).
// NODATA is an empty slice with a nil error. Other services (GKE CoreDNS,
// LB) use it to resolve emulated names in-process.
func (s *Service) Resolve(ctx context.Context, name string, qtype uint16) ([]mdns.RR, error) {
	m := s.answer(ctx, mdns.Question{Name: mdns.Fqdn(name), Qtype: qtype, Qclass: mdns.ClassINET})
	switch m.Rcode {
	case mdns.RcodeSuccess:
		return m.Answer, nil
	case mdns.RcodeNameError:
		return m.Answer, ErrNXDomain
	case mdns.RcodeRefused:
		return nil, ErrRefused
	default:
		return nil, ErrServFail
	}
}

// serveDNS is the miekg/dns handler for UDP and TCP.
func (s *Service) serveDNS(w mdns.ResponseWriter, req *mdns.Msg) {
	resp := new(mdns.Msg)
	switch {
	case req.Opcode != mdns.OpcodeQuery:
		resp.SetRcode(req, mdns.RcodeNotImplemented)
	case len(req.Question) != 1:
		resp.SetRcode(req, mdns.RcodeFormatError)
	case req.Question[0].Qclass != mdns.ClassINET && req.Question[0].Qclass != mdns.ClassANY:
		resp.SetRcode(req, mdns.RcodeRefused)
	default:
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		a := s.answer(ctx, req.Question[0])
		cancel()
		resp.SetReply(req)
		resp.Rcode = a.Rcode
		resp.Authoritative = a.Authoritative
		resp.RecursionAvailable = !s.env.Config.DNSNoForward
		resp.Answer, resp.Ns, resp.Extra = a.Answer, a.Ns, a.Extra
		// Echo the query's letter case on the owner (DNS 0x20 resolvers).
		qn := req.Question[0].Name
		for _, rr := range resp.Answer {
			if strings.EqualFold(rr.Header().Name, qn) {
				rr.Header().Name = qn
			}
		}
	}
	size := mdns.MinMsgSize
	if opt := req.IsEdns0(); opt != nil {
		if opt.UDPSize() > uint16(size) {
			size = int(opt.UDPSize())
		}
		resp.SetEdns0(uint16(size), false)
	}
	if _, udp := w.RemoteAddr().(*net.UDPAddr); udp {
		resp.Truncate(size)
	} else {
		resp.Compress = true
	}
	if err := w.WriteMsg(resp); err != nil {
		s.env.Log.Debug("dns write", "err", err)
	}
}

// answer resolves q against the zones, chasing CNAMEs across every zone
// served and forwarding names outside them.
func (s *Service) answer(ctx context.Context, q mdns.Question) *mdns.Msg {
	ix := s.currentIndex()
	out := new(mdns.Msg)
	name := strings.ToLower(mdns.Fqdn(q.Name))
	z := ix.findZone(name)
	if z == nil {
		return s.forward(ctx, mdns.Question{Name: q.Name, Qtype: q.Qtype, Qclass: mdns.ClassINET})
	}
	out.Authoritative = true
	seen := map[string]bool{}
	for hop := 0; ; hop++ {
		r := z.lookup(name, q.Qtype)
		out.Answer = append(out.Answer, r.answer...)
		out.Ns, out.Extra, out.Rcode = r.ns, r.extra, r.rcode
		if r.referral {
			out.Authoritative = hop > 0
			return out
		}
		if r.cname == "" || q.Qtype == mdns.TypeCNAME || q.Qtype == mdns.TypeANY {
			return out
		}
		target := strings.ToLower(r.cname)
		seen[name] = true
		if seen[target] || hop >= maxCNAMEChain {
			return out
		}
		name = target
		if z = ix.findZone(name); z == nil {
			// Out-of-zone target: complete the chain via the host resolver.
			if f := s.forward(ctx, mdns.Question{Name: target, Qtype: q.Qtype, Qclass: mdns.ClassINET}); f.Rcode != mdns.RcodeRefused {
				out.Answer = append(out.Answer, f.Answer...)
				out.Ns, out.Extra, out.Rcode = nil, nil, f.Rcode
			}
			return out
		}
		// The chain continues in a zone we serve; clear the authority of
		// the intermediate step.
		out.Ns, out.Extra = nil, nil
	}
}

// lookupResult is the outcome of a lookup within one zone.
type lookupResult struct {
	answer, ns, extra []mdns.RR
	rcode             int
	cname             string // CNAME target to chase, if any
	referral          bool
}

// lookup implements the authoritative algorithm of RFC 1034 4.3.2 for one
// zone: delegations, exact matches, CNAMEs, wildcards, NODATA and NXDOMAIN.
func (z *zoneIdx) lookup(qname string, qtype uint16) lookupResult {
	// Delegation below the apex (closest to the apex first).
	var anc []string
	for n := qname; n != "" && n != z.origin; n = parent(n) {
		anc = append(anc, n)
	}
	for i := len(anc) - 1; i >= 0; i-- {
		if set := z.names[anc[i]][mdns.TypeNS]; set != nil {
			if anc[i] == qname && qtype == mdns.TypeDS {
				break
			}
			res := lookupResult{referral: true, ns: copyRRs(set.records(), "")}
			for _, rr := range res.ns {
				res.extra = append(res.extra, z.glue(rr.(*mdns.NS).Ns)...)
			}
			return res
		}
	}
	if sets, ok := z.names[qname]; ok {
		return z.fromSets(sets, qname, qtype, "")
	}
	if z.ents[qname] {
		return z.nodata()
	}
	// Wildcard at the closest encloser.
	ce := parent(qname)
	for ce != z.origin && !z.ents[ce] && z.names[ce] == nil {
		ce = parent(ce)
	}
	if sets, ok := z.names["*."+ce]; ok {
		return z.fromSets(sets, qname, qtype, qname)
	}
	res := z.nodata()
	res.rcode = mdns.RcodeNameError
	return res
}

// fromSets answers from the rrsets at one owner; owner, when set, rewrites
// synthesized wildcard records to the query name.
func (z *zoneIdx) fromSets(sets map[uint16]*rrsetIdx, qname string, qtype uint16, owner string) lookupResult {
	var res lookupResult
	switch {
	case qtype == mdns.TypeANY:
		for _, set := range sets {
			res.answer = append(res.answer, copyRRs(set.records(), owner)...)
		}
	case sets[qtype] != nil:
		res.answer = copyRRs(sets[qtype].records(), owner)
	case sets[mdns.TypeCNAME] != nil:
		res.answer = copyRRs(sets[mdns.TypeCNAME].records(), owner)
		if len(res.answer) > 0 {
			res.cname = res.answer[0].(*mdns.CNAME).Target
		}
	}
	if len(res.answer) == 0 {
		return z.nodata()
	}
	if qtype == mdns.TypeMX || qtype == mdns.TypeSRV || qtype == mdns.TypeNS {
		for _, rr := range res.answer {
			switch v := rr.(type) {
			case *mdns.MX:
				res.extra = append(res.extra, z.glue(v.Mx)...)
			case *mdns.SRV:
				res.extra = append(res.extra, z.glue(v.Target)...)
			case *mdns.NS:
				res.extra = append(res.extra, z.glue(v.Ns)...)
			}
		}
	}
	return res
}

// nodata is a NOERROR response with the SOA in the authority section, its
// TTL capped at the SOA minimum (RFC 2308).
func (z *zoneIdx) nodata() lookupResult {
	res := lookupResult{rcode: mdns.RcodeSuccess}
	if z.soa != nil {
		soa := mdns.Copy(z.soa).(*mdns.SOA)
		soa.Hdr.Ttl = min(soa.Hdr.Ttl, soa.Minttl)
		res.ns = []mdns.RR{soa}
	}
	return res
}

// glue returns in-zone A/AAAA records for a target host.
func (z *zoneIdx) glue(host string) []mdns.RR {
	h := strings.ToLower(host)
	if !inZone(h, z.origin) {
		return nil
	}
	var out []mdns.RR
	for _, t := range []uint16{mdns.TypeA, mdns.TypeAAAA} {
		if set := z.names[h][t]; set != nil {
			out = append(out, copyRRs(set.records(), "")...)
		}
	}
	return out
}

// copyRRs deep-copies records, optionally renaming the owner.
func copyRRs(in []mdns.RR, owner string) []mdns.RR {
	out := make([]mdns.RR, 0, len(in))
	for _, rr := range in {
		c := mdns.Copy(rr)
		if owner != "" {
			c.Header().Name = owner
		}
		out = append(out, c)
	}
	return out
}
