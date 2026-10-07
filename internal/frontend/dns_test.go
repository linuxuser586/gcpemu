package frontend

import (
	"net"
	"sync/atomic"
	"testing"

	mdns "github.com/miekg/dns"
)

func TestDNSRelay(t *testing.T) {
	// Upstream answers every A query with 192.0.2.7.
	l, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	upAddr := l.LocalAddr().String()
	up := &mdns.Server{PacketConn: l, Handler: mdns.HandlerFunc(func(w mdns.ResponseWriter, r *mdns.Msg) {
		m := new(mdns.Msg)
		m.SetReply(r)
		m.Answer = append(m.Answer, &mdns.A{Hdr: mdns.RR_Header{Name: r.Question[0].Name, Rrtype: mdns.TypeA, Class: mdns.ClassINET, Ttl: 5}, A: net.ParseIP("192.0.2.7")})
		_ = w.WriteMsg(m)
	})}
	go func() { _ = up.ActivateAndServe() }()
	defer up.Shutdown()

	self := net.ParseIP("10.9.8.7")
	var noUpstream atomic.Bool
	h := &DNS{
		Answer: func(name string) net.IP {
			if Serves([]string{"storage.googleapis.com"}, name) {
				return self
			}
			return nil
		},
		Upstreams: func(string) []string {
			if noUpstream.Load() {
				return nil
			}
			return []string{upAddr}
		},
	}
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &mdns.Server{PacketConn: pc, Handler: h}
	go func() { _ = srv.ActivateAndServe() }()
	defer srv.Shutdown()
	addr := pc.LocalAddr().String()

	ask := func(name string, qt uint16) *mdns.Msg {
		t.Helper()
		m := new(mdns.Msg)
		m.SetQuestion(name, qt)
		c := &mdns.Client{}
		var r *mdns.Msg
		for i := 0; i < 20; i++ { // server start race
			if r, _, err = c.Exchange(m, addr); err == nil {
				return r
			}
		}
		t.Fatalf("query %s: %v", name, err)
		return nil
	}
	if r := ask("storage.googleapis.com.", mdns.TypeA); len(r.Answer) != 1 || r.Answer[0].(*mdns.A).A.String() != "10.9.8.7" {
		t.Errorf("served A = %v", r.Answer)
	}
	if r := ask("us-docker.pkg.dev.", mdns.TypeA); len(r.Answer) != 1 || r.Answer[0].(*mdns.A).A.String() != "10.9.8.7" {
		t.Errorf("registry A = %v", r.Answer)
	}
	if r := ask("storage.googleapis.com.", mdns.TypeAAAA); r.Rcode != mdns.RcodeSuccess || len(r.Answer) != 0 {
		t.Errorf("served AAAA = %v %v", r.Rcode, r.Answer)
	}
	if r := ask("logging.googleapis.com.", mdns.TypeA); len(r.Answer) != 1 || r.Answer[0].(*mdns.A).A.String() != "192.0.2.7" {
		t.Errorf("relayed A = %v", r.Answer)
	}
	noUpstream.Store(true)
	if r := ask("logging.googleapis.com.", mdns.TypeA); r.Rcode != mdns.RcodeServerFailure {
		t.Errorf("no upstream rcode = %v", r.Rcode)
	}
}
