package dns

import (
	"context"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"testing"

	mdns "github.com/miekg/dns"
	dnsv1 "google.golang.org/api/dns/v1"

	"github.com/linuxuser586/gcpemu/internal/clock"
	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// fakeUpstream serves A 198.51.100.1 for every name.
func fakeUpstream(t *testing.T) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	srv := &mdns.Server{PacketConn: pc, NotifyStartedFunc: func() { close(started) }, Handler: mdns.HandlerFunc(func(w mdns.ResponseWriter, r *mdns.Msg) {
		m := new(mdns.Msg)
		m.SetReply(r)
		rr, _ := mdns.NewRR(r.Question[0].Name + " 30 IN A 198.51.100.1")
		m.Answer = []mdns.RR{rr}
		_ = w.WriteMsg(m)
	})}
	go func() { _ = srv.ActivateAndServe() }()
	<-started
	t.Cleanup(func() { _ = srv.Shutdown() })
	return pc.LocalAddr().String()
}

func newTestService(t *testing.T, noForward bool) *Service {
	cfg := config.Defaults()
	cfg.DNSNoForward = noForward
	env := &emu.Env{Config: &cfg, Store: store.NewMemory(), Clock: clock.NewFake(), IDs: emu.NewIDs(true),
		Log: slog.New(slog.DiscardHandler), Endpoints: emu.NewEndpoints()}
	return New(env).(*Service)
}

func TestForwarding(t *testing.T) {
	s := newTestService(t, false)
	s.upstreams = []string{fakeUpstream(t)}
	if _, err := s.CreateZone("p", &dnsv1.ManagedZone{Name: "z", DnsName: "z.test."}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertRRSet("p", "z", &dnsv1.ResourceRecordSet{Name: "ext.z.test.", Type: "CNAME", Ttl: 60, Rrdatas: []string{"lb.example.net."}}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	rrs, err := s.Resolve(ctx, "example.org", mdns.TypeA)
	if err != nil || len(rrs) != 1 || rrs[0].(*mdns.A).A.String() != "198.51.100.1" {
		t.Fatalf("forward: %v %v", err, rrs)
	}
	// A CNAME leaving every zone is completed via the host resolver.
	rrs, err = s.Resolve(ctx, "ext.z.test", mdns.TypeA)
	if err != nil || len(rrs) != 2 {
		t.Fatalf("out-of-zone chase: %v %v", err, rrs)
	}
	// No upstream answers → SERVFAIL.
	s.upstreams = nil
	if _, err := s.Resolve(ctx, "example.org", mdns.TypeA); err != ErrServFail {
		t.Fatalf("no upstream: %v", err)
	}
}

func TestHostResolvers(t *testing.T) {
	old := resolvConf
	t.Cleanup(func() { resolvConf = old })
	s := newTestService(t, false)
	p := filepath.Join(t.TempDir(), "resolv.conf")
	_ = os.WriteFile(p, []byte("nameserver 192.0.2.53\n"), 0o600)
	resolvConf = p
	if got := s.hostResolvers(); len(got) != 1 || got[0] != "192.0.2.53:53" {
		t.Fatalf("got %v", got)
	}
	resolvConf = filepath.Join(t.TempDir(), "missing")
	if got := s.hostResolvers(); len(got) != 1 || got[0] != "8.8.8.8:53" {
		t.Fatalf("fallback %v", got)
	}
	s.env.Config.Offline = true
	if got := s.hostResolvers(); len(got) != 0 {
		t.Fatalf("offline %v", got)
	}
}
