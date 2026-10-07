package dns

import (
	"net/http/httptest"
	"strings"
	"testing"

	mdns "github.com/miekg/dns"
	dnsv1 "google.golang.org/api/dns/v1"

	"github.com/linuxuser586/gcpemu/internal/apierr"
)

func TestValidateRRSet(t *testing.T) {
	rs := func(name, typ string, data ...string) *dnsv1.ResourceRecordSet {
		return &dnsv1.ResourceRecordSet{Name: name, Type: typ, Ttl: 300, Rrdatas: data}
	}
	tests := []struct {
		rs     *dnsv1.ResourceRecordSet
		reason string // "" = valid
	}{
		{rs("a.example.com.", "A", "192.0.2.1"), ""},
		{rs("a.example.com.", "A", "192.0.2.1", "192.0.2.1"), "duplicateResourceRecordData"},
		{rs("a.example.com.", "A", "::1"), "invalid"},
		{rs("a.example.com.", "AAAA", "2001:db8::1"), ""},
		{rs("a.example.com", "A", "192.0.2.1"), "invalid"},
		{rs("a.other.com.", "A", "192.0.2.1"), "invalid"},
		{rs("", "A", "192.0.2.1"), "required"},
		{rs("a.example.com.", "", "192.0.2.1"), "required"},
		{rs("a.example.com.", "BOGUS", "x"), "invalid"},
		{rs("a.example.com.", "A"), "required"},
		{rs("example.com.", "CNAME", "x.example.com."), "cnameAtApex"},
		{rs("c.example.com.", "CNAME", "x.example.com.", "y.example.com."), "invalid"},
		{rs("example.com.", "MX", "10 mail.example.com."), ""},
		{rs("example.com.", "MX", "mail.example.com."), "invalid"},
		{rs("example.com.", "TXT", `"a b" "c"`), ""},
		{rs("_s._tcp.example.com.", "SRV", "1 2 3 t.example.com."), ""},
		{rs("_s._tcp.example.com.", "SRV", "1 2 t.example.com."), "invalid"},
		{rs("example.com.", "CAA", `0 issue "ca.example"`), ""},
		{rs("1.2.0.192.in-addr.arpa.", "PTR", "h.example.com."), "invalid"}, // outside zone
		{rs("p.example.com.", "PTR", "h.example.com."), ""},
		{rs("sub.example.com.", "NS", "ns1.example.net."), ""},
		{rs("sub.example.com.", "SOA", "a. b. 1 2 3 4 5"), "invalid"},
		{rs("*.example.com.", "A", "192.0.2.1"), ""},
		{rs("a.example.com.", "A", "192.0.2.1\n1.2.3.4"), "invalid"},
	}
	for _, tc := range tests {
		err := validateRRSet(tc.rs, "f", "example.com.")
		got := ""
		if err != nil {
			got = apierr.From(err).LegacyReason
		}
		if got != tc.reason {
			t.Errorf("%s %s %v: got %q (%v), want %q", tc.rs.Name, tc.rs.Type, tc.rs.Rrdatas, got, err, tc.reason)
		}
	}
}

func TestZoneNames(t *testing.T) {
	for s, ok := range map[string]bool{"a": true, "my-zone-1": true, "1zone": false, "Zone": false, "zone-": false, strings.Repeat("a", 64): false} {
		if validZoneName(s) != ok {
			t.Errorf("validZoneName(%q) != %v", s, ok)
		}
	}
	for s, ok := range map[string]bool{"example.com.": true, "example.com": false, ".": false, "a b.": false, "x.y.z.": true} {
		if validDNSName(s) != ok {
			t.Errorf("validDNSName(%q) != %v", s, ok)
		}
	}
}

func TestSameRRSet(t *testing.T) {
	a := &dnsv1.ResourceRecordSet{Name: "a.x.", Type: "A", Ttl: 1, Rrdatas: []string{"192.0.2.1", "192.0.2.2"}}
	b := &dnsv1.ResourceRecordSet{Name: "A.X.", Type: "A", Ttl: 1, Rrdatas: []string{"192.0.2.2", "192.0.2.1"}}
	if !sameRRSet(a, b) {
		t.Error("order/case should not matter")
	}
	b.Ttl = 2
	if sameRRSet(a, b) {
		t.Error("ttl must match")
	}
}

func TestBumpSerial(t *testing.T) {
	rs := &dnsv1.ResourceRecordSet{Name: "x.", Type: "SOA", Ttl: 1, Rrdatas: []string{"ns. host. 7 21600 3600 259200 300"}}
	if got := bumpSerial(rs).Rrdatas[0]; got != "ns. host. 8 21600 3600 259200 300" {
		t.Fatal(got)
	}
}

