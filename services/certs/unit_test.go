package certs

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"

	mdns "github.com/miekg/dns"

	"github.com/linuxuser586/gcpemu/internal/ca"
	"github.com/linuxuser586/gcpemu/internal/clock"
	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/store"
)

func TestSelectEntry(t *testing.T) {
	entries := []obj{
		{"name": "exact", "hostname": "www.example.com"},
		{"name": "wild", "hostname": "*.example.com"},
		{"name": "primary", "matcher": "PRIMARY"},
	}
	for sni, want := range map[string]string{
		"www.example.com":  "exact",
		"www.example.com.": "exact",
		"api.example.com":  "wild",
		"example.com":      "primary",
		"a.b.example.com":  "primary",
		"":                 "primary",
	} {
		got := selectEntry(entries, sni)
		if len(got) == 0 || got[0]["name"] != want {
			t.Errorf("selectEntry(%q) = %v, want %s first", sni, got, want)
		}
	}
	if got := selectEntry(entries[:2], "other.test"); len(got) != 0 {
		t.Errorf("no match: %v", got)
	}
}

func TestNames(t *testing.T) {
	for in, want := range map[string]string{
		"projects/p/locations/global/certificateMaps/m":                                              "projects/p/locations/global/certificateMaps/m",
		"//certificatemanager.googleapis.com/projects/p/locations/global/certificateMaps/m":          "projects/p/locations/global/certificateMaps/m",
		"https://certificatemanager.googleapis.com/v1/projects/p/locations/global/certificateMaps/m": "projects/p/locations/global/certificateMaps/m",
	} {
		if got := canonical(in); got != want {
			t.Errorf("canonical(%q) = %q", in, got)
		}
	}
	n, err := parseName(kEntry, "projects/p/locations/global/certificateMaps/m/certificateMapEntries/e")
	if err != nil || n.Parent != "m" || n.ID != "e" || n.parent(kEntry) != "projects/p/locations/global/certificateMaps/m" {
		t.Fatalf("%+v %v", n, err)
	}
	if _, err := parseName(kCert, "projects/p/locations/global/certificateMaps/m"); err == nil {
		t.Fatal("wrong collection accepted")
	}
	for id, ok := range map[string]bool{"a": true, "a-1": true, "A": false, "1a": false, "a-": false, strings.Repeat("a", 64): false} {
		if (validID(kCert, id) == nil) != ok {
			t.Errorf("validID(%q)", id)
		}
	}
	if checkLocation(kMap, "us-central1", false) == nil || checkLocation(kCert, "us-central1", false) != nil || checkLocation(kCert, "mars", false) == nil {
		t.Fatal("checkLocation")
	}
}

func TestDNSRecord(t *testing.T) {
	a := dnsRecord("p", "example.com", "FIXED_RECORD")
	b := dnsRecord("p", "example.com", "FIXED_RECORD")
	if a.Name != "_acme-challenge.example.com." || a.Type != "CNAME" || a.Data != b.Data || !strings.HasSuffix(a.Data, ".authorize.certificatemanager.goog.") {
		t.Fatalf("%+v", a)
	}
	pp := dnsRecord("p", "example.com", "PER_PROJECT_RECORD")
	if !strings.HasPrefix(pp.Name, "_acme-challenge_") || !strings.HasSuffix(pp.Name, ".example.com.") || pp.Data == a.Data {
		t.Fatalf("%+v", pp)
	}
}

