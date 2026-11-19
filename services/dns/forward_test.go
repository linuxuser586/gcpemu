package dns

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	mdns "github.com/miekg/dns"
	dnsv1 "google.golang.org/api/dns/v1"

	"github.com/linuxuser586/gcpemu/internal/clock"
	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// fakeResolver serves A ip for every name.
func fakeResolver(t *testing.T, ip string) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	srv := &mdns.Server{PacketConn: pc, NotifyStartedFunc: func() { close(started) }, Handler: mdns.HandlerFunc(func(w mdns.ResponseWriter, r *mdns.Msg) {
		m := new(mdns.Msg)
		m.SetReply(r)
		rr, _ := mdns.NewRR(r.Question[0].Name + " 30 IN A " + ip)
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
	p := startForwarding(t, s, deadResolver(t))
	writeResolvConf(t, p, time.Now().Add(-time.Hour), fakeResolver(t, "198.51.100.1"))
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
	// Before Start, nothing is forwarded → SERVFAIL.
	s.fwd.Store(nil)
	if _, err := s.Resolve(ctx, "example.org", mdns.TypeA); err != ErrServFail {
		t.Fatalf("no upstream: %v", err)
	}
}

func TestHostNameservers(t *testing.T) {
	s := newTestService(t, false)
	p := startForwarding(t, s, "192.0.2.8:53")
	_ = os.WriteFile(p, []byte("nameserver 192.0.2.53\n"), 0o600)
	if got := s.fwd.Load().upstreams(); !slices.Equal(got, []string{"192.0.2.53:53", "192.0.2.8:53"}) {
		t.Fatalf("got %v", got)
	}
	_ = os.Remove(p)
	if got := s.fwd.Load().upstreams(); len(got) != 1 || got[0] != "192.0.2.8:53" {
		t.Fatalf("fallback %v", got)
	}
	s.env.Config.Offline = true
	s.startForwarding()
	if got := s.fwd.Load().upstreams(); len(got) != 0 {
		t.Fatalf("offline, missing %v", got)
	}
	_ = os.WriteFile(p, []byte("nameserver 192.0.2.53\n"), 0o600)
	if got := s.fwd.Load().upstreams(); !slices.Equal(got, []string{"192.0.2.53:53"}) {
		t.Fatalf("offline %v", got)
	}
}

// deadResolver is a UDP address that reads queries and never answers, as
// an unreachable resolver times out.
func deadResolver(t *testing.T) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	go func() {
		buf := make([]byte, 4096)
		for {
			if _, _, err := pc.ReadFrom(buf); err != nil {
				return
			}
		}
	}()
	return pc.LocalAddr().String()
}

// writeResolvConf writes a resolv.conf listing addrs (IP:port) at p, with
// a modification time of mtime.
func writeResolvConf(t *testing.T, p string, mtime time.Time, addrs ...string) {
	t.Helper()
	var b strings.Builder
	for _, a := range addrs {
		b.WriteString("nameserver " + a + "\n")
	}
	if err := os.WriteFile(p, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

// startForwarding points s at a resolv.conf in a temporary directory and
// a public fallback of public, as Start would with the host's.
func startForwarding(t *testing.T, s *Service, public string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "resolv.conf")
	oldConf, oldPublic := resolvConf, publicNameserver
	resolvConf, publicNameserver = p, public
	t.Cleanup(func() { resolvConf, publicNameserver = oldConf, oldPublic })
	s.startForwarding()
	return p
}

// resolveA resolves example.org's single A record through s.
func resolveA(t *testing.T, s *Service) (string, error) {
	t.Helper()
	rrs, err := s.Resolve(context.Background(), "example.org", mdns.TypeA)
	if err != nil {
		return "", err
	}
	if len(rrs) != 1 {
		t.Fatalf("answer %v", rrs)
	}
	return rrs[0].(*mdns.A).A.String(), nil
}

func TestForwardingFollowsResolvConf(t *testing.T) {
	s := newTestService(t, false)
	p := startForwarding(t, s, deadResolver(t))
	t0 := time.Now().Add(-time.Hour)
	writeResolvConf(t, p, t0, fakeResolver(t, "198.51.100.1"))
	if got, err := resolveA(t, s); err != nil || got != "198.51.100.1" {
		t.Fatalf("first: %v %v", got, err)
	}
	// The host moves networks: resolv.conf now lists another resolver.
	writeResolvConf(t, p, t0.Add(time.Minute), fakeResolver(t, "198.51.100.2"))
	if got, err := resolveA(t, s); err != nil || got != "198.51.100.2" {
		t.Fatalf("after change: %v %v", got, err)
	}
}

func TestForwardingSkipsDeadResolver(t *testing.T) {
	s := newTestService(t, false)
	p := startForwarding(t, s, deadResolver(t))
	writeResolvConf(t, p, time.Now().Add(-time.Hour), deadResolver(t), fakeResolver(t, "198.51.100.3"))
	if got, err := resolveA(t, s); err != nil || got != "198.51.100.3" {
		t.Fatalf("first: %v %v", got, err)
	}
	// The dead resolver is skipped now: the next lookups don't wait on it.
	for i := range 3 {
		start := time.Now()
		if got, err := resolveA(t, s); err != nil || got != "198.51.100.3" {
			t.Fatalf("lookup %d: %v %v", i, got, err)
		}
		if d := time.Since(start); d > time.Second {
			t.Fatalf("lookup %d waited %v on the dead resolver", i, d)
		}
	}
}

func TestForwardingPublicFallback(t *testing.T) {
	t.Run("online", func(t *testing.T) {
		s := newTestService(t, false)
		p := startForwarding(t, s, fakeResolver(t, "198.51.100.9"))
		writeResolvConf(t, p, time.Now().Add(-time.Hour), deadResolver(t))
		if got, err := resolveA(t, s); err != nil || got != "198.51.100.9" {
			t.Fatalf("got %v %v", got, err)
		}
	})
	t.Run("offline", func(t *testing.T) {
		s := newTestService(t, false)
		s.env.Config.Offline = true
		p := startForwarding(t, s, fakeResolver(t, "198.51.100.9"))
		writeResolvConf(t, p, time.Now().Add(-time.Hour), deadResolver(t))
		if got, err := resolveA(t, s); err != ErrServFail {
			t.Fatalf("got %v %v, want SERVFAIL", got, err)
		}
	})
}

// GET /_emu/v1/dns/forwarding reports, for doctor, whether names outside
// every zone are forwarded and to which of the host's resolvers.
func TestServeForwarding(t *testing.T) {
	get := func(s *Service) forwardingStatus {
		t.Helper()
		rec := httptest.NewRecorder()
		s.serveForwarding(rec, httptest.NewRequest(http.MethodGet, "/_emu/v1/dns/forwarding", nil))
		var v forwardingStatus
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &v) != nil {
			t.Fatalf("%d %s", rec.Code, rec.Body)
		}
		return v
	}
	s := newTestService(t, false)
	p := startForwarding(t, s, "192.0.2.8:53")
	writeResolvConf(t, p, time.Now().Add(-time.Hour), "192.0.2.53:53", "[2001:db8::53]:53")
	want := forwardingStatus{Nameservers: []string{"192.0.2.53:53", "[2001:db8::53]:53"}, Public: "192.0.2.8:53"}
	if got := get(s); !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
	s.env.Config.Offline = true
	s.startForwarding()
	if got := get(s); !got.Offline || got.Public != "" {
		t.Errorf("offline: %+v", got)
	}
	s.env.Config.DNSNoForward = true
	if got := get(s); !got.NoForward {
		t.Errorf("no-forward: %+v", got)
	}
}

// Several dead host resolvers share the lookup's 5 s, as the data plane
// gives it, so the public resolver still answers the first lookup.
func TestForwardingBudgetReachesPublic(t *testing.T) {
	s := newTestService(t, false)
	p := startForwarding(t, s, fakeResolver(t, "198.51.100.9"))
	writeResolvConf(t, p, time.Now().Add(-time.Hour), deadResolver(t), deadResolver(t), deadResolver(t))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rrs, err := s.Resolve(ctx, "example.org", mdns.TypeA)
	if err != nil || len(rrs) != 1 || rrs[0].(*mdns.A).A.String() != "198.51.100.9" {
		t.Fatalf("got %v %v", rrs, err)
	}
}

// A failed resolver is skipped for 30 s, then tried again; when every
// host resolver is in its window they are all tried rather than none.
func TestForwardingSkipWindow(t *testing.T) {
	s := newTestService(t, false)
	p := startForwarding(t, s, "192.0.2.8:53")
	writeResolvConf(t, p, time.Now().Add(-time.Hour), "192.0.2.53:53", "192.0.2.54:53")
	f := s.fwd.Load()
	now := time.Now()
	f.now = func() time.Time { return now }
	f.upstreams() // reads resolv.conf, as a lookup does before any failure
	f.failed("192.0.2.53:53")
	if got := f.upstreams(); !slices.Equal(got, []string{"192.0.2.54:53", "192.0.2.8:53"}) {
		t.Fatalf("one skipped: %v", got)
	}
	f.failed("192.0.2.54:53")
	if got := f.upstreams(); !slices.Equal(got, []string{"192.0.2.53:53", "192.0.2.54:53", "192.0.2.8:53"}) {
		t.Fatalf("all skipped: %v", got)
	}
	now = now.Add(29 * time.Second)
	f.failed("192.0.2.54:53") // its window starts again; .53's ends first
	now = now.Add(time.Second)
	if got := f.upstreams(); !slices.Equal(got, []string{"192.0.2.53:53", "192.0.2.8:53"}) {
		t.Fatalf("after 30 s: %v", got)
	}
}
