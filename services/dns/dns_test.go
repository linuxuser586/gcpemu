package dns_test

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	mdns "github.com/miekg/dns"
	dnsv1 "google.golang.org/api/dns/v1"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
	"gopkg.in/yaml.v3"

	"github.com/linuxuser586/gcpemu/emutest"
	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/services/dns"
)

const proj = "test-proj"

func newClient(t *testing.T, endpoint string) *dnsv1.Service {
	t.Helper()
	svc, err := dnsv1.NewService(context.Background(), option.WithEndpoint(endpoint), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

func apiCode(err error) (int, string) {
	var ge *googleapi.Error
	if errors.As(err, &ge) {
		r := ""
		if len(ge.Errors) > 0 {
			r = ge.Errors[0].Reason
		}
		return ge.Code, r
	}
	return 0, ""
}

func wantErr(t *testing.T, err error, code int, reason string) {
	t.Helper()
	c, r := apiCode(err)
	if c != code || r != reason {
		t.Fatalf("got error %v (code %d reason %q), want %d %q", err, c, r, code, reason)
	}
}

func rrset(name, typ string, ttl int64, data ...string) *dnsv1.ResourceRecordSet {
	return &dnsv1.ResourceRecordSet{Name: name, Type: typ, Ttl: ttl, Rrdatas: data}
}

func TestControlPlane(t *testing.T) {
	inst := emutest.Start(t, []string{"dns"})
	ctx := context.Background()
	c := newClient(t, inst.GatewayURL()+"/")

	// Validation errors.
	_, err := c.ManagedZones.Create(proj, &dnsv1.ManagedZone{Name: "bad", DnsName: "example.com", Description: "x"}).Do()
	wantErr(t, err, 400, "invalid")
	_, err = c.ManagedZones.Create(proj, &dnsv1.ManagedZone{Name: "Bad_Name", DnsName: "example.com.", Description: "x"}).Do()
	wantErr(t, err, 400, "invalid")
	_, err = c.ManagedZones.Create(proj, &dnsv1.ManagedZone{Name: "nodns", Description: "x"}).Do()
	wantErr(t, err, 400, "required")

	z, err := c.ManagedZones.Create(proj, &dnsv1.ManagedZone{Name: "example", DnsName: "example.com.", Description: "Example"}).Context(ctx).Do()
	if err != nil {
		t.Fatal(err)
	}
	if z.Kind != "dns#managedZone" || z.Visibility != "public" || len(z.NameServers) != 4 || z.Id == 0 || z.CreationTime == "" {
		t.Fatalf("zone = %+v", z)
	}
	if !strings.HasPrefix(z.NameServers[0], "ns-cloud-") || !strings.HasSuffix(z.NameServers[0], "1.googledomains.com.") {
		t.Fatalf("nameServers = %v", z.NameServers)
	}
	_, err = c.ManagedZones.Create(proj, &dnsv1.ManagedZone{Name: "example", DnsName: "example.com.", Description: "x"}).Do()
	wantErr(t, err, 409, "alreadyExists")

	// Get via the gateway prefix mount (/dns/dns/v1/...) and by ID.
	c2 := newClient(t, inst.GatewayURL()+"/dns/")
	g, err := c2.ManagedZones.Get(proj, "example").Do()
	if err != nil || g.Id != z.Id {
		t.Fatalf("get via /dns/: %v %+v", err, g)
	}
	if g, err = c.ManagedZones.Get(proj, "1"+strings.Repeat("0", 18)).Do(); err == nil {
		t.Fatalf("unexpected zone %v", g.Name)
	}
	_, err = c.ManagedZones.Get(proj, "missing").Do()
	wantErr(t, err, 404, "notFound")

	// Private zone with a network binding.
	pz, err := c.ManagedZones.Create(proj, &dnsv1.ManagedZone{
		Name: "internal", DnsName: "internal.test.", Description: "private", Visibility: "private",
		PrivateVisibilityConfig: &dnsv1.ManagedZonePrivateVisibilityConfig{Networks: []*dnsv1.ManagedZonePrivateVisibilityConfigNetwork{
			{NetworkUrl: "projects/" + proj + "/global/networks/default"},
		}},
	}).Do()
	if err != nil {
		t.Fatal(err)
	}
	if pz.NameServers[0] != "ns-gcp-private.googledomains.com." ||
		pz.PrivateVisibilityConfig.Networks[0].NetworkUrl != "https://www.googleapis.com/compute/v1/projects/"+proj+"/global/networks/default" {
		t.Fatalf("private zone = %+v", pz)
	}

	l, err := c.ManagedZones.List(proj).Do()
	if err != nil || len(l.ManagedZones) != 2 {
		t.Fatalf("list: %v %d", err, len(l.ManagedZones))
	}
	l, err = c.ManagedZones.List(proj).DnsName("example.com.").Do()
	if err != nil || len(l.ManagedZones) != 1 {
		t.Fatalf("list by dnsName: %v", err)
	}
	// Pagination.
	l, err = c.ManagedZones.List(proj).MaxResults(1).Do()
	if err != nil || len(l.ManagedZones) != 1 || l.NextPageToken == "" {
		t.Fatalf("page 1: %v %+v", err, l)
	}
	l2, err := c.ManagedZones.List(proj).MaxResults(1).PageToken(l.NextPageToken).Do()
	if err != nil || len(l2.ManagedZones) != 1 || l2.ManagedZones[0].Name == l.ManagedZones[0].Name || l2.NextPageToken != "" {
		t.Fatalf("page 2: %v %+v", err, l2)
	}

	// Automatic SOA and NS.
	rl, err := c.ResourceRecordSets.List(proj, "example").Do()
	if err != nil || len(rl.Rrsets) != 2 {
		t.Fatalf("rrsets: %v %+v", err, rl)
	}
	ch0, err := c.Changes.Get(proj, "example", "0").Do()
	if err != nil || ch0.Status != "done" || len(ch0.Additions) != 2 {
		t.Fatalf("change 0: %v %+v", err, ch0)
	}

	// changes.create.
	ch, err := c.Changes.Create(proj, "example", &dnsv1.Change{Additions: []*dnsv1.ResourceRecordSet{
		rrset("www.example.com.", "A", 300, "192.0.2.1", "192.0.2.2"),
		rrset("example.com.", "MX", 300, "10 mail.example.com."),
		rrset("example.com.", "TXT", 300, `"v=spf1 -all"`),
		rrset("_sip._tcp.example.com.", "SRV", 300, "10 5 5060 sip.example.com."),
		rrset("example.com.", "CAA", 300, `0 issue "letsencrypt.org"`),
		rrset("v6.example.com.", "AAAA", 300, "2001:db8::1"),
	}}).Do()
	if err != nil {
		t.Fatal(err)
	}
	if ch.Id != "1" || ch.Status != "done" || ch.Kind != "dns#change" {
		t.Fatalf("change = %+v", ch)
	}
	// Conflicting addition.
	_, err = c.Changes.Create(proj, "example", &dnsv1.Change{Additions: []*dnsv1.ResourceRecordSet{rrset("www.example.com.", "A", 300, "192.0.2.9")}}).Do()
	wantErr(t, err, 409, "alreadyExists")
	// Deletion must match exactly.
	_, err = c.Changes.Create(proj, "example", &dnsv1.Change{Deletions: []*dnsv1.ResourceRecordSet{rrset("www.example.com.", "A", 300, "192.0.2.1")}}).Do()
	wantErr(t, err, 412, "conditionNotMet")
	_, err = c.Changes.Create(proj, "example", &dnsv1.Change{Deletions: []*dnsv1.ResourceRecordSet{rrset("nope.example.com.", "A", 300, "192.0.2.1")}}).Do()
	wantErr(t, err, 404, "notFound")
	// Bad rdata, out-of-zone name, CNAME conflict.
	_, err = c.Changes.Create(proj, "example", &dnsv1.Change{Additions: []*dnsv1.ResourceRecordSet{rrset("x.example.com.", "A", 300, "1.2.3")}}).Do()
	wantErr(t, err, 400, "invalid")
	_, err = c.Changes.Create(proj, "example", &dnsv1.Change{Additions: []*dnsv1.ResourceRecordSet{rrset("x.other.com.", "A", 300, "1.2.3.4")}}).Do()
	wantErr(t, err, 400, "invalid")
	_, err = c.Changes.Create(proj, "example", &dnsv1.Change{Additions: []*dnsv1.ResourceRecordSet{rrset("www.example.com.", "CNAME", 300, "other.example.com.")}}).Do()
	wantErr(t, err, 409, "cnameResourceRecordSetConflict")
	// Atomic replace (deletion + addition in one change); failure leaves state intact.
	_, err = c.Changes.Create(proj, "example", &dnsv1.Change{
		Deletions: []*dnsv1.ResourceRecordSet{rrset("www.example.com.", "A", 300, "192.0.2.2", "192.0.2.1")},
		Additions: []*dnsv1.ResourceRecordSet{rrset("www.example.com.", "A", 60, "192.0.2.3"), rrset("bad.example.com.", "A", 60, "nope")},
	}).Do()
	wantErr(t, err, 400, "invalid")
	ch, err = c.Changes.Create(proj, "example", &dnsv1.Change{
		Deletions: []*dnsv1.ResourceRecordSet{rrset("www.example.com.", "A", 300, "192.0.2.2", "192.0.2.1")},
		Additions: []*dnsv1.ResourceRecordSet{rrset("www.example.com.", "A", 60, "192.0.2.3")},
	}).Do()
	if err != nil || ch.Id != "2" {
		t.Fatalf("replace: %v %+v", err, ch)
	}
	cl, err := c.Changes.List(proj, "example").SortOrder("descending").Do()
	if err != nil || len(cl.Changes) != 3 || cl.Changes[0].Id != "2" {
		t.Fatalf("changes list: %v %+v", err, cl)
	}
	_, err = c.Changes.Get(proj, "example", "99").Do()
	wantErr(t, err, 404, "notFound")

	// SOA serial bumped by each change.
	soa, err := c.ResourceRecordSets.Get(proj, "example", "example.com.", "SOA").Do()
	if err != nil || !strings.Contains(soa.Rrdatas[0], " 3 21600 ") {
		t.Fatalf("soa: %v %+v", err, soa)
	}

	// rrsets CRUD.
	rs, err := c.ResourceRecordSets.Create(proj, "example", rrset("api.example.com.", "CNAME", 300, "www.example.com.")).Do()
	if err != nil || rs.Kind != "dns#resourceRecordSet" {
		t.Fatalf("rrset create: %v", err)
	}
	_, err = c.ResourceRecordSets.Create(proj, "example", rrset("api.example.com.", "CNAME", 300, "www.example.com.")).Do()
	wantErr(t, err, 409, "alreadyExists")
	rs, err = c.ResourceRecordSets.Patch(proj, "example", "api.example.com.", "CNAME", &dnsv1.ResourceRecordSet{Ttl: 120}).Do()
	if err != nil || rs.Ttl != 120 || rs.Rrdatas[0] != "www.example.com." {
		t.Fatalf("patch: %v %+v", err, rs)
	}
	rl, err = c.ResourceRecordSets.List(proj, "example").Name("api.example.com.").Type("CNAME").Do()
	if err != nil || len(rl.Rrsets) != 1 || rl.Rrsets[0].Ttl != 120 {
		t.Fatalf("filtered list: %v %+v", err, rl)
	}
	if _, err = c.ResourceRecordSets.Delete(proj, "example", "api.example.com.", "CNAME").Do(); err != nil {
		t.Fatal(err)
	}
	_, err = c.ResourceRecordSets.Get(proj, "example", "api.example.com.", "CNAME").Do()
	wantErr(t, err, 404, "notFound")

	// Patch zone → operation.
	op, err := c.ManagedZones.Patch(proj, "example", &dnsv1.ManagedZone{Description: "changed", Labels: map[string]string{"a": "b"}}).Do()
	if err != nil || op.Status != "done" || op.Type != "UPDATE" || op.ZoneContext.NewValue.Description != "changed" {
		t.Fatalf("patch zone: %v %+v", err, op)
	}
	ops, err := c.ManagedZoneOperations.List(proj, "example").Do()
	if err != nil || len(ops.Operations) != 1 {
		t.Fatalf("ops: %v", err)
	}
	if _, err := c.ManagedZoneOperations.Get(proj, "example", ops.Operations[0].Id).Do(); err != nil {
		t.Fatal(err)
	}
	g, _ = c.ManagedZones.Get(proj, "example").Do()
	if g.Description != "changed" || g.Labels["a"] != "b" {
		t.Fatalf("after patch: %+v", g)
	}

	// Delete of a non-empty zone fails; empty zone succeeds.
	err = c.ManagedZones.Delete(proj, "example").Do()
	wantErr(t, err, 400, "containerNotEmpty")
	if err := c.ManagedZones.Delete(proj, "internal").Do(); err != nil {
		t.Fatal(err)
	}

	pr, err := c.Projects.Get(proj).Do()
	if err != nil || pr.Id != proj || pr.Quota == nil {
		t.Fatalf("project: %v %+v", err, pr)
	}

	// Env vars.
	ev := inst.EnvVars()
	if ev["CLOUDSDK_API_ENDPOINT_OVERRIDES_DNS"] != inst.GatewayURL()+"/dns/v1/" || ev["GCPEMU_DNS"] != inst.Endpoint("dns") {
		t.Fatalf("env = %v", ev)
	}
}

func TestHostMode(t *testing.T) {
	inst := emutest.Start(t, []string{"dns"})
	req, _ := http.NewRequest("GET", inst.GatewayURL()+"/dns/v1/projects/"+proj+"/managedZones", nil)
	req.Host = "dns.googleapis.com"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestChangeLatency(t *testing.T) {
	inst := emutest.Start(t, []string{"dns"}, func(c *config.Config) { c.LROLatency["dns"] = "300ms" })
	c := newClient(t, inst.GatewayURL()+"/")
	if _, err := c.ManagedZones.Create(proj, &dnsv1.ManagedZone{Name: "z", DnsName: "z.test.", Description: "d"}).Do(); err != nil {
		t.Fatal(err)
	}
	ch, err := c.Changes.Create(proj, "z", &dnsv1.Change{Additions: []*dnsv1.ResourceRecordSet{rrset("a.z.test.", "A", 60, "192.0.2.1")}}).Do()
	if err != nil || ch.Status != "pending" {
		t.Fatalf("change: %v %+v", err, ch)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		ch, err = c.Changes.Get(proj, "z", ch.Id).Do()
		if err != nil {
			t.Fatal(err)
		}
		if ch.Status == "done" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("change never done")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// dataPlaneFixture creates zones used by the query tests.
func dataPlaneFixture(t *testing.T, c *dnsv1.Service) {
	t.Helper()
	zones := []struct{ name, dns string }{{"app", "app.test."}, {"sub", "sub.app.test."}, {"other", "other.test."}}
	for _, z := range zones {
		if _, err := c.ManagedZones.Create(proj, &dnsv1.ManagedZone{Name: z.name, DnsName: z.dns, Description: "d"}).Do(); err != nil {
			t.Fatal(err)
		}
	}
	add := func(zone string, rs ...*dnsv1.ResourceRecordSet) {
		if _, err := c.Changes.Create(proj, zone, &dnsv1.Change{Additions: rs}).Do(); err != nil {
			t.Fatal(err)
		}
	}
	add("app",
		rrset("www.app.test.", "A", 120, "192.0.2.10"),
		rrset("alias.app.test.", "CNAME", 300, "www.app.test."),
		rrset("cross.app.test.", "CNAME", 300, "target.other.test."),
		rrset("dangling.app.test.", "CNAME", 300, "missing.app.test."),
		rrset("*.wild.app.test.", "A", 60, "192.0.2.99"),
		rrset("txt.app.test.", "TXT", 300, `"hello world" "second string"`),
		rrset("app.test.", "MX", 300, "10 mail.app.test."),
		rrset("mail.app.test.", "A", 300, "192.0.2.25"),
		rrset("_http._tcp.app.test.", "SRV", 300, "0 5 80 www.app.test."),
		rrset("deep.ent.app.test.", "A", 300, "192.0.2.30"),
		&dnsv1.ResourceRecordSet{Name: "wrr.app.test.", Type: "A", Ttl: 30, RoutingPolicy: &dnsv1.RRSetRoutingPolicy{
			Wrr: &dnsv1.RRSetRoutingPolicyWrrPolicy{Items: []*dnsv1.RRSetRoutingPolicyWrrPolicyWrrPolicyItem{
				{Weight: 1, Rrdatas: []string{"192.0.2.41"}}, {Weight: 0, Rrdatas: []string{"192.0.2.42"}},
			}}}},
		&dnsv1.ResourceRecordSet{Name: "geo.app.test.", Type: "A", Ttl: 30, RoutingPolicy: &dnsv1.RRSetRoutingPolicy{
			Geo: &dnsv1.RRSetRoutingPolicyGeoPolicy{Items: []*dnsv1.RRSetRoutingPolicyGeoPolicyGeoPolicyItem{
				{Location: "us-east1", Rrdatas: []string{"192.0.2.51"}}, {Location: "europe-west1", Rrdatas: []string{"192.0.2.52"}},
			}}}},
	)
	add("sub", rrset("x.sub.app.test.", "A", 300, "192.0.2.60"))
	add("other", rrset("target.other.test.", "A", 300, "192.0.2.70"))
}

func query(t *testing.T, addr, network, name string, qtype uint16) *mdns.Msg {
	t.Helper()
	m := new(mdns.Msg)
	m.SetQuestion(name, qtype)
	cl := &mdns.Client{Net: network, Timeout: 3 * time.Second}
	r, _, err := cl.Exchange(m, addr)
	if err != nil {
		t.Fatalf("%s %s %s: %v", network, name, mdns.TypeToString[qtype], err)
	}
	return r
}

func answers(r *mdns.Msg) []string {
	var out []string
	for _, rr := range r.Answer {
		out = append(out, rr.String())
	}
	return out
}

func TestDataPlane(t *testing.T) {
	inst := emutest.Start(t, []string{"dns"}, func(c *config.Config) { c.DNSNoForward = true })
	c := newClient(t, inst.GatewayURL()+"/")
	dataPlaneFixture(t, c)
	addr := inst.Endpoint("dns")

	for _, network := range []string{"udp", "tcp"} {
		t.Run(network, func(t *testing.T) {
			q := func(name string, qt uint16) *mdns.Msg { return query(t, addr, network, name, qt) }

			r := q("www.app.test.", mdns.TypeA)
			if r.Rcode != mdns.RcodeSuccess || !r.Authoritative || len(r.Answer) != 1 ||
				r.Answer[0].(*mdns.A).A.String() != "192.0.2.10" || r.Answer[0].Header().Ttl != 120 {
				t.Fatalf("A: %v", r)
			}
			// CNAME chase within the zone.
			r = q("alias.app.test.", mdns.TypeA)
			if len(r.Answer) != 2 || r.Answer[0].Header().Rrtype != mdns.TypeCNAME || r.Answer[1].(*mdns.A).A.String() != "192.0.2.10" {
				t.Fatalf("CNAME chase: %v", answers(r))
			}
			// CNAME chase across zones.
			r = q("cross.app.test.", mdns.TypeA)
			if len(r.Answer) != 2 || r.Answer[1].(*mdns.A).A.String() != "192.0.2.70" {
				t.Fatalf("cross-zone chase: %v", answers(r))
			}
			// Dangling CNAME → NXDOMAIN with the CNAME in the answer.
			r = q("dangling.app.test.", mdns.TypeA)
			if r.Rcode != mdns.RcodeNameError || len(r.Answer) != 1 {
				t.Fatalf("dangling: %v", r)
			}
			// NXDOMAIN with SOA in authority.
			r = q("nope.app.test.", mdns.TypeA)
			if r.Rcode != mdns.RcodeNameError || !r.Authoritative || len(r.Ns) != 1 || r.Ns[0].Header().Rrtype != mdns.TypeSOA {
				t.Fatalf("NXDOMAIN: %v", r)
			}
			if r.Ns[0].Header().Ttl != 300 {
				t.Fatalf("negative TTL = %d", r.Ns[0].Header().Ttl)
			}
			// NODATA.
			r = q("www.app.test.", mdns.TypeAAAA)
			if r.Rcode != mdns.RcodeSuccess || len(r.Answer) != 0 || len(r.Ns) != 1 || r.Ns[0].Header().Rrtype != mdns.TypeSOA {
				t.Fatalf("NODATA: %v", r)
			}
			// Empty non-terminal is NODATA, not NXDOMAIN.
			r = q("ent.app.test.", mdns.TypeA)
			if r.Rcode != mdns.RcodeSuccess || len(r.Answer) != 0 {
				t.Fatalf("ENT: %v", r)
			}
			// Wildcard.
			r = q("anything.wild.app.test.", mdns.TypeA)
			if len(r.Answer) != 1 || r.Answer[0].Header().Name != "anything.wild.app.test." || r.Answer[0].(*mdns.A).A.String() != "192.0.2.99" {
				t.Fatalf("wildcard: %v", answers(r))
			}
			// TXT with multiple strings.
			r = q("txt.app.test.", mdns.TypeTXT)
			if len(r.Answer) != 1 || !slices.Equal(r.Answer[0].(*mdns.TXT).Txt, []string{"hello world", "second string"}) {
				t.Fatalf("TXT: %v", answers(r))
			}
			// MX and SRV with glue.
			r = q("app.test.", mdns.TypeMX)
			if len(r.Answer) != 1 || r.Answer[0].(*mdns.MX).Mx != "mail.app.test." || len(r.Extra) != 1 {
				t.Fatalf("MX: %v", r)
			}
			r = q("_http._tcp.app.test.", mdns.TypeSRV)
			if len(r.Answer) != 1 || r.Answer[0].(*mdns.SRV).Port != 80 {
				t.Fatalf("SRV: %v", r)
			}
			// Most specific zone wins.
			r = q("x.sub.app.test.", mdns.TypeA)
			if len(r.Answer) != 1 || r.Answer[0].(*mdns.A).A.String() != "192.0.2.60" {
				t.Fatalf("sub zone: %v", r)
			}
			// Apex NS/SOA.
			r = q("app.test.", mdns.TypeNS)
			if len(r.Answer) != 4 {
				t.Fatalf("NS: %v", r)
			}
			// Routing policies: WRR with weight 0 never picked; geo picks first.
			for range 5 {
				if r = q("wrr.app.test.", mdns.TypeA); len(r.Answer) != 1 || r.Answer[0].(*mdns.A).A.String() != "192.0.2.41" {
					t.Fatalf("wrr: %v", answers(r))
				}
			}
			if r = q("geo.app.test.", mdns.TypeA); len(r.Answer) != 1 || r.Answer[0].(*mdns.A).A.String() != "192.0.2.51" {
				t.Fatalf("geo: %v", answers(r))
			}
			// Outside all zones with forwarding disabled → REFUSED.
			r = q("example.org.", mdns.TypeA)
			if r.Rcode != mdns.RcodeRefused {
				t.Fatalf("forward disabled: %v", r)
			}
		})
	}

	// Changes are visible immediately (index rebuilt on commit).
	if _, err := c.Changes.Create(proj, "app", &dnsv1.Change{Additions: []*dnsv1.ResourceRecordSet{rrset("new.app.test.", "A", 5, "192.0.2.77")}}).Do(); err != nil {
		t.Fatal(err)
	}
	if r := query(t, addr, "udp", "new.app.test.", mdns.TypeA); len(r.Answer) != 1 {
		t.Fatalf("new record: %v", r)
	}

	// In-process Resolve.
	svc, ok := inst.Env.Lookup("dns")
	if !ok {
		t.Fatal("no dns service")
	}
	res := svc.(*dns.Service)
	rrs, err := res.Resolve(context.Background(), "alias.app.test", mdns.TypeA)
	if err != nil || len(rrs) != 2 {
		t.Fatalf("Resolve: %v %v", err, rrs)
	}
	if _, err := res.Resolve(context.Background(), "nope.app.test", mdns.TypeA); !errors.Is(err, dns.ErrNXDomain) {
		t.Fatalf("Resolve NX: %v", err)
	}
	if _, err := res.Resolve(context.Background(), "example.org", mdns.TypeA); !errors.Is(err, dns.ErrRefused) {
		t.Fatalf("Resolve refused: %v", err)
	}
}

func TestSeedAndReset(t *testing.T) {
	inst := emutest.Start(t, []string{"dns"}, func(c *config.Config) { c.DNSNoForward = true })
	svc, _ := inst.Env.Lookup("dns")
	seed := `
project: seed-proj
zones:
  - name: seeded
    dnsName: seeded.test
    description: seeded zone
    records:
      - name: www
        type: A
        rrdatas: [192.0.2.5]
      - name: "@"
        type: TXT
        ttl: 60
        rrdatas: ['"hi"']
  - name: priv
    dnsName: priv.test.
    networks: [default]
    records:
      - name: db.priv.test.
        type: A
        rrdatas: [10.0.0.5]
`
	var node yaml.Node
	if err := yaml.Unmarshal([]byte(seed), &node); err != nil {
		t.Fatal(err)
	}
	seeder := svc.(interface {
		ApplySeed(context.Context, *yaml.Node, string) error
	})
	for range 2 { // idempotent
		if err := seeder.ApplySeed(context.Background(), node.Content[0], "."); err != nil {
			t.Fatal(err)
		}
	}
	addr := inst.Endpoint("dns")
	if r := query(t, addr, "udp", "www.seeded.test.", mdns.TypeA); len(r.Answer) != 1 || r.Answer[0].Header().Ttl != 300 {
		t.Fatalf("seeded A: %v", r)
	}
	// Private zones are answered on the host for now.
	if r := query(t, addr, "tcp", "db.priv.test.", mdns.TypeA); len(r.Answer) != 1 {
		t.Fatalf("private A: %v", r)
	}
	c := newClient(t, inst.GatewayURL()+"/")
	cl, err := c.Changes.List("seed-proj", "seeded").Do()
	if err != nil || len(cl.Changes) != 3 { // zone creation + two upserts; second seed is a no-op
		t.Fatalf("changes: %v %d", err, len(cl.Changes))
	}
	if err := inst.Reset(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r := query(t, addr, "udp", "www.seeded.test.", mdns.TypeA); r.Rcode != mdns.RcodeRefused {
		t.Fatalf("after reset: %v", r)
	}
}