func TestPaginate(t *testing.T) {
	items := []keyed[int]{{"c", 3}, {"a", 1}, {"b", 2}}
	r := httptest.NewRequest("GET", "/?maxResults=2", nil)
	p, next, err := paginate(r, items)
	if err != nil || len(p) != 2 || p[0] != 1 || next == "" {
		t.Fatalf("%v %v %q", err, p, next)
	}
	r = httptest.NewRequest("GET", "/?maxResults=2&pageToken="+next, nil)
	p, next, _ = paginate(r, items)
	if len(p) != 1 || p[0] != 3 || next != "" {
		t.Fatalf("%v %q", p, next)
	}
	if _, _, err := paginate(httptest.NewRequest("GET", "/?maxResults=x", nil), items); err == nil {
		t.Fatal("want error")
	}
}

func TestParent(t *testing.T) {
	for in, want := range map[string]string{"a.b.": "b.", "b.": ".", ".": ""} {
		if got := parent(in); got != want {
			t.Errorf("parent(%q) = %q", in, got)
		}
	}
}

func testZone() *zoneIdx {
	z := &zoneIdx{origin: "ex.test.", names: map[string]map[uint16]*rrsetIdx{}, ents: map[string]bool{}}
	for _, rs := range []*dnsv1.ResourceRecordSet{
		{Name: "ex.test.", Type: "SOA", Ttl: 3600, Rrdatas: []string{"ns. h. 1 2 3 4 60"}},
		{Name: "ex.test.", Type: "NS", Ttl: 3600, Rrdatas: []string{"ns1.ex.test."}},
		{Name: "ns1.ex.test.", Type: "A", Ttl: 300, Rrdatas: []string{"192.0.2.53"}},
		{Name: "deleg.ex.test.", Type: "NS", Ttl: 300, Rrdatas: []string{"ns.deleg.ex.test."}},
		{Name: "ns.deleg.ex.test.", Type: "A", Ttl: 300, Rrdatas: []string{"192.0.2.54"}},
		{Name: "*.ex.test.", Type: "TXT", Ttl: 300, Rrdatas: []string{`"wild"`}},
		{Name: "a.b.c.ex.test.", Type: "A", Ttl: 300, Rrdatas: []string{"192.0.2.1"}},
	} {
		z.add(rs)
	}
	z.computeENTs()
	return z
}

func TestLookup(t *testing.T) {
	z := testZone()
	r := z.lookup("host.deleg.ex.test.", mdns.TypeA)
	if !r.referral || len(r.ns) != 1 || len(r.extra) != 1 {
		t.Fatalf("referral: %+v", r)
	}
	r = z.lookup("c.ex.test.", mdns.TypeA) // ENT: NODATA, wildcard does not apply
	if r.rcode != mdns.RcodeSuccess || len(r.answer) != 0 || r.ns[0].Header().Ttl != 60 {
		t.Fatalf("ENT: %+v", r)
	}
	r = z.lookup("x.ex.test.", mdns.TypeTXT)
	if len(r.answer) != 1 || r.answer[0].Header().Name != "x.ex.test." {
		t.Fatalf("wildcard: %+v", r)
	}
	r = z.lookup("x.ex.test.", mdns.TypeA) // wildcard owner exists but no A → NODATA
	if r.rcode != mdns.RcodeSuccess || len(r.answer) != 0 {
		t.Fatalf("wildcard nodata: %+v", r)
	}
	r = z.lookup("zz.c.ex.test.", mdns.TypeA) // closest encloser c.ex.test., no *.c → NXDOMAIN
	if r.rcode != mdns.RcodeNameError {
		t.Fatalf("NXDOMAIN: %+v", r)
	}
	r = z.lookup("ex.test.", mdns.TypeANY)
	if len(r.answer) != 2 {
		t.Fatalf("ANY: %+v", r)
	}
}

func TestPolicyPick(t *testing.T) {
	rs := &dnsv1.ResourceRecordSet{Name: "w.x.", Type: "A", Ttl: 1, RoutingPolicy: &dnsv1.RRSetRoutingPolicy{
		Wrr: &dnsv1.RRSetRoutingPolicyWrrPolicy{Items: []*dnsv1.RRSetRoutingPolicyWrrPolicyWrrPolicyItem{
			{Weight: 1, Rrdatas: []string{"192.0.2.1"}}, {Weight: 1, Rrdatas: []string{"192.0.2.2"}},
		}}}}
	if err := validateRRSet(rs, "f", "x."); err != nil {
		t.Fatal(err)
	}
	pc := buildPolicy(rs)
	seen := map[string]bool{}
	for range 200 {
		seen[pc.pick()[0].(*mdns.A).A.String()] = true
	}
	if len(seen) != 2 {
		t.Fatalf("wrr picked %v", seen)
	}
	rs.RoutingPolicy.Geo = &dnsv1.RRSetRoutingPolicyGeoPolicy{}
	if err := validateRRSet(rs, "f", "x."); err == nil {
		t.Fatal("two policies must be rejected")
	}
}
