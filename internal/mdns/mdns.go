// Package mdns advertises and browses DNS-SD services over multicast DNS
// (RFC 6762, RFC 6763), over IPv4, as Linked Instances find each other on
// the LAN (ADR 0027).
//
// Port 5353 is also the default port of the dns Service, which Host mode's
// DNS forwarding sends to, and an mDNS daemon (avahi-daemon,
// mDNSResponder, Windows' DNS Client) usually holds it on the wildcard
// address. Neither side takes it from the other:
//
//   - A Responder binds the mDNS group address itself (the wildcard on
//     Windows, which cannot bind a multicast address), with SO_REUSEADDR
//     and SO_REUSEPORT, so it shares multicast queries with the daemon and
//     never receives unicast datagrams meant for the dns Service's
//     specific address. Its host name is its own (gcpemu-…), never the
//     machine's, which the daemon answers for.
//   - Browse is a one-shot querier (RFC 6762 §5.1): it queries from an
//     ephemeral port, and responders answer it by unicast to that port, so
//     browsing binds nothing on 5353.
package mdns

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/miekg/dns"
	"golang.org/x/net/ipv4"
)

// Port is the mDNS port, and Group the IPv4 group address.
var (
	Port  = 5353
	Group = net.IPv4(224, 0, 0, 251)
)

// TTLs of the records a Responder answers with (RFC 6762 §10): a host's
// addresses and SRV, and the rest. Legacy unicast answers get at most
// legacyTTL.
const (
	hostTTL   = 120
	otherTTL  = 4500
	legacyTTL = 10
)

// Service is a DNS-SD service instance to advertise.
type Service struct {
	// Instance is the instance's label, e.g. "default@office".
	Instance string
	// Type is the service type, e.g. "_gcpemu._tcp".
	Type string
	// Host is the label of the host name the service's addresses are
	// published under, in .local; it must not be the machine's own.
	Host string
	Port int
	// Addrs are the addresses to advertise. An unspecified one stands for
	// every address of every multicast-capable interface.
	Addrs []net.IP
	// TXT holds the TXT record's strings, "key=value".
	TXT []string
}

// Options configure Advertise and Browse.
type Options struct {
	// Port zero means Port.
	Port int
	// Wait is how long Browse collects answers; zero means a second, or
	// until its context is done if that is sooner.
	Wait time.Duration
	Log  *slog.Logger
}

// port is o's mDNS port.
func (o Options) port() int {
	if o.Port == 0 {
		return Port
	}
	return o.Port
}

// Entry is a browsed service instance.
type Entry struct {
	// Instance is its label, and Host the name its SRV record targets.
	Instance string
	Host     string
	Port     int
	Addrs    []net.IP
	// TXT maps its TXT record's keys, lowercased, to their values.
	TXT map[string]string
}

// iface is an interface a Responder advertises on, and the addresses it
// advertises there.
type iface struct {
	net.Interface
	addrs []net.IP
}

// Responder answers mDNS queries for a Service until Close.
type Responder struct {
	svc   Service
	port  int
	ifs   []iface
	pc    *ipv4.PacketConn
	conn  net.PacketConn
	log   *slog.Logger
	wmu   sync.Mutex // serialises SetMulticastInterface and its write
	done  chan struct{}
	close sync.Once
}

