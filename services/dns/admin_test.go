package dns_test

import (
	"encoding/json"
	"net/http"
	"net/url"
	"testing"

	dnsv1 "google.golang.org/api/dns/v1"

	"github.com/linuxuser586/gcpemu/emutest"
	"github.com/linuxuser586/gcpemu/internal/config"
)

// TestConsoleQuery checks the Web console's query box: GET
// /_emu/v1/dns/query asks the DNS server, from the host or from a VPC
// network, and reports the response's sections and the answering zone.
func TestConsoleQuery(t *testing.T) {
	inst := emutest.Start(t, []string{"dns"}, emutest.WithConfig(func(c *config.Config) { c.DNSNoForward = true }))
	c := newClient(t, inst.GatewayURL()+"/")
	dataPlaneFixture(t, c)
	vpc := "projects/" + proj + "/global/networks/vpc-a"
	if _, err := c.ManagedZones.Create(proj, &dnsv1.ManagedZone{Name: "priv", DnsName: "svc.internal.", Description: "p",
		Visibility: "private", PrivateVisibilityConfig: &dnsv1.ManagedZonePrivateVisibilityConfig{
			Networks: []*dnsv1.ManagedZonePrivateVisibilityConfigNetwork{{NetworkUrl: vpc}}}}).Do(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Changes.Create(proj, "priv", &dnsv1.Change{Additions: []*dnsv1.ResourceRecordSet{
		rrset("db.svc.internal.", "A", 60, "10.0.0.5")}}).Do(); err != nil {
		t.Fatal(err)
	}

	type rr struct {
		Name, Type, Data string
		TTL              uint32
	}
	type result struct {
		Server, Name, Type, Network, Rcode, Zone string
		Authoritative                            bool
		Answer, Authority, Additional            []rr
	}
	ask := func(params url.Values, code int) (result, string) {
		t.Helper()
		resp, err := http.Get(inst.GatewayURL() + "/_emu/v1/dns/query?" + params.Encode())
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var raw json.RawMessage
		_ = json.NewDecoder(resp.Body).Decode(&raw)
		if resp.StatusCode != code {
			t.Fatalf("query %v: %d %s", params, resp.StatusCode, raw)
		}
		var out result
		var e struct{ Error string }
		_ = json.Unmarshal(raw, &out)
		_ = json.Unmarshal(raw, &e)
		return out, e.Error
	}

	got, _ := ask(url.Values{"name": {"alias.app.test"}}, 200)
	if got.Name != "alias.app.test." || got.Type != "A" || got.Rcode != "NOERROR" || !got.Authoritative ||
		got.Zone != "projects/"+proj+"/managedZones/app" || got.Server == "" || len(got.Answer) != 2 ||
		got.Answer[0] != (rr{"alias.app.test.", "CNAME", "www.app.test.", 300}) ||
		got.Answer[1] != (rr{"www.app.test.", "A", "192.0.2.10", 120}) {
		t.Errorf("CNAME chase = %+v", got)
	}
	got, _ = ask(url.Values{"name": {"app.test."}, "type": {"mx"}}, 200)
	if len(got.Answer) != 1 || got.Answer[0].Data != "10 mail.app.test." ||
		len(got.Additional) != 1 || got.Additional[0].Data != "192.0.2.25" {
		t.Errorf("MX = %+v", got)
	}
	got, _ = ask(url.Values{"name": {"nope.app.test."}}, 200)
	if got.Rcode != "NXDOMAIN" || len(got.Answer) != 0 || len(got.Authority) != 1 || got.Authority[0].Type != "SOA" {
		t.Errorf("NXDOMAIN = %+v", got)
	}

	// A private zone answers only on its network.
	got, _ = ask(url.Values{"name": {"db.svc.internal."}}, 200)
	if got.Rcode != "REFUSED" || got.Zone != "" || got.Network != "" {
		t.Errorf("private zone from the host = %+v", got)
	}
	got, _ = ask(url.Values{"name": {"db.svc.internal."}, "network": {"https://www.googleapis.com/compute/v1/" + vpc}}, 200)
	if got.Rcode != "NOERROR" || got.Network != vpc || got.Zone != "projects/"+proj+"/managedZones/priv" ||
		len(got.Answer) != 1 || got.Answer[0].Data != "10.0.0.5" {
		t.Errorf("private zone on its network = %+v", got)
	}

	for _, tc := range []struct {
		params url.Values
		want   string
	}{
		{url.Values{}, "name is required"},
		{url.Values{"name": {"a..b"}}, "name a..b. is not a domain name"},
		{url.Values{"name": {"x.test"}, "type": {"BOGUS"}}, "type BOGUS is not a DNS record type"},
		{url.Values{"name": {"x.test"}, "network": {"vpc-a"}}, "network must be a VPC network, e.g. projects/P/global/networks/N"},
	} {
		if _, e := ask(tc.params, 400); e != tc.want {
			t.Errorf("query %v: error %q, want %q", tc.params, e, tc.want)
		}
	}
}
