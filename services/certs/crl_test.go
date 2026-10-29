package certs

import (
	"context"
	"io"
	"log/slog"
	"slices"
	"testing"

	"github.com/linuxuser586/gcpemu/internal/ca"
	"github.com/linuxuser586/gcpemu/internal/clock"
	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// TestManagedCertificateFollowsCRLURL: a stored managed certificate whose
// leaf doesn't name the Instance's current CRL URL, as one issued before
// ADR 0046 or under another gateway port, is issued again on the next
// start, and again after a CA rotation.
func TestManagedCertificateFollowsCRLURL(t *testing.T) {
	cfg := config.Defaults()
	dir := t.TempDir()
	st := store.NewMemory()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx := context.Background()
	n := resName{Project: "p", Location: "global", ID: "app"}
	boot := func(crlURL string) *Service {
		t.Helper()
		authority, err := ca.Load(dir, "test")
		if err != nil {
			t.Fatal(err)
		}
		if crlURL != "" {
			authority.SetCRLURL(crlURL)
		}
		env := &emu.Env{Config: &cfg, Store: st, Clock: clock.Real{}, IDs: emu.NewIDs(true), Log: log,
			Auth: emu.NewPolicyAuthorizer(config.IAMOff, log), Endpoints: emu.NewEndpoints(), CA: authority}
		s := New(env).(*Service)
		dns := &fakeDNS{fakePeer{"dns"}, map[string]string{"app.example.test.": "203.0.113.10"}}
		lb := &fakeLB{fakePeer{"lb"}, []string{"203.0.113.10"}}
		env.SetServices(map[string]emu.Service{"certs": s, "dns": dns, "lb": lb})
		s.reconcileAll(ctx)
		return s
	}
	crlURLs := func(s *Service) []string {
		t.Helper()
		c, err := s.Certificate(ctx, n.name(kCert))
		if err != nil {
			t.Fatal(err)
		}
		return c.Leaf.CRLDistributionPoints
	}

	s := boot("")
	if err := s.upsert(ctx, kCert, n, obj{"managed": map[string]any{"domains": []any{"app.example.test"}}}); err != nil {
		t.Fatal(err)
	}
	s.reconcileAll(ctx)
	if got := crlURLs(s); len(got) != 0 {
		t.Fatalf("leaf issued with no CRL URL set names %q", got)
	}

	for _, u := range []string{"http://127.0.0.1:4443/ca.crl", "http://127.0.0.1:5555/ca.crl"} {
		s = boot(u)
		if got := crlURLs(s); !slices.Equal(got, []string{u}) {
			t.Fatalf("after a start with CRL URL %s, the leaf names %q", u, got)
		}
	}

	old, _ := s.Certificate(ctx, n.name(kCert))
	if err := s.env.CA.Rotate("test"); err != nil {
		t.Fatal(err)
	}
	s.invalidate()
	s.reconcileAll(ctx)
	c, err := s.Certificate(ctx, n.name(kCert))
	if err != nil {
		t.Fatal(err)
	}
	if c.Leaf.Equal(old.Leaf) || c.Leaf.CheckSignatureFrom(s.env.CA.Certificate()) != nil {
		t.Error("managed certificate not issued again by the rotated CA")
	}
}