// Advertise starts answering queries for svc on the interfaces that hold
// its addresses, and announces it there.
func Advertise(svc Service, o Options) (*Responder, error) {
	ifs, err := interfacesFor(svc.Addrs)
	if err != nil {
		return nil, err
	}
	if len(ifs) == 0 {
		return nil, fmt.Errorf("no multicast-capable interface holds %v", svc.Addrs)
	}
	conn, err := listenGroup(o.port())
	if err != nil {
		return nil, fmt.Errorf("mDNS: %w", err)
	}
	pc := ipv4.NewPacketConn(conn)
	var joined []iface
	for _, ifi := range ifs {
		if err := pc.JoinGroup(&ifi.Interface, &net.UDPAddr{IP: Group}); err != nil {
			continue
		}
		joined = append(joined, ifi)
	}
	if len(joined) == 0 {
		conn.Close()
		return nil, fmt.Errorf("mDNS: cannot join %s on any interface holding %v", Group, svc.Addrs)
	}
	// Where control messages work, answer each query with the addresses
	// of the interface it came in on; elsewhere (Windows) with all.
	_ = pc.SetControlMessage(ipv4.FlagInterface, true)
	_ = pc.SetMulticastLoopback(true)
	_ = pc.SetMulticastTTL(255)
	r := &Responder{svc: svc, port: o.port(), ifs: joined, pc: pc, conn: conn, log: cmpLog(o.Log), done: make(chan struct{})}
	go r.serve()
	go r.announce()
	return r, nil
}

// Close sends a goodbye (RFC 6762 §10.1) and stops answering.
func (r *Responder) Close() error {
	var err error
	r.close.Do(func() {
		close(r.done)
		for _, ifi := range r.ifs {
			// The service type's own PTR is shared with every other
			// Instance on the host, so it is left to expire.
			r.sendMulticast(ifi, unsolicited(r.records(ifi.addrs, 0)[1:]))
		}
		err = r.conn.Close()
	})
	return err
}

// announce sends unsolicited responses twice, a second apart (RFC 6762
// §8.3), so that caches pick up the service without asking.
func (r *Responder) announce() {
	for i := range 2 {
		if i > 0 {
			select {
			case <-r.done:
				return
			case <-time.After(time.Second):
			}
		}
		for _, ifi := range r.ifs {
			r.sendMulticast(ifi, unsolicited(r.records(ifi.addrs, -1)))
		}
	}
}

// serve answers queries until the socket is closed.
func (r *Responder) serve() {
	b := make([]byte, 9000)
	for {
		n, cm, src, err := r.pc.ReadFrom(b)
		if err != nil {
			select {
			case <-r.done:
			default:
				r.log.Warn("mDNS responder stopped", "err", err)
			}
			return
		}
		var q dns.Msg
		if q.Unpack(b[:n]) != nil || q.Response || q.Opcode != dns.OpcodeQuery {
			continue
		}
		ifs := r.ifs
		if cm != nil && cm.IfIndex != 0 {
			ifs = slices.DeleteFunc(slices.Clone(r.ifs), func(i iface) bool { return i.Index != cm.IfIndex })
			if len(ifs) == 0 {
				continue
			}
		}
		r.answer(&q, src.(*net.UDPAddr), ifs)
	}
}

// answer answers q, from src, on ifs.
func (r *Responder) answer(q *dns.Msg, src *net.UDPAddr, ifs []iface) {
	legacy := src.Port != r.port
	for _, ifi := range ifs {
		all := r.records(ifi.addrs, -1)
		resp := new(dns.Msg)
		resp.Response, resp.Authoritative = true, true
		unicast := legacy
		for _, qq := range q.Question {
			ans := matching(all, qq)
			if len(ans) == 0 {
				continue
			}
			if qq.Qclass&(1<<15) != 0 {
				unicast = true // the QU bit
			}
			resp.Answer = append(resp.Answer, ans...)
		}
		if len(resp.Answer) == 0 {
			continue
		}
		// The rest of what a browser needs goes in the additional section
		// (RFC 6763 §12).
		for _, rr := range all {
			if !slices.ContainsFunc(resp.Answer, func(a dns.RR) bool { return dns.IsDuplicate(a, rr) }) {
				resp.Extra = append(resp.Extra, rr)
			}
		}
		if legacy {
			// RFC 6762 §6.7: echo the ID and question, no cache-flush bit,
			// short TTLs.
			resp.Id, resp.Question = q.Id, q.Question
			for _, rr := range slices.Concat(resp.Answer, resp.Extra) {
				h := rr.Header()
				h.Class &^= 1 << 15
				h.Ttl = min(h.Ttl, legacyTTL)
			}
		}
		if unicast {
			r.send(resp, src)
		} else {
			r.sendMulticast(ifi, resp)
		}
	}
}

