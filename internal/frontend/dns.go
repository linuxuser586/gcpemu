package frontend

import (
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	mdns "github.com/miekg/dns"
)

// DNS is a small resolver that answers the names a relay serves with its
// own address and relays everything else upstream. The GKE node agent
// (cluster DNS upstream) and the host-mode container use it so that
// *.googleapis.com names the emulator serves resolve to their 443 relay.
type DNS struct {
	// Answer returns the IPv4 address for name (normalized, no trailing
	// dot), or nil when the name is relayed.
	Answer func(name string) net.IP
	// Upstreams returns the servers (host:port) a name is relayed to;
	// none yields SERVFAIL.
	Upstreams func(name string) []string
	// TTL of synthesized answers (default 60 s).
	TTL uint32
	// Network, when set, tags relayed queries with the VPC network they
	// come from (NetworkOption).
	Network string
}

// ServeDNS implements mdns.Handler.
func (d *DNS) ServeDNS(w mdns.ResponseWriter, req *mdns.Msg) {
	if len(req.Question) == 1 && d.Answer != nil {
		q := req.Question[0]
		if ip := d.Answer(Normalize(q.Name)); ip != nil {
			_ = w.WriteMsg(d.synth(req, q, ip))
			return
		}
	}
	name := ""
	if len(req.Question) > 0 {
		name = Normalize(req.Question[0].Name)
	}
	var ups []string
	if d.Upstreams != nil {
		ups = d.Upstreams(name)
	}
	proto := "udp"
	if _, ok := w.RemoteAddr().(*net.TCPAddr); ok {
		proto = "tcp"
	}
	out := req
	if d.Network != "" {
		out = SetNetwork(req, d.Network)
	}
	for _, up := range ups {
		c := &mdns.Client{Net: proto, Timeout: 5 * time.Second}
		resp, _, err := c.Exchange(out, up)
		if err == nil && resp.Truncated && proto == "udp" {
			c.Net = "tcp"
			resp, _, err = c.Exchange(out, up)
		}
		if err == nil {
			_ = w.WriteMsg(resp)
			return
		}
	}
	m := new(mdns.Msg)
	m.SetRcode(req, mdns.RcodeServerFailure)
	_ = w.WriteMsg(m)
}

// synth answers q for a served name: an A record for A queries, NODATA
// for every other type (so clients fall back to IPv4).
func (d *DNS) synth(req *mdns.Msg, q mdns.Question, ip net.IP) *mdns.Msg {
	m := new(mdns.Msg)
	m.SetReply(req)
	m.Authoritative = true
	m.RecursionAvailable = true
	ttl := d.TTL
	if ttl == 0 {
		ttl = 60
	}
	if q.Qtype == mdns.TypeA || q.Qtype == mdns.TypeANY {
		if v4 := ip.To4(); v4 != nil {
			m.Answer = append(m.Answer, &mdns.A{
				Hdr: mdns.RR_Header{Name: q.Name, Rrtype: mdns.TypeA, Class: mdns.ClassINET, Ttl: ttl},
				A:   v4,
			})
		}
	}
	return m
}

// ListenDNS serves h on addr over UDP and TCP and returns once both are
// listening; stop shuts them down.
func ListenDNS(addr string, h mdns.Handler) (stop func(), err error) {
	udp := &mdns.Server{Addr: addr, Net: "udp", Handler: h}
	tcp := &mdns.Server{Addr: addr, Net: "tcp", Handler: h}
	errc := make(chan error, 2)
	var started sync.WaitGroup
	started.Add(2)
	udp.NotifyStartedFunc = started.Done
	tcp.NotifyStartedFunc = started.Done
	go func() { errc <- udp.ListenAndServe() }()
	go func() { errc <- tcp.ListenAndServe() }()
	ok := make(chan struct{})
	go func() { started.Wait(); close(ok) }()
	select {
	case <-ok:
	case err := <-errc:
		_ = udp.Shutdown()
		_ = tcp.Shutdown()
		return nil, fmt.Errorf("dns on %s: %w", addr, err)
	case <-time.After(5 * time.Second):
		return nil, errors.New("dns server did not start on " + addr)
	}
	return func() { _ = udp.Shutdown(); _ = tcp.Shutdown() }, nil
}
