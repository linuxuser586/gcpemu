package dns

import (
	"context"
	"net"
	"os"
	"slices"
	"sync"
	"time"

	mdns "github.com/miekg/dns"
)

// resolvConf is the host resolver configuration, and publicResolver the
// resolver tried when none of the host's answer (variables for tests).
var (
	resolvConf     = "/etc/resolv.conf"
	publicResolver = "8.8.8.8:53"
)

// skipFor is how long a host resolver that failed to answer is skipped.
const skipFor = 30 * time.Second

// forwarder holds the host's resolvers, which follow resolv.conf: it is
// read again whenever its modification time or size changes, as when the
// host moves networks (#1196). A resolver that fails to answer (an
// unreachable one times out) is skipped for skipFor, so lookups don't wait
// on it before reaching a working one later in the list.
type forwarder struct {
	path    string
	offline bool
	now     func() time.Time

	mu      sync.Mutex
	stat    os.FileInfo // resolv.conf when last read; nil if missing
	servers []string
	skip    map[string]time.Time // resolver → end of its skip window
}

// startForwarding sets up forwarding to the host's resolvers. Until it
// runs (Start), names outside every zone answer SERVFAIL.
func (s *Service) startForwarding() {
	s.fwd.Store(&forwarder{path: resolvConf, offline: s.env.Config.Offline, now: time.Now})
}

// hostResolvers returns the host's resolvers, reading resolv.conf again
// if it changed since the last call.
func (f *forwarder) hostResolvers() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	st, err := os.Stat(f.path)
	if err != nil {
		st = nil
	}
	if f.servers != nil && unchangedConf(st, f.stat) {
		return f.servers
	}
	f.stat = st
	f.servers = []string{}
	f.skip = nil // a new network's resolvers start afresh
	if cc, err := mdns.ClientConfigFromFile(f.path); err == nil {
		for _, srv := range cc.Servers {
			f.servers = append(f.servers, serverAddr(srv, cc.Port))
		}
	}
	return f.servers
}

// serverAddr is the address of resolv.conf's nameserver srv: an IP
// address on port, or srv itself when it already names a port (IP:port,
// which tests write; resolv.conf has no such form).
func serverAddr(srv, port string) string {
	if h, _, err := net.SplitHostPort(srv); err == nil && net.ParseIP(h) != nil {
		return srv
	}
	return net.JoinHostPort(srv, port)
}

// unchangedConf reports whether a and b, resolv.conf at two times, have
// the same modification time and size.
func unchangedConf(a, b os.FileInfo) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.ModTime().Equal(b.ModTime()) && a.Size() == b.Size()
}

// upstreams lists the resolvers to try in order: the host's outside their
// skip windows (all of them when every one is in its window), then, unless
// --offline, the public resolver for when none of the host's answer.
func (f *forwarder) upstreams() []string {
	host := f.hostResolvers()
	f.mu.Lock()
	now := f.now()
	var out []string
	for _, h := range host {
		if until, ok := f.skip[h]; !ok || !now.Before(until) {
			out = append(out, h)
		}
	}
	f.mu.Unlock()
	if len(out) == 0 {
		out = host
	}
	if !f.offline && !slices.Contains(out, publicResolver) {
		out = append(slices.Clip(out), publicResolver)
	}
	return out
}

// failed starts addr's skip window.
func (f *forwarder) failed(addr string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.skip == nil {
		f.skip = map[string]time.Time{}
	}
	f.skip[addr] = f.now().Add(skipFor)
}

// status reports, for GET /_emu/v1/dns/forwarding, the host's resolvers
// and the public one when it is used.
func (f *forwarder) status() forwardingStatus {
	out := forwardingStatus{Offline: f.offline, Resolvers: slices.Clone(f.hostResolvers())}
	if !f.offline {
		out.Public = publicResolver
	}
	return out
}

// upstreamTimeout is the longest one upstream is waited for.
const upstreamTimeout = 2 * time.Second

// forward sends q to the host resolver. It returns REFUSED when forwarding
// is disabled and SERVFAIL when no upstream answers.
//
// The caller's deadline is shared among the upstreams still to try, each
// waited for at most upstreamTimeout, so the public resolver at the end
// of the list is reached even behind several dead ones. An upstream that
// doesn't answer within its share is skipped from then on (forwarder).
func (s *Service) forward(ctx context.Context, q mdns.Question) *mdns.Msg {
	out := new(mdns.Msg)
	if s.env.Config.DNSNoForward {
		out.Rcode = mdns.RcodeRefused
		return out
	}
	out.Rcode = mdns.RcodeServerFailure
	f := s.fwd.Load()
	if f == nil {
		return out
	}
	self := s.Addr()
	m := new(mdns.Msg)
	m.SetQuestion(q.Name, q.Qtype)
	m.RecursionDesired = true
	m.SetEdns0(4096, false)
	ups := slices.DeleteFunc(slices.Clone(f.upstreams()), func(up string) bool {
		return up == self // never forward to ourselves
	})
	for i, up := range ups {
		wait := upstreamTimeout
		if dl, ok := ctx.Deadline(); ok {
			wait = min(wait, time.Until(dl)/time.Duration(len(ups)-i))
		}
		r, err := exchange(ctx, m, up, wait)
		if err != nil || r == nil {
			if ctx.Err() == nil { // the upstream failed, not the caller's wait
				f.failed(up)
			}
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

// exchange queries an upstream over UDP, retrying over TCP on truncation,
// waiting at most wait for each.
func exchange(ctx context.Context, m *mdns.Msg, addr string, wait time.Duration) (*mdns.Msg, error) {
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	c := &mdns.Client{Net: "udp", Timeout: wait}
	r, _, err := c.ExchangeContext(ctx, m, addr)
	if err == nil && r != nil && r.Truncated {
		c.Net = "tcp"
		r, _, err = c.ExchangeContext(ctx, m, addr)
	}
	return r, err
}