// matching returns the records of all that answer q.
func matching(all []dns.RR, q dns.Question) []dns.RR {
	var out []dns.RR
	for _, rr := range all {
		h := rr.Header()
		if strings.EqualFold(h.Name, q.Name) && (q.Qtype == dns.TypeANY || q.Qtype == h.Rrtype) {
			out = append(out, dns.Copy(rr))
		}
	}
	return out
}

// names are the service's names: its type's, its instance's, and its
// host's, each in .local.
func (r *Responder) names() (typ, inst, host string) {
	typ = r.svc.Type + ".local."
	return typ, escapeLabel(r.svc.Instance) + "." + typ, r.svc.Host + ".local."
}

// records are every record the Responder answers with for addrs, with
// TTLs ttl (-1 for each record's own; 0 says goodbye). The first is the
// service type's PTR, which other Instances on the host share.
func (r *Responder) records(addrs []net.IP, ttl int) []dns.RR {
	typ, inst, host := r.names()
	ttlOf := func(own uint32) uint32 {
		if ttl < 0 {
			return own
		}
		return uint32(ttl)
	}
	const flush = 1 << 15 // the cache-flush bit, on records only this Responder owns
	out := []dns.RR{
		&dns.PTR{Hdr: dns.RR_Header{Name: "_services._dns-sd._udp.local.", Rrtype: dns.TypePTR, Class: dns.ClassINET, Ttl: ttlOf(otherTTL)}, Ptr: typ},
		&dns.PTR{Hdr: dns.RR_Header{Name: typ, Rrtype: dns.TypePTR, Class: dns.ClassINET, Ttl: ttlOf(otherTTL)}, Ptr: inst},
		&dns.SRV{Hdr: dns.RR_Header{Name: inst, Rrtype: dns.TypeSRV, Class: dns.ClassINET | flush, Ttl: ttlOf(hostTTL)}, Port: uint16(r.svc.Port), Target: host},
		&dns.TXT{Hdr: dns.RR_Header{Name: inst, Rrtype: dns.TypeTXT, Class: dns.ClassINET | flush, Ttl: ttlOf(otherTTL)}, Txt: cmpTXT(r.svc.TXT)},
	}
	for _, ip := range addrs {
		if ip4 := ip.To4(); ip4 != nil {
			out = append(out, &dns.A{Hdr: dns.RR_Header{Name: host, Rrtype: dns.TypeA, Class: dns.ClassINET | flush, Ttl: ttlOf(hostTTL)}, A: ip4})
		} else {
			out = append(out, &dns.AAAA{Hdr: dns.RR_Header{Name: host, Rrtype: dns.TypeAAAA, Class: dns.ClassINET | flush, Ttl: ttlOf(hostTTL)}, AAAA: ip})
		}
	}
	return out
}

// cmpTXT is txt, or the empty string a TXT record must at least hold.
func cmpTXT(txt []string) []string {
	if len(txt) == 0 {
		return []string{""}
	}
	return txt
}

// unsolicited is a response announcing rrs, or saying goodbye to them.
func unsolicited(rrs []dns.RR) *dns.Msg {
	return &dns.Msg{MsgHdr: dns.MsgHdr{Response: true, Authoritative: true}, Answer: rrs}
}

// sendMulticast sends m to the group on ifi.
func (r *Responder) sendMulticast(ifi iface, m *dns.Msg) {
	b, err := m.Pack()
	if err != nil {
		r.log.Warn("mDNS: cannot pack a response", "err", err)
		return
	}
	r.wmu.Lock()
	defer r.wmu.Unlock()
	if err := r.pc.SetMulticastInterface(&ifi.Interface); err != nil {
		r.log.Debug("mDNS: cannot pick the interface", "interface", ifi.Name, "err", err)
		return
	}
	if _, err := r.pc.WriteTo(b, nil, &net.UDPAddr{IP: Group, Port: r.port}); err != nil {
		r.log.Debug("mDNS: cannot send", "interface", ifi.Name, "err", err)
	}
}

