package dns_test

import (
	"context"
	"net"
	"sync"
	"testing"

	mdns "github.com/miekg/dns"

	"github.com/linuxuser586/gcpemu/emutest"
	"github.com/linuxuser586/gcpemu/internal/config"
)

// Host names (ADR 0014): a host query for a name under a host suffix is
// answered by the installed function, which a CNAME target outside the
// suffixes leaves to the zones; a query from a VPC network, a name outside
// the suffixes and Resolve never reach it.
func TestHostNames(t *testing.T) {
	inst := emutest.Start(t, []string{"dns"}, emutest.WithConfig(func(c *config.Config) { c.DNSNoForward = true }))
	c := newClient(t, inst.GatewayURL()+"/")
	dataPlaneFixture(t, c)
	addr := inst.Endpoint("dns")
	svc, _ := inst.Internal().Env.Lookup("dns")
	hn := svc.(interface {
		SetHostNames([]string, func(context.Context, string, uint16) ([]mdns.RR, int))
	})

	var mu sync.Mutex
	var asked []string
	hn.SetHostNames([]string{"svc.cluster.local.", "gke.internal."}, func(_ context.Context, name string, qtype uint16) ([]mdns.RR, int) {
		mu.Lock()
		asked = append(asked, name)
		mu.Unlock()
		hdr := mdns.RR_Header{Name: name, Class: mdns.ClassINET, Ttl: 5}
		switch name {
		case "web.default.svc.cluster.local.":
			if qtype != mdns.TypeA {
				return nil, mdns.RcodeSuccess
			}
			hdr.Rrtype = mdns.TypeA
			return []mdns.RR{&mdns.A{Hdr: hdr, A: net.ParseIP("127.0.0.7")}}, mdns.RcodeSuccess
		case "ext.default.svc.cluster.local.":
			hdr.Rrtype = mdns.TypeCNAME
			return []mdns.RR{&mdns.CNAME{Hdr: hdr, Target: "www.app.test."}}, mdns.RcodeSuccess
		case "chain.default.svc.cluster.local.":
			hdr.Rrtype = mdns.TypeCNAME
			return []mdns.RR{&mdns.CNAME{Hdr: hdr, Target: "web.default.svc.cluster.local."}}, mdns.RcodeSuccess
		case "full.default.svc.c.l.p.gke.internal.":
			return nil, mdns.RcodeServerFailure
		}
		return nil, mdns.RcodeNameError
	})
	t.Cleanup(func() { hn.SetHostNames(nil, nil) })

	for _, proto := range []string{"udp", "tcp"} {
		r := query(t, addr, proto, "Web.Default.svc.cluster.local.", mdns.TypeA)
		if r.Rcode != mdns.RcodeSuccess || !r.Authoritative || len(r.Answer) != 1 || r.Answer[0].(*mdns.A).A.String() != "127.0.0.7" ||
			r.Answer[0].Header().Name != "Web.Default.svc.cluster.local." {
			t.Errorf("%s A: %v", proto, r)
		}
	}
	if r := query(t, addr, "udp", "web.default.svc.cluster.local.", mdns.TypeAAAA); r.Rcode != mdns.RcodeSuccess || len(r.Answer) != 0 {
		t.Errorf("AAAA: %v", r)
	}
	r := query(t, addr, "udp", "ext.default.svc.cluster.local.", mdns.TypeA)
	if r.Rcode != mdns.RcodeSuccess || len(r.Answer) != 2 || r.Answer[1].(*mdns.A).A.String() != "192.0.2.10" {
		t.Errorf("ExternalName chased into a zone: %v", answers(r))
	}
	r = query(t, addr, "udp", "chain.default.svc.cluster.local.", mdns.TypeA)
	if len(r.Answer) != 2 || r.Answer[1].(*mdns.A).A.String() != "127.0.0.7" {
		t.Errorf("CNAME to a host name: %v", answers(r))
	}
	if r := query(t, addr, "udp", "ext.default.svc.cluster.local.", mdns.TypeCNAME); len(r.Answer) != 1 {
		t.Errorf("CNAME query is not chased: %v", answers(r))
	}
	if r := query(t, addr, "udp", "full.default.svc.c.l.p.gke.internal.", mdns.TypeA); r.Rcode != mdns.RcodeServerFailure {
		t.Errorf("SERVFAIL: %v", r)
	}
	// A name under a suffix is never forwarded (a host resolver would
	// send it straight back).
	if r := query(t, addr, "udp", "x.y.z.svc.cluster.local.", mdns.TypeA); r.Rcode != mdns.RcodeNameError || !r.Authoritative {
		t.Errorf("other name under a suffix: %v", r)
	}

	mu.Lock()
	asked = nil
	mu.Unlock()
	if r := queryNet(t, addr, "udp", "projects/"+proj+"/global/networks/default", "web.default.svc.cluster.local.", mdns.TypeA); len(r.Answer) != 0 {
		t.Errorf("VPC query answered: %v", r)
	}
	if r := query(t, addr, "udp", "www.app.test.", mdns.TypeA); len(r.Answer) != 1 {
		t.Errorf("zone name: %v", r)
	}
	if _, err := svc.(interface {
		Resolve(context.Context, string, uint16) ([]mdns.RR, error)
	}).Resolve(context.Background(), "web.default.svc.cluster.local.", mdns.TypeA); err == nil {
		t.Errorf("Resolve answered a host name")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(asked) != 0 {
		t.Errorf("asked for %v; want no VPC, zone or Resolve name", asked)
	}
}
