package dns_test

import (
	"testing"

	mdns "github.com/miekg/dns"
	dnsv1 "google.golang.org/api/dns/v1"

	"github.com/linuxuser586/gcpemu/emutest"
	"github.com/linuxuser586/gcpemu/internal/config"
)

func TestPolicies(t *testing.T) {
	inst := emutest.Start(t, []string{"dns"})
	c := newClient(t, inst.GatewayURL()+"/")
	net := "projects/" + proj + "/global/networks/vpc"

	_, err := c.Policies.Create(proj, &dnsv1.Policy{Name: "Bad_Name"}).Do()
	wantErr(t, err, 400, "invalid")
	_, err = c.Policies.Create(proj, &dnsv1.Policy{Name: "p", AlternativeNameServerConfig: &dnsv1.PolicyAlternativeNameServerConfig{
		TargetNameServers: []*dnsv1.PolicyAlternativeNameServerConfigTargetNameServer{{Ipv4Address: "::1"}},
	}}).Do()
	wantErr(t, err, 400, "invalid")

	p, err := c.Policies.Create(proj, &dnsv1.Policy{
		Name: "inbound", Description: "d", EnableInboundForwarding: true, EnableLogging: true,
		Networks: []*dnsv1.PolicyNetwork{{NetworkUrl: net}},
		AlternativeNameServerConfig: &dnsv1.PolicyAlternativeNameServerConfig{
			TargetNameServers: []*dnsv1.PolicyAlternativeNameServerConfigTargetNameServer{{Ipv4Address: "192.0.2.53", ForwardingPath: "private"}},
		},
	}).Do()
	if err != nil {
		t.Fatal(err)
	}
	if p.Kind != "dns#policy" || p.Id == 0 || !p.EnableInboundForwarding ||
		p.Networks[0].NetworkUrl != "https://www.googleapis.com/compute/v1/"+net || p.Networks[0].Kind != "dns#policyNetwork" {
		t.Fatalf("policy = %+v", p)
	}
	_, err = c.Policies.Create(proj, &dnsv1.Policy{Name: "inbound"}).Do()
	wantErr(t, err, 409, "alreadyExists")
	// A network takes one policy.
	_, err = c.Policies.Create(proj, &dnsv1.Policy{Name: "other", Networks: []*dnsv1.PolicyNetwork{{NetworkUrl: net}}}).Do()
	wantErr(t, err, 400, "invalid")

	g, err := c.Policies.Get(proj, "inbound").Do()
	if err != nil || g.Id != p.Id || g.AlternativeNameServerConfig.TargetNameServers[0].ForwardingPath != "private" {
		t.Fatalf("get: %v %+v", err, g)
	}
	_, err = c.Policies.Get(proj, "missing").Do()
	wantErr(t, err, 404, "notFound")
	if l, err := c.Policies.List(proj).Do(); err != nil || len(l.Policies) != 1 || l.Kind != "dns#policiesListResponse" {
		t.Fatalf("list: %v %+v", err, l)
	}

	// A bound policy cannot be deleted; patching the networks away frees it.
	wantErr(t, c.Policies.Delete(proj, "inbound").Do(), 400, "resourceInUse")
	pr, err := c.Policies.Patch(proj, "inbound", &dnsv1.Policy{Description: "patched", ForceSendFields: []string{"Networks"}, NullFields: []string{"Networks"}}).Do()
	if err != nil || pr.Policy.Description != "patched" || len(pr.Policy.Networks) != 0 || !pr.Policy.EnableLogging {
		t.Fatalf("patch: %v %+v", err, pr)
	}
	ur, err := c.Policies.Update(proj, "inbound", &dnsv1.Policy{Name: "inbound"}).Do()
	if err != nil || ur.Policy.Description != "" || ur.Policy.EnableLogging || ur.Policy.AlternativeNameServerConfig != nil {
		t.Fatalf("update: %v %+v", err, ur)
	}
	if err := c.Policies.Delete(proj, "inbound").Do(); err != nil {
		t.Fatal(err)
	}
	_, err = c.Policies.Get(proj, "inbound").Do()
	wantErr(t, err, 404, "notFound")
}