// send sends m by unicast to dst.
func (r *Responder) send(m *dns.Msg, dst *net.UDPAddr) {
	b, err := m.Pack()
	if err != nil {
		r.log.Warn("mDNS: cannot pack a response", "err", err)
		return
	}
	if _, err := r.pc.WriteTo(b, nil, dst); err != nil {
		r.log.Debug("mDNS: cannot answer", "to", dst, "err", err)
	}
}

// Browse queries for service instances of typ (e.g. "_gcpemu._tcp") on
// every multicast-capable interface, and returns those that answer within
// o.Wait, by instance label.
func Browse(ctx context.Context, typ string, o Options) ([]Entry, error) {
	ifs, err := interfacesFor([]net.IP{net.IPv4zero})
	if err != nil {
		return nil, err
	}
	conn, err := net.ListenPacket("udp4", "0.0.0.0:0")
	if err != nil {
		return nil, fmt.Errorf("mDNS: %w", err)
	}
	defer conn.Close()
	pc := ipv4.NewPacketConn(conn)
	_ = pc.SetMulticastLoopback(true)
	_ = pc.SetMulticastTTL(255)
	q := new(dns.Msg)
	q.SetQuestion(typ+".local.", dns.TypePTR)
	q.RecursionDesired = false
	b, err := q.Pack()
	if err != nil {
		return nil, err
	}
	sendAll := func() {
		for _, ifi := range ifs {
			if pc.SetMulticastInterface(&ifi.Interface) == nil {
				_, _ = pc.WriteTo(b, nil, &net.UDPAddr{IP: Group, Port: o.port()})
			}
		}
	}
	wait := o.Wait
	if wait == 0 {
		wait = time.Second
	}
	deadline := time.Now().Add(wait)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.SetReadDeadline(time.Now()) })
	defer stop()
	_ = conn.SetReadDeadline(deadline)

	// A query is sent again shortly after, in case the first was lost.
	sendAll()
	resend := time.AfterFunc(wait/4, sendAll)
	defer resend.Stop()

	c := collector{typ: strings.ToLower(typ + ".local."), srv: map[string]*dns.SRV{}, txt: map[string][]string{}, addrs: map[string][]net.IP{}, inst: map[string]bool{}}
	buf := make([]byte, 9000)
	for {
		n, _, err := conn.ReadFrom(buf)
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				break
			}
			return nil, err
		}
		var m dns.Msg
		if m.Unpack(buf[:n]) != nil || !m.Response {
			continue
		}
		c.add(slices.Concat(m.Answer, m.Ns, m.Extra))
	}
	if err := ctx.Err(); err != nil && time.Now().Before(deadline) {
		return nil, err
	}
	return c.entries(), nil
}

// collector gathers the records browsing receives.
type collector struct {
	typ   string
	inst  map[string]bool
	srv   map[string]*dns.SRV
	txt   map[string][]string
	addrs map[string][]net.IP
	// gone are instances whose PTR said goodbye.
	gone []string
}

func (c *collector) add(rrs []dns.RR) {
	for _, rr := range rrs {
		name := strings.ToLower(rr.Header().Name)
		switch rr := rr.(type) {
		case *dns.PTR:
			if name == c.typ {
				in := strings.ToLower(rr.Ptr)
				c.inst[in] = rr.Hdr.Ttl > 0
			}
		case *dns.SRV:
			c.srv[name] = rr
		case *dns.TXT:
			c.txt[name] = rr.Txt
		case *dns.A:
			if !slices.ContainsFunc(c.addrs[name], rr.A.Equal) {
				c.addrs[name] = append(c.addrs[name], rr.A)
			}
		case *dns.AAAA:
			if !slices.ContainsFunc(c.addrs[name], rr.AAAA.Equal) {
				c.addrs[name] = append(c.addrs[name], rr.AAAA)
			}
		}
	}
}