func TestMaskAndFilter(t *testing.T) {
	paths, err := maskPaths(kCert, obj{}, []string{"description", "labels", "self_managed"})
	if err != nil || strings.Join(paths, ",") != "description,labels,selfManaged" {
		t.Fatalf("%v %v", paths, err)
	}
	if _, err := maskPaths(kCert, obj{}, []string{"bogus"}); err == nil {
		t.Fatal("unknown path accepted")
	}
	if _, err := maskPaths(kCert, obj{}, nil); err == nil {
		t.Fatal("certificatemanager requires a mask")
	}
	if paths, _ := maskPaths(kBAC, obj{}, nil); len(paths) == 0 {
		t.Fatal("networksecurity without mask replaces all fields")
	}
	o := obj{"name": "x", "labels": map[string]any{"env": "prod"}, "scope": "CLIENT_AUTH"}
	for f, want := range map[string]bool{"": true, "labels.env=prod": true, "labels.env=dev": false, `scope="CLIENT_AUTH"`: true, "labels.env:*": true, "labels.x:*": false} {
		if matchFilter(o, f) != want {
			t.Errorf("filter %q", f)
		}
	}
	res := obj{"scope": "DEFAULT", "managed": map[string]any{"state": "STATE_UNSPECIFIED"}, "labels": map[string]any{}}
	normalize(kCert, res)
	if len(res) != 1 || len(res["managed"].(map[string]any)) != 0 {
		t.Fatalf("normalize: %v", res)
	}
}

// ---- load balancer authorization with fake dns and lb peers ----

type fakePeer struct{ name string }

func (f *fakePeer) Name() string                { return f.name }
func (f *fakePeer) Register(emu.Router) error   { return nil }
func (f *fakePeer) Start(context.Context) error { return nil }
func (f *fakePeer) Stop(context.Context) error  { return nil }
func (f *fakePeer) Ready() error                { return nil }

type fakeDNS struct {
	fakePeer
	a map[string]string
}

func (f *fakeDNS) Resolve(ctx context.Context, name string, qtype uint16) ([]mdns.RR, error) {
	if ip, ok := f.a[mdns.Fqdn(name)]; ok && qtype == mdns.TypeA {
		return []mdns.RR{&mdns.A{Hdr: mdns.RR_Header{Name: mdns.Fqdn(name), Rrtype: mdns.TypeA}, A: net.ParseIP(ip)}}, nil
	}
	return nil, nil
}

type fakeLB struct {
	fakePeer
	ips []string
}

func (f *fakeLB) ForwardingRuleIPs(ctx context.Context, project string) []string { return f.ips }

func TestLoadBalancerAuthorization(t *testing.T) {
	cfg := config.Defaults()
	authority, err := ca.Load(t.TempDir(), "test")
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	env := &emu.Env{Config: &cfg, Store: store.NewMemory(), Clock: clock.Real{}, IDs: emu.NewIDs(true), Log: log,
		Auth: emu.NewPolicyAuthorizer(config.IAMOff, log), Endpoints: emu.NewEndpoints(), CA: authority}
	s := New(env).(*Service)
	dns := &fakeDNS{fakePeer{"dns"}, map[string]string{"app.example.test.": "203.0.113.9"}}
	lb := &fakeLB{fakePeer{"lb"}, []string{"203.0.113.10"}}
	env.SetServices(map[string]emu.Service{"certs": s, "dns": dns, "lb": lb})
	ctx := context.Background()
	n := resName{Project: "p", Location: "global", ID: "lbauth"}
	if err := s.upsert(ctx, kCert, n, obj{"managed": map[string]any{"domains": []any{"app.example.test"}}}); err != nil {
		t.Fatal(err)
	}
	s.reconcileCert(ctx, n.name(kCert))
	o, _, _ := s.loadKind(kCert, n.name(kCert))
	if certActive(o) {
		t.Fatal("authorized before DNS points at the forwarding rule")
	}
	if v, _ := getPath(o, "managed.authorizationAttemptInfo"); !strings.Contains(mustJSON(v), "WRONG_IP_ADDRESS") {
		t.Fatalf("attempt info: %v", v)
	}
	dns.a["app.example.test."] = "203.0.113.10"
	if !s.reconcileCert(ctx, n.name(kCert)) {
		t.Fatal("no change after DNS update")
	}
	c, err := s.Certificate(ctx, n.name(kCert))
	if err != nil || c.Leaf.DNSNames[0] != "app.example.test" {
		t.Fatalf("certificate: %v %v", c, err)
	}
	if err := s.upsert(ctx, kCert, resName{Project: "p", Location: "global", ID: "wild"}, obj{"managed": map[string]any{"domains": []any{"*.example.test"}}}); err == nil {
		t.Fatal("wildcard without DNS authorization accepted")
	}
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