func TestResponsePolicies(t *testing.T) {
	inst := emutest.Start(t, []string{"dns"}, emutest.WithConfig(func(c *config.Config) { c.DNSNoForward = true }))
	c := newClient(t, inst.GatewayURL()+"/")
	vpc, other := "projects/"+proj+"/global/networks/vpc", "projects/"+proj+"/global/networks/other"

	if _, err := c.ManagedZones.Create(proj, &dnsv1.ManagedZone{Name: "pub", DnsName: "corp.test.", Description: "x"}).Do(); err != nil {
		t.Fatal(err)
	}
	for _, rs := range []*dnsv1.ResourceRecordSet{
		rrset("www.corp.test.", "A", 60, "192.0.2.1"),
		rrset("keep.ads.corp.test.", "A", 60, "192.0.2.2"),
	} {
		if _, err := c.ResourceRecordSets.Create(proj, "pub", rs).Do(); err != nil {
			t.Fatal(err)
		}
	}

	rp, err := c.ResponsePolicies.Create(proj, &dnsv1.ResponsePolicy{
		ResponsePolicyName: "rp", Description: "d",
		Networks: []*dnsv1.ResponsePolicyNetwork{{NetworkUrl: vpc}},
	}).Do()
	if err != nil {
		t.Fatal(err)
	}
	if rp.Kind != "dns#responsePolicy" || rp.Id == 0 || rp.Networks[0].NetworkUrl != "https://www.googleapis.com/compute/v1/"+vpc {
		t.Fatalf("response policy = %+v", rp)
	}
	_, err = c.ResponsePolicies.Create(proj, &dnsv1.ResponsePolicy{ResponsePolicyName: "rp"}).Do()
	wantErr(t, err, 409, "alreadyExists")

	local := func(sets ...*dnsv1.ResourceRecordSet) *dnsv1.ResponsePolicyRuleLocalData {
		return &dnsv1.ResponsePolicyRuleLocalData{LocalDatas: sets}
	}
	// Validation: local data or bypass, owned by the rule's name.
	for _, bad := range []*dnsv1.ResponsePolicyRule{
		{RuleName: "r", DnsName: "www.corp.test."},
		{RuleName: "r", DnsName: "www.corp.test.", LocalData: local(rrset("other.corp.test.", "A", 60, "10.0.0.1"))},
		{RuleName: "r", DnsName: "www.corp.test.", Behavior: "bypassResponsePolicy", LocalData: local(rrset("www.corp.test.", "A", 60, "10.0.0.1"))},
		{RuleName: "r", DnsName: "www.*.corp.test.", LocalData: local(rrset("www.*.corp.test.", "A", 60, "10.0.0.1"))},
	} {
		if _, err := c.ResponsePolicyRules.Create(proj, "rp", bad).Do(); err == nil {
			t.Errorf("rule %+v accepted", bad)
		}
	}
	for _, rl := range []*dnsv1.ResponsePolicyRule{
		{RuleName: "www", DnsName: "www.corp.test.", LocalData: local(
			rrset("www.corp.test.", "A", 30, "10.0.0.1"), rrset("www.corp.test.", "TXT", 30, `"local"`))},
		{RuleName: "ads", DnsName: "*.ads.corp.test.", LocalData: local(rrset("*.ads.corp.test.", "CNAME", 30, "www.corp.test."))},
		{RuleName: "keep", DnsName: "keep.ads.corp.test.", Behavior: "bypassResponsePolicy"},
	} {
		got, err := c.ResponsePolicyRules.Create(proj, "rp", rl).Do()
		if err != nil {
			t.Fatal(err)
		}
		if got.Kind != "dns#responsePolicyRule" {
			t.Fatalf("rule = %+v", got)
		}
	}
	_, err = c.ResponsePolicyRules.Create(proj, "rp", &dnsv1.ResponsePolicyRule{RuleName: "dup", DnsName: "WWW.corp.test.",
		LocalData: local(rrset("WWW.corp.test.", "A", 30, "10.0.0.1"))}).Do()
	wantErr(t, err, 409, "alreadyExists")

	addr := inst.Endpoint("dns")
	check := func(network, name string, qtype uint16, rcode int, want ...string) {
		t.Helper()
		var r *mdns.Msg
		if network == "" {
			r = query(t, addr, "udp", name, qtype)
		} else {
			r = queryNet(t, addr, "udp", network, name, qtype)
		}
		var got []string
		for _, rr := range r.Answer {
			got = append(got, rr.Header().Name+" "+rdata(rr))
		}
		if r.Rcode != rcode || len(got) != len(want) {
			t.Errorf("%s %s on %q: %s %v, want %s %v", mdns.TypeToString[qtype], name, network,
				mdns.RcodeToString[r.Rcode], got, mdns.RcodeToString[rcode], want)
			return
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("%s %s on %q: %v, want %v", mdns.TypeToString[qtype], name, network, got, want)
			}
		}
	}
	check(vpc, "www.corp.test.", mdns.TypeA, mdns.RcodeSuccess, "www.corp.test. 10.0.0.1")
	check(vpc, "www.corp.test.", mdns.TypeTXT, mdns.RcodeSuccess, `www.corp.test. "local"`)
	check(vpc, "www.corp.test.", mdns.TypeAAAA, mdns.RcodeSuccess) // NODATA from local data
	// The wildcard rule answers names the zone lacks and chases its CNAME
	// through the policy again; the bypass rule leaves the zone's answer.
	check(vpc, "x.ads.corp.test.", mdns.TypeA, mdns.RcodeSuccess, "x.ads.corp.test. www.corp.test.", "www.corp.test. 10.0.0.1")
	check(vpc, "keep.ads.corp.test.", mdns.TypeA, mdns.RcodeSuccess, "keep.ads.corp.test. 192.0.2.2")
	// A wildcard rule does not cover its own parent (an empty non-terminal).
	check(vpc, "ads.corp.test.", mdns.TypeA, mdns.RcodeSuccess)
	// Unbound networks and the host see the zone.
	check(other, "www.corp.test.", mdns.TypeA, mdns.RcodeSuccess, "www.corp.test. 192.0.2.1")
	check("", "www.corp.test.", mdns.TypeA, mdns.RcodeSuccess, "www.corp.test. 192.0.2.1")

	var logged bool
	for _, e := range inst.Internal().Env.RequestLog.Entries("dns") {
		logged = logged || (e.Method == "UDP A www.corp.test." && e.Principal == vpc && e.Resource == "projects/"+proj+"/responsePolicies/rp")
	}
	if !logged {
		t.Errorf("request log: %+v", inst.Internal().Env.RequestLog.Entries("dns"))
	}

	// Rule patch and update; then rebinding the policy moves the override.
	pr, err := c.ResponsePolicyRules.Patch(proj, "rp", "www", &dnsv1.ResponsePolicyRule{
		LocalData: local(rrset("www.corp.test.", "A", 30, "10.0.0.7")),
	}).Do()
	if err != nil || pr.ResponsePolicyRule.DnsName != "www.corp.test." || len(pr.ResponsePolicyRule.LocalData.LocalDatas) != 1 {
		t.Fatalf("rule patch: %v %+v", err, pr)
	}
	check(vpc, "www.corp.test.", mdns.TypeA, mdns.RcodeSuccess, "www.corp.test. 10.0.0.7")
	if _, err := c.ResponsePolicyRules.Patch(proj, "rp", "www", &dnsv1.ResponsePolicyRule{Behavior: "bypassResponsePolicy"}).Do(); err != nil {
		t.Fatal(err)
	}
	check(vpc, "www.corp.test.", mdns.TypeA, mdns.RcodeSuccess, "www.corp.test. 192.0.2.1")
	if l, err := c.ResponsePolicyRules.List(proj, "rp").Do(); err != nil || len(l.ResponsePolicyRules) != 3 {
		t.Fatalf("rules list: %v %+v", err, l)
	}
	rpp, err := c.ResponsePolicies.Patch(proj, "rp", &dnsv1.ResponsePolicy{
		Networks: []*dnsv1.ResponsePolicyNetwork{{NetworkUrl: other}},
	}).Do()
	if err != nil || rpp.ResponsePolicy.Description != "d" {
		t.Fatalf("policy patch: %v %+v", err, rpp)
	}
	check(vpc, "x.ads.corp.test.", mdns.TypeA, mdns.RcodeNameError)
	check(other, "x.ads.corp.test.", mdns.TypeA, mdns.RcodeSuccess, "x.ads.corp.test. www.corp.test.", "www.corp.test. 192.0.2.1")

	// Deletion needs the rules and bindings gone first.
	wantErr(t, c.ResponsePolicies.Delete(proj, "rp").Do(), 400, "containerNotEmpty")
	for _, r := range []string{"www", "ads", "keep"} {
		if err := c.ResponsePolicyRules.Delete(proj, "rp", r).Do(); err != nil {
			t.Fatal(err)
		}
	}
	_, err = c.ResponsePolicyRules.Get(proj, "rp", "www").Do()
	wantErr(t, err, 404, "notFound")
	wantErr(t, c.ResponsePolicies.Delete(proj, "rp").Do(), 400, "resourceInUse")
	if _, err := c.ResponsePolicies.Update(proj, "rp", &dnsv1.ResponsePolicy{ResponsePolicyName: "rp"}).Do(); err != nil {
		t.Fatal(err)
	}
	if err := c.ResponsePolicies.Delete(proj, "rp").Do(); err != nil {
		t.Fatal(err)
	}
	if l, err := c.ResponsePolicies.List(proj).Do(); err != nil || len(l.ResponsePolicies) != 0 {
		t.Fatalf("list after delete: %v %+v", err, l)
	}
}