func (c *collector) entries() []Entry {
	var out []Entry
	for in, live := range c.inst {
		srv := c.srv[in]
		if !live || srv == nil {
			continue
		}
		label, _ := strings.CutSuffix(in, "."+c.typ)
		e := Entry{Instance: unescapeLabel(label), Host: strings.TrimSuffix(srv.Target, "."), Port: int(srv.Port),
			Addrs: c.addrs[strings.ToLower(srv.Target)], TXT: map[string]string{}}
		for _, s := range c.txt[in] {
			k, v, _ := strings.Cut(s, "=")
			e.TXT[strings.ToLower(k)] = v
		}
		out = append(out, e)
	}
	slices.SortFunc(out, func(a, b Entry) int { return strings.Compare(a.Instance, b.Instance) })
	return out
}

// interfacesFor returns the up, multicast-capable interfaces that hold
// ips, each with the addresses of ips it holds. An unspecified address
// stands for every such interface but loopback, with all its addresses.
func interfacesFor(ips []net.IP) ([]iface, error) {
	all, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	var out []iface
	for _, ifi := range all {
		if ifi.Flags&net.FlagUp == 0 || ifi.Flags&net.FlagMulticast == 0 {
			continue
		}
		addrs, _ := ifi.Addrs()
		var held []net.IP
		var hasV4 bool
		for _, a := range addrs {
			n, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			hasV4 = hasV4 || n.IP.To4() != nil
			for _, ip := range ips {
				switch {
				case ip.IsUnspecified() && ifi.Flags&net.FlagLoopback == 0 && !n.IP.IsLinkLocalUnicast():
					held = append(held, n.IP)
				case ip.Equal(n.IP):
					held = append(held, n.IP)
				}
			}
		}
		// IPv4 multicast carries the service, so the interface needs an
		// IPv4 address.
		if hasV4 && len(held) > 0 {
			out = append(out, iface{ifi, slices.CompactFunc(held, net.IP.Equal)})
		}
	}
	return out, nil
}

// escapeLabel escapes a DNS-SD instance label for miekg/dns, which reads
// names in presentation format.
func escapeLabel(s string) string {
	var b strings.Builder
	for _, r := range s {
		if strings.ContainsRune(`.\ ()";@$`, r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// unescapeLabel undoes escapeLabel, and miekg/dns's \DDD escapes.
func unescapeLabel(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			if i+3 < len(s) && isDigit(s[i+1]) && isDigit(s[i+2]) && isDigit(s[i+3]) {
				b.WriteByte(byte((s[i+1]-'0')*100 + (s[i+2]-'0')*10 + s[i+3] - '0'))
				i += 3
				continue
			}
			i++
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func isDigit(c byte) bool { return '0' <= c && c <= '9' }

// cmpLog is l, or a logger that discards.
func cmpLog(l *slog.Logger) *slog.Logger {
	if l == nil {
		return slog.New(slog.DiscardHandler)
	}
	return l
}

// listenGroup binds the mDNS port with the reuse options mDNS daemons use,
// on the group address where the OS allows it.
func listenGroup(port int) (net.PacketConn, error) {
	lc := net.ListenConfig{Control: func(_, _ string, c syscall.RawConn) error {
		var serr error
		if err := c.Control(func(fd uintptr) { serr = setReuse(fd) }); err != nil {
			return err
		}
		return serr
	}}
	addr := &net.UDPAddr{IP: bindIP, Port: port}
	pc, err := lc.ListenPacket(context.Background(), "udp4", addr.String())
	if err != nil {
		return nil, errors.Join(fmt.Errorf("cannot share %s", addr), err)
	}
	return pc, nil
}
