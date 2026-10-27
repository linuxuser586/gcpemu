//go:build conformance

package conformance

import (
	"context"
	"fmt"
	"testing"

	dns "google.golang.org/api/dns/v1"
)

func TestDNSManagedZones(t *testing.T)     { dnsManagedZones.test(t) }
func TestDNSRecordSetChanges(t *testing.T) { dnsRecordSetChanges.test(t) }

// dnsService is a Cloud DNS client for c.
func dnsService(t testing.TB, c *conn) *dns.Service {
	t.Helper()
	s, err := dns.NewService(c.ctx, c.HTTP("dns")...)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// createZone creates the public managed zone base for
// <name>.example.com., deleted when the test ends.
func createZone(t testing.TB, c *conn, s *dns.Service, base string) *dns.ManagedZone {
	t.Helper()
	z, err := s.ManagedZones.Create(c.Project(), &dns.ManagedZone{
		Name:        c.Name(base),
		DnsName:     c.Name(base) + ".example.com.",
		Description: "gcpemu Conformance suite",
		Visibility:  "public",
		Labels:      c.Labels(),
	}).Context(c.ctx).Do()
	if err != nil {
		t.Fatalf("create zone: %v", err)
	}
	c.Cleanup(func(ctx context.Context) { _ = s.ManagedZones.Delete(c.Project(), z.Name).Context(ctx).Do() })
	return z
}

// dnsZoneOptions: a zone's name servers are picked per zone.
var dnsZoneOptions = []Option{Generated("nameServers")}

var dnsManagedZones = scenario{
	product: "dns",
	opts:    append([]Option{RunOnly("managedZones")}, dnsZoneOptions...),
	body: func(t testing.TB, c *conn) {
		s := dnsService(t, c)
		z := createZone(t, c, s, "zone")
		if _, err := s.ManagedZones.Get(c.Project(), z.Name).Context(c.ctx).Do(); err != nil {
			t.Fatalf("get: %v", err)
		}
		if _, err := s.ManagedZones.List(c.Project()).DnsName(z.DnsName).Context(c.ctx).Do(); err != nil {
			t.Fatalf("list: %v", err)
		}
		if _, err := s.ManagedZones.Patch(c.Project(), z.Name, &dns.ManagedZone{Description: "patched"}).Context(c.ctx).Do(); err != nil {
			t.Fatalf("patch: %v", err)
		}
		if _, err := s.ManagedZones.Get(c.Project(), c.Name("missing")).Context(c.ctx).Do(); err == nil {
			t.Error("get of a missing zone succeeded")
		}
		if _, err := s.ManagedZones.Create(c.Project(), &dns.ManagedZone{Name: z.Name, DnsName: z.DnsName, Description: "taken"}).Context(c.ctx).Do(); err == nil {
			t.Error("creating a zone whose name is taken succeeded")
		}
		if err := s.ManagedZones.Delete(c.Project(), z.Name).Context(c.ctx).Do(); err != nil {
			t.Fatalf("delete: %v", err)
		}
	},
}

var dnsRecordSetChanges = scenario{
	product: "dns",
	opts:    append([]Option{Unordered("rrsets", "type")}, dnsZoneOptions...),
	body: func(t testing.TB, c *conn) {
		s := dnsService(t, c)
		z := createZone(t, c, s, "records")
		a := &dns.ResourceRecordSet{Name: "www." + z.DnsName, Type: "A", Ttl: 300, Rrdatas: []string{"192.0.2.1"}}
		apply := func(change *dns.Change) {
			t.Helper()
			ch, err := s.Changes.Create(c.Project(), z.Name, change).Context(c.ctx).Do()
			if err != nil {
				t.Fatalf("change: %v", err)
			}
			c.Retry(func() error {
				got, err := s.Changes.Get(c.Project(), z.Name, ch.Id).Context(c.ctx).Do()
				if err != nil {
					return err
				}
				if got.Status != "done" {
					return fmt.Errorf("change %s is %s", ch.Id, got.Status)
				}
				return nil
			})
		}
		apply(&dns.Change{Additions: []*dns.ResourceRecordSet{a}})
		if _, err := s.ResourceRecordSets.List(c.Project(), z.Name).Context(c.ctx).Do(); err != nil {
			t.Fatalf("list record sets: %v", err)
		}
		if _, err := s.ResourceRecordSets.Get(c.Project(), z.Name, a.Name, "A").Context(c.ctx).Do(); err != nil {
			t.Fatalf("get record set: %v", err)
		}
		changed := *a
		changed.Rrdatas = []string{"192.0.2.2"}
		apply(&dns.Change{Deletions: []*dns.ResourceRecordSet{a}, Additions: []*dns.ResourceRecordSet{&changed}})
		if _, err := s.Changes.Create(c.Project(), z.Name, &dns.Change{Additions: []*dns.ResourceRecordSet{&changed}}).Context(c.ctx).Do(); err == nil {
			t.Error("adding a record set that exists succeeded")
		}
		if _, err := s.ResourceRecordSets.Get(c.Project(), z.Name, "missing."+z.DnsName, "A").Context(c.ctx).Do(); err == nil {
			t.Error("get of a missing record set succeeded")
		}
		apply(&dns.Change{Deletions: []*dns.ResourceRecordSet{&changed}})
	},
}