func rdata(rr mdns.RR) string {
	switch v := rr.(type) {
	case *mdns.A:
		return v.A.String()
	case *mdns.CNAME:
		return v.Target
	case *mdns.TXT:
		return `"` + v.Txt[0] + `"`
	}
	return rr.String()
}

func TestZoneIAM(t *testing.T) {
	for _, services := range [][]string{{"dns"}, {"dns", "iam"}} {
		t.Run(services[len(services)-1], func(t *testing.T) {
			inst := emutest.Start(t, services)
			c := newClient(t, inst.GatewayURL()+"/")
			if _, err := c.ManagedZones.Create(proj, &dnsv1.ManagedZone{Name: "z", DnsName: "z.test.", Description: "x"}).Do(); err != nil {
				t.Fatal(err)
			}
			res := "projects/" + proj + "/managedZones/z"
			_, err := c.ManagedZones.GetIamPolicy("projects/"+proj+"/managedZones/missing", &dnsv1.GoogleIamV1GetIamPolicyRequest{}).Do()
			wantErr(t, err, 404, "notFound")
			p, err := c.ManagedZones.GetIamPolicy(res, &dnsv1.GoogleIamV1GetIamPolicyRequest{}).Do()
			if err != nil || len(p.Bindings) != 0 || p.Etag == "" {
				t.Fatalf("get: %v %+v", err, p)
			}
			member := "user:alice@example.com"
			set, err := c.ManagedZones.SetIamPolicy(res, &dnsv1.GoogleIamV1SetIamPolicyRequest{Policy: &dnsv1.GoogleIamV1Policy{
				Etag: p.Etag, Bindings: []*dnsv1.GoogleIamV1Binding{{Role: "roles/dns.reader", Members: []string{member}}},
			}}).Do()
			if err != nil || len(set.Bindings) != 1 || set.Etag == p.Etag {
				t.Fatalf("set: %v %+v", err, set)
			}
			// A stale etag is refused.
			_, err = c.ManagedZones.SetIamPolicy(res, &dnsv1.GoogleIamV1SetIamPolicyRequest{Policy: &dnsv1.GoogleIamV1Policy{Etag: p.Etag}}).Do()
			wantErr(t, err, 409, "aborted")
			_, err = c.ManagedZones.SetIamPolicy(res, &dnsv1.GoogleIamV1SetIamPolicyRequest{Policy: &dnsv1.GoogleIamV1Policy{
				Bindings: []*dnsv1.GoogleIamV1Binding{{Role: "dns.reader", Members: []string{member}}},
			}}).Do()
			wantErr(t, err, 400, "invalid")
			g, err := c.ManagedZones.GetIamPolicy(res, &dnsv1.GoogleIamV1GetIamPolicyRequest{}).Do()
			if err != nil || g.Etag != set.Etag || g.Bindings[0].Members[0] != member {
				t.Fatalf("get after set: %v %+v", err, g)
			}
			tp, err := c.ManagedZones.TestIamPermissions(res, &dnsv1.GoogleIamV1TestIamPermissionsRequest{
				Permissions: []string{"dns.managedZones.get", "dns.resourceRecordSets.list"},
			}).Do()
			if err != nil || len(tp.Permissions) != 2 {
				t.Fatalf("testIamPermissions: %v %+v", err, tp)
			}

			// A deleted and recreated zone starts without bindings.
			if err := c.ManagedZones.Delete(proj, "z").Do(); err != nil {
				t.Fatal(err)
			}
			if _, err := c.ManagedZones.Create(proj, &dnsv1.ManagedZone{Name: "z", DnsName: "z.test.", Description: "x"}).Do(); err != nil {
				t.Fatal(err)
			}
			if g, err := c.ManagedZones.GetIamPolicy(res, &dnsv1.GoogleIamV1GetIamPolicyRequest{}).Do(); err != nil || len(g.Bindings) != 0 {
				t.Fatalf("get after recreate: %v %+v", err, g)
			}
		})
	}
}
