package lb_test

import (
	"context"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	computev1 "google.golang.org/api/compute/v1"
	dnsv1 "google.golang.org/api/dns/v1"
	"google.golang.org/api/option"

	"github.com/linuxuser586/gcpemu/emutest"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/services/gke"
	"github.com/linuxuser586/gcpemu/services/lb"
)

// resetAcross runs an Instance Reset and then after, with work held in
// flight until release is closed. Reset either returns at once, leaving
// the work to finish after it, or waits for the work, which is released
// once Reset is seen waiting.
func resetAcross(t *testing.T, e *env, release chan struct{}, after func() error) {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		if err := e.inst.Reset(context.Background()); err != nil {
			done <- err
			return
		}
		done <- after()
	}()
	select {
	case err := <-done:
		close(release)
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(200 * time.Millisecond):
		close(release) // Reset is waiting for the work
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
}

// A managed-certificate pass in flight across an Instance Reset writes
// nothing afterwards: the certificate it read before the Reset does not
// replace one of the same name created after it (#307).
func TestResetWaitsForManagedCertPass(t *testing.T) {
	e := start(t)
	c := e.c
	svc, _ := e.inst.Env.Lookup("lb")
	var armed atomic.Bool
	armed.Store(true)
	entered, release := make(chan struct{}), make(chan struct{})
	lb.SetBeforeCertWrite(svc, func(ctx context.Context, path string) {
		if armed.CompareAndSwap(true, false) {
			close(entered)
			<-release
		}
	})
	e.do(c.BackendServices.Insert(proj, &computev1.BackendService{Name: "bs", LoadBalancingScheme: "EXTERNAL_MANAGED"}).Do())
	e.do(c.UrlMaps.Insert(proj, &computev1.UrlMap{Name: "map", DefaultService: "global/backendServices/bs"}).Do())
	cert := &computev1.SslCertificate{Name: "managed", Type: "MANAGED",
		Managed: &computev1.SslCertificateManagedSslCertificate{Domains: []string{"shop.example.test"}}}
	e.do(c.SslCertificates.Insert(proj, cert).Do())
	e.do(c.TargetHttpsProxies.Insert(proj, &computev1.TargetHttpsProxy{Name: "p", UrlMap: "global/urlMaps/map",
		SslCertificates: []string{"global/sslCertificates/managed"}}).Do())
	e.do(c.GlobalForwardingRules.Insert(proj, &computev1.ForwardingRule{Name: "fr", LoadBalancingScheme: "EXTERNAL_MANAGED",
		PortRange: "8080", Target: "global/targetHttpsProxies/p"}).Do())
	fr, err := c.GlobalForwardingRules.Get(proj, "fr").Do()
	if err != nil {
		t.Fatal(err)
	}
	// DNS points at the forwarding rule: the next pass activates the
	// certificate, and is held before writing it.
	dc, err := dnsv1.NewService(context.Background(), option.WithEndpoint(e.inst.GatewayURL()+"/dns/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dc.ManagedZones.Create(proj, &dnsv1.ManagedZone{Name: "shop", DnsName: "example.test.", Description: "x"}).Do(); err != nil {
		t.Fatal(err)
	}
	if _, err := dc.Changes.Create(proj, "shop", &dnsv1.Change{Additions: []*dnsv1.ResourceRecordSet{
		{Name: "shop.example.test.", Type: "A", Ttl: 60, Rrdatas: []string{fr.IPAddress}}}}).Do(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(15 * time.Second):
		t.Fatal("no managed-certificate pass activated the certificate")
	}

	resetAcross(t, e, release, func() error {
		after := *cert
		after.Description = "after"
		op, err := c.SslCertificates.Insert(proj, &after).Do()
		if err == nil && op.Status != "DONE" {
			_, err = c.GlobalOperations.Wait(proj, op.Name).Do()
		}
		return err
	})
	time.Sleep(300 * time.Millisecond) // a pass from before the Reset, if one survived, finishes
	got, err := c.SslCertificates.Get(proj, "managed").Do()
	if err != nil {
		t.Fatal(err)
	}
	if got.Description != "after" || got.Managed.Status != "PROVISIONING" {
		t.Fatalf("the certificate created after Reset was replaced by one read before it: %+v %+v", got, got.Managed)
	}
}

// An edge route pass in flight across an Instance Reset creates no runtime
// object afterwards: pod routes read before the Reset start no lb-edge
// container after it (#307).
func TestResetWaitsForEdgeRoutePass(t *testing.T) {
	emutest.RequireRuntime(t)
	e := start(t)
	c := e.c
	ctx := context.Background()
	svc, _ := e.inst.Env.Lookup("lb")
	vpcSvc, _ := e.inst.Env.Lookup("compute")
	subnetwork, err := vpcSvc.(emu.VPC).ResolveSubnetwork(ctx, proj, "us-central1", "", "")
	if err != nil {
		t.Fatal(err)
	}
	// An external forwarding rule the edge would route for pods.
	e.do(c.BackendServices.Insert(proj, &computev1.BackendService{Name: "be", LoadBalancingScheme: "EXTERNAL_MANAGED"}).Do())
	e.do(c.UrlMaps.Insert(proj, &computev1.UrlMap{Name: "map", DefaultService: "global/backendServices/be"}).Do())
	e.do(c.TargetHttpProxies.Insert(proj, &computev1.TargetHttpProxy{Name: "p", UrlMap: "global/urlMaps/map"}).Do())
	e.do(c.GlobalForwardingRules.Insert(proj, &computev1.ForwardingRule{Name: "fr", LoadBalancingScheme: "EXTERNAL_MANAGED",
		PortRange: "8080", Target: "global/targetHttpProxies/p"}).Do())

	// The first pass reads a pod route and is held; the next one, which
	// starts once that pass is over, signals second.
	var calls atomic.Int32
	entered, release, second := make(chan struct{}), make(chan struct{}), make(chan struct{})
	lb.SetPodRoutes(svc, func(ctx context.Context) []gke.PodRoute {
		switch calls.Add(1) {
		case 1:
			close(entered)
			<-release
			return []gke.PodRoute{{Cluster: "projects/" + proj + "/locations/us-central1/clusters/c", Node: "n",
				CIDR: "10.200.0.0/24", Via: "10.128.0.5", Subnetwork: subnetwork}}
		case 2:
			close(second)
		}
		return nil
	})
	svc.(*lb.Service).SetOptions(lb.Options{})
	select {
	case <-entered:
	case <-time.After(15 * time.Second):
		t.Fatal("no edge route pass ran")
	}

	resetAcross(t, e, release, func() error { return nil })
	select {
	case <-second:
	case <-time.After(60 * time.Second):
		t.Fatal("the edge route pass from before the Reset did not finish")
	}
	cts, err := e.inst.Containers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, ct := range cts {
		if ct.Service == "lb" {
			t.Fatalf("container %s started after Reset from pod routes read before it", ct.Name)
		}
	}
}

// After an Instance Reset managed certificates are provisioned again
// (#307).
func TestManagedCertPassRunsAfterReset(t *testing.T) {
	e := start(t)
	c := e.c
	if err := e.inst.Reset(context.Background()); err != nil {
		t.Fatal(err)
	}
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
	eventually(t, 15*time.Second, "managed certificate active after Reset", func() bool {
		cert, err := c.SslCertificates.Get(proj, "managed").Do()
		return err == nil && cert.Managed.Status == "ACTIVE"
	})
}

// After an Instance Reset the data plane matches the reset Store: a
// forwarding rule from before it keeps no listener, and one created
// afterwards on the same port serves (#450).
func TestResetClosesListeners(t *testing.T) {
	e := start(t)
	e.neg(named(t, "before", nil))
	before := e.httpLB(&computev1.BackendService{Name: "bs"})
	if before == "" {
		t.Fatal("no listener before Reset")
	}
	if err := e.inst.Reset(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := e.inst.Endpoint("lb:fr"); got != "" {
		t.Errorf("lb:fr = %q after Reset, want none", got)
	}
	if conn, err := net.DialTimeout("tcp", before, time.Second); err == nil {
		conn.Close()
		t.Errorf("%s still accepts connections after Reset", before)
	}

	e.neg(named(t, "after", nil))
	l := e.httpLB(&computev1.BackendService{Name: "bs"})
	if l == "" {
		t.Fatal("a forwarding rule created after Reset has no listener")
	}
	eventually(t, 10*time.Second, "forwarding rule created after Reset serves", func() bool {
		resp, err := e.client(l).Get("http://shop.example.test:8080/x")
		if err != nil {
			return false
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp.StatusCode == 200 && string(body) == "after"
	})
}

// Deleting a forwarding rule drops its lb: entry with its listener
// (#450).
func TestDeleteDropsListenerEntry(t *testing.T) {
	e := start(t)
	e.neg(named(t, "x", nil))
	if e.httpLB(&computev1.BackendService{Name: "bs"}) == "" {
		t.Fatal("no listener")
	}
	e.do(e.c.GlobalForwardingRules.Delete(proj, "fr").Do())
	if got := e.inst.Endpoint("lb:fr"); got != "" {
		t.Fatalf("lb:fr = %q after the forwarding rule was deleted, want none", got)
	}
}
