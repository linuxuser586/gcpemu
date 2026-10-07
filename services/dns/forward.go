package dns

import (
	"context"
	"net"
	"time"

	mdns "github.com/miekg/dns"
)

// resolvConf is the host resolver configuration (a variable for tests).
var resolvConf = "/etc/resolv.conf"

// hostResolvers returns the upstream servers for names outside every zone:
// the host's resolv.conf nameservers, else 8.8.8.8 unless --offline.
func (s *Service) hostResolvers() []string {
	var out []string
	if cc, err := mdns.ClientConfigFromFile(resolvConf); err == nil {
		for _, srv := range cc.Servers {
			out = append(out, net.JoinHostPort(srv, cc.Port))
		}
	}
	if len(out) == 0 && !s.env.Config.Offline {
		out = []string{"8.8.8.8:53"}
	}
	return out
}

// forward sends q to the host resolver. It returns REFUSED when forwarding
// is disabled and SERVFAIL when no upstream answers.
func (s *Service) forward(ctx context.Context, q mdns.Question) *mdns.Msg {
	out := new(mdns.Msg)
	if s.env.Config.DNSNoForward {
		out.Rcode = mdns.RcodeRefused
		return out
	}
	out.Rcode = mdns.RcodeServerFailure
	self := s.Addr()
	m := new(mdns.Msg)
	m.SetQuestion(q.Name, q.Qtype)
	m.RecursionDesired = true
	m.SetEdns0(4096, false)
	for _, up := range s.upstreams {
		if up == self {
			continue // never forward to ourselves
		}
		r, err := exchange(ctx, m, up)
		if err != nil || r == nil {
			continue
		}
		out.Rcode = r.Rcode
		out.Answer, out.Ns = r.Answer, r.Ns
		for _, rr := range r.Extra {
			if rr.Header().Rrtype != mdns.TypeOPT {
				out.Extra = append(out.Extra, rr)
			}
		}
		return out
	}
	return out
}

// exchange queries an upstream over UDP, retrying over TCP on truncation.
func exchange(ctx context.Context, m *mdns.Msg, addr string) (*mdns.Msg, error) {
	c := &mdns.Client{Net: "udp", Timeout: 2 * time.Second}
	r, _, err := c.ExchangeContext(ctx, m, addr)
	if err == nil && r != nil && r.Truncated {
		c.Net = "tcp"
		r, _, err = c.ExchangeContext(ctx, m, addr)
	}
	return r, err
}
