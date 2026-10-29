package lb_test

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"net"
	"net/http"
	"slices"
	"testing"
	"time"

	computev1 "google.golang.org/api/compute/v1"
	dnsv1 "google.golang.org/api/dns/v1"
	"google.golang.org/api/option"

	"github.com/linuxuser586/gcpemu/emutest"
	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/services/lb"
)

// TestManagedCertFollowsCRLURL: a managed SslCertificate's leaf names the
// gateway's CRL URL at the port it actually listens on, and is issued
// again when a Persistent Instance starts on another port, and by a CA
// rotation before it returns (ADR 0046).
func TestManagedCertFollowsCRLURL(t *testing.T) {
	dir := t.TempDir()
	run := func(port int) *env {
		t.Helper()
		inst := emutest.Start(t, []string{"lb", "cdn", "certs"}, emutest.WithDataDir(dir),
			emutest.WithConfig(func(c *config.Config) { c.Ports["gateway"] = port }))
		svc, _ := inst.Internal().Env.Lookup("lb")
		svc.(*lb.Service).SetOptions(lb.Options{DirectDial: true})
		c, err := computev1.NewService(context.Background(), option.WithEndpoint(inst.GatewayURL()+"/compute/v1/"), option.WithoutAuthentication())
		if err != nil {
			t.Fatal(err)
		}
		return &env{t: t, inst: inst, c: c}
	}
	leafOf := func(e *env) *x509.Certificate {
		cert, err := e.c.SslCertificates.Get(proj, "managed").Do()
		if err != nil || cert.Managed.Status != "ACTIVE" {
			return nil
		}
		b, _ := pem.Decode([]byte(cert.Certificate))
		if b == nil {
			return nil
		}
		leaf, _ := x509.ParseCertificate(b.Bytes)
		return leaf
	}
	wantURL := func(e *env) []string { return []string{e.inst.GatewayURL() + "/ca.crl"} }

	e := run(0)
	c := e.c
	e.do(c.BackendServices.Insert(proj, &computev1.BackendService{Name: "bs", LoadBalancingScheme: "EXTERNAL_MANAGED"}).Do())
	e.do(c.UrlMaps.Insert(proj, &computev1.UrlMap{Name: "map", DefaultService: "global/backendServices/bs"}).Do())
	e.do(c.SslCertificates.Insert(proj, &computev1.SslCertificate{Name: "managed", Type: "MANAGED",
		Managed: &computev1.SslCertificateManagedSslCertificate{Domains: []string{"shop.example.test"}}}).Do())
	e.do(c.TargetHttpsProxies.Insert(proj, &computev1.TargetHttpsProxy{Name: "p", UrlMap: "global/urlMaps/map",
		SslCertificates: []string{"global/sslCertificates/managed"}}).Do())
	e.do(c.GlobalForwardingRules.Insert(proj, &computev1.ForwardingRule{Name: "fr", LoadBalancingScheme: "EXTERNAL_MANAGED",
		PortRange: "8080", Target: "global/targetHttpsProxies/p"}).Do())
	fr, _ := c.GlobalForwardingRules.Get(proj, "fr").Do()
	dc, _ := dnsv1.NewService(context.Background(), option.WithEndpoint(e.inst.GatewayURL()+"/dns/"), option.WithoutAuthentication())
	if _, err := dc.ManagedZones.Create(proj, &dnsv1.ManagedZone{Name: "shop", DnsName: "example.test.", Description: "x"}).Do(); err != nil {
		t.Fatal(err)
	}
	if _, err := dc.Changes.Create(proj, "shop", &dnsv1.Change{Additions: []*dnsv1.ResourceRecordSet{
		{Name: "shop.example.test.", Type: "A", Ttl: 60, Rrdatas: []string{fr.IPAddress}}}}).Do(); err != nil {
		t.Fatal(err)
	}
	eventually(t, 15*time.Second, "managed certificate active with the gateway's CRL URL", func() bool {
		leaf := leafOf(e)
		return leaf != nil && slices.Equal(leaf.CRLDistributionPoints, wantURL(e))
	})
	first := leafOf(e)
	if err := e.inst.Close(context.Background()); err != nil {
		t.Fatal(err)
	}

	e = run(freePort(t))
	eventually(t, 15*time.Second, "managed certificate issued again with the new port's CRL URL", func() bool {
		leaf := leafOf(e)
		return leaf != nil && slices.Equal(leaf.CRLDistributionPoints, wantURL(e))
	})
	if leafOf(e).Equal(first) {
		t.Fatal("the leaf was not issued again")
	}

	resp, err := http.Post(e.inst.GatewayURL()+"/_emu/v1/ca/rotate", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("rotate: %s", resp.Status)
	}
	if leaf := leafOf(e); leaf == nil || leaf.CheckSignatureFrom(e.inst.Internal().Env.CA.Certificate()) != nil {
		t.Error("managed certificate not issued again by the rotated CA before rotate returned")
	}
}

// freePort returns a loopback port that was free a moment ago.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}
