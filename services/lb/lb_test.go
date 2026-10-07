package lb_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"cloud.google.com/go/storage"
	mdns "github.com/miekg/dns"
	computev1 "google.golang.org/api/compute/v1"
	dnsv1 "google.golang.org/api/dns/v1"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	"github.com/linuxuser586/gcpemu/emutest"
	"github.com/linuxuser586/gcpemu/internal/ca"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/services/compute"
	"github.com/linuxuser586/gcpemu/services/dns"
	"github.com/linuxuser586/gcpemu/services/lb"
)

const proj = "lb-test"

type env struct {
	t    *testing.T
	inst *emutest.Instance
	c    *computev1.Service
}

func start(t *testing.T) *env {
	t.Helper()
	inst := emutest.Start(t, []string{"lb", "cdn"})
	svc, _ := inst.Env.Lookup("lb")
	svc.(*lb.Service).SetOptions(lb.Options{DirectDial: true})
	c, err := computev1.NewService(context.Background(), option.WithEndpoint(inst.GatewayURL()+"/compute/v1/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	return &env{t: t, inst: inst, c: c}
}

// do fails the test unless the call and its operation succeed.
func (e *env) do(op *computev1.Operation, err error) *computev1.Operation {
	e.t.Helper()
	if err != nil {
		e.t.Fatalf("call: %v", err)
	}
	if op.Status != "DONE" {
		op, err = e.c.GlobalOperations.Wait(proj, op.Name).Do()
		if err != nil {
			e.t.Fatal(err)
		}
	}
	if op.Error != nil {
		e.t.Fatalf("operation %s failed: %+v", op.OperationType, op.Error.Errors[0])
	}
	return op
}

func apiErr(err error) (int, string) {
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

// backend is a local origin server recording the last request.
type backend struct {
	*httptest.Server
	last chan *http.Request
}

func newBackend(t *testing.T) *backend {
	b := &backend{last: make(chan *http.Request, 100)}
	b.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.WriteHeader(http.StatusOK)
			return
		}
		select {
		case b.last <- r:
		default:
		}
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Cache-Control", "public, max-age=60")
		_, _ = io.WriteString(w, "backend:"+r.Host+r.URL.Path)
	}))
	t.Cleanup(b.Close)
	return b
}

func (b *backend) port() int64 {
	_, p, _ := net.SplitHostPort(b.Listener.Addr().String())
	n, _ := strconv.Atoi(p)
	return int64(n)
}

// stack creates a full HTTPS load balancer: NEG backend service plus a
// CDN-enabled GCS backend bucket.
func (e *env) stack(origin *backend) (fr *computev1.ForwardingRule) {
	t, c := e.t, e.c
	ctx := context.Background()
	e.do(c.Networks.Insert(proj, &computev1.Network{Name: "vpc", AutoCreateSubnetworks: false, ForceSendFields: []string{"AutoCreateSubnetworks"}}).Do())
	e.do(c.NetworkEndpointGroups.Insert(proj, "us-central1-a", &computev1.NetworkEndpointGroup{
		Name: "neg", NetworkEndpointType: "GCE_VM_IP_PORT", Network: "global/networks/vpc", DefaultPort: origin.port()}).Do())
	e.do(c.NetworkEndpointGroups.AttachNetworkEndpoints(proj, "us-central1-a", "neg", &computev1.NetworkEndpointGroupsAttachEndpointsRequest{
		NetworkEndpoints: []*computev1.NetworkEndpoint{{IpAddress: "127.0.0.1", Port: origin.port()}}}).Do())
	e.do(c.HealthChecks.Insert(proj, &computev1.HealthCheck{Name: "hc", Type: "HTTP", CheckIntervalSec: 1, TimeoutSec: 1,
		HttpHealthCheck: &computev1.HTTPHealthCheck{PortSpecification: "USE_SERVING_PORT", RequestPath: "/healthz"}}).Do())
	e.do(c.BackendServices.Insert(proj, &computev1.BackendService{
		Name: "api", LoadBalancingScheme: "EXTERNAL_MANAGED", Protocol: "HTTP", HealthChecks: []string{"global/healthChecks/hc"},
		Backends:              []*computev1.Backend{{Group: "zones/us-central1-a/networkEndpointGroups/neg", BalancingMode: "RATE", MaxRatePerEndpoint: 100}},
		CustomRequestHeaders:  []string{"X-Client-Region:{client_region}", "X-TLS:{tls_version}"},
		CustomResponseHeaders: []string{"X-Served-By:gcpemu"},
		LogConfig:             &computev1.BackendServiceLogConfig{Enable: true},
	}).Do())

	// Static content in GCS behind a CDN-enabled backend bucket.
	sc, err := storage.NewClient(ctx, option.WithEndpoint("http://"+e.inst.Endpoint("gcs")+"/storage/v1/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	bkt := sc.Bucket("static-assets")
	if err := bkt.Create(ctx, proj, &storage.BucketAttrs{Website: &storage.BucketWebsite{MainPageSuffix: "index.html", NotFoundPage: "404.html"}}); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"index.html": "<h1>home</h1>", "app.css": "body{}", "404.html": "missing"} {
		w := bkt.Object(name).NewWriter(ctx)
		if strings.HasSuffix(name, ".css") {
			w.ContentType = "text/css"
		} else {
			w.ContentType = "text/html"
		}
		_, _ = io.WriteString(w, body)
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
	}
	e.do(c.BackendBuckets.Insert(proj, &computev1.BackendBucket{Name: "static", BucketName: "static-assets", EnableCdn: true}).Do())

	e.do(c.UrlMaps.Insert(proj, &computev1.UrlMap{
		Name: "map", DefaultService: "global/backendBuckets/static",
		HeaderAction: &computev1.HttpHeaderAction{ResponseHeadersToAdd: []*computev1.HttpHeaderOption{
			{HeaderName: "X-Cache-Status", HeaderValue: "{cdn_cache_status}", Replace: true}}},
		HostRules: []*computev1.HostRule{{Hosts: []string{"app.example.test"}, PathMatcher: "app"}},
		PathMatchers: []*computev1.PathMatcher{{
			Name: "app", DefaultService: "global/backendBuckets/static",
			PathRules: []*computev1.PathRule{
				{Paths: []string{"/api/*"}, Service: "global/backendServices/api"},
				{Paths: []string{"/old"}, UrlRedirect: &computev1.HttpRedirectAction{PathRedirect: "/new", RedirectResponseCode: "FOUND"}},
			},
		}},
		Tests: []*computev1.UrlMapTest{{Host: "app.example.test", Path: "/api/x", Service: "global/backendServices/api"}},
	}).Do())

	leaf, err := e.inst.Env.CA.Issue(ca.Leaf{DNSNames: []string{"app.example.test"}})
	if err != nil {
		t.Fatal(err)
	}
	chain, key, _ := ca.EncodeCert(leaf)
	e.do(c.SslCertificates.Insert(proj, &computev1.SslCertificate{Name: "app-cert", Certificate: string(chain), PrivateKey: string(key)}).Do())
	e.do(c.SslPolicies.Insert(proj, &computev1.SslPolicy{Name: "modern", Profile: "MODERN", MinTlsVersion: "TLS_1_2"}).Do())
	e.do(c.TargetHttpsProxies.Insert(proj, &computev1.TargetHttpsProxy{Name: "https", UrlMap: "global/urlMaps/map",
		SslCertificates: []string{"global/sslCertificates/app-cert"}, SslPolicy: "global/sslPolicies/modern"}).Do())
	e.do(c.GlobalAddresses.Insert(proj, &computev1.Address{Name: "lb-ip"}).Do())
	e.do(c.GlobalForwardingRules.Insert(proj, &computev1.ForwardingRule{Name: "https-fr", LoadBalancingScheme: "EXTERNAL_MANAGED",
		IPAddress: "global/addresses/lb-ip", PortRange: "8080", Target: "global/targetHttpsProxies/https"}).Do())
	fr, err = c.GlobalForwardingRules.Get(proj, "https-fr").Do()
	if err != nil {
		t.Fatal(err)
	}
	return fr
}

// client returns an HTTPS client that trusts the emulator CA and dials
// the forwarding rule's mapped listener.
func (e *env) client(listener string) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: e.inst.Env.CA.Pool()},
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, listener)
			},
			ForceAttemptHTTP2: true,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Timeout:       10 * time.Second,
	}
}

func get(t *testing.T, cl *http.Client, url string) (*http.Response, string) {
	t.Helper()
	resp, err := cl.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(b)
}

func TestLoadBalancerEndToEnd(t *testing.T) {
	e := start(t)
	origin := newBackend(t)
	fr := e.stack(origin)
	c := e.c

	if fr.IPAddress == "" || fr.PortRange != "8080-8080" || !strings.HasSuffix(fr.Target, "/global/targetHttpsProxies/https") {
		t.Fatalf("forwarding rule = %+v", fr)
	}
	addr, err := c.GlobalAddresses.Get(proj, "lb-ip").Do()
	if err != nil || addr.Status != "IN_USE" || addr.Address != fr.IPAddress {
		t.Fatalf("address = %+v, %v", addr, err)
	}
	listener := e.inst.Endpoint("lb:https-fr")
	if listener == "" {
		t.Fatal("no listener endpoint recorded")
	}
	if v := e.inst.EnvVars()["GCPEMU_LB_HTTPS_FR"]; v != listener {
		t.Errorf("GCPEMU_LB_HTTPS_FR = %q, want %q", v, listener)
	}
	cl := e.client(listener)
	base := "https://app.example.test:8080"

	// Static content from GCS through Cloud CDN: miss, then hit.
	resp, body := get(t, cl, base+"/app.css")
	if resp.StatusCode != 200 || body != "body{}" || resp.Header.Get("X-Cache-Status") != "miss" {
		t.Fatalf("first static: %d %q cache=%q", resp.StatusCode, body, resp.Header.Get("X-Cache-Status"))
	}
	if resp.ProtoMajor != 2 {
		t.Errorf("client protocol = %s, want HTTP/2", resp.Proto)
	}
	resp, _ = get(t, cl, base+"/app.css")
	if resp.Header.Get("X-Cache-Status") != "hit" || resp.Header.Get("Age") == "" {
		t.Fatalf("second static: cache=%q age=%q", resp.Header.Get("X-Cache-Status"), resp.Header.Get("Age"))
	}
	resp, body = get(t, cl, base+"/")
	if resp.StatusCode != 200 || body != "<h1>home</h1>" {
		t.Fatalf("index: %d %q", resp.StatusCode, body)
	}
	resp, body = get(t, cl, base+"/nope.txt")
	if resp.StatusCode != 404 || body != "missing" {
		t.Fatalf("404 page: %d %q", resp.StatusCode, body)
	}

	// Cache invalidation (FR-CDN-005): effective when the operation is done.
	e.do(c.UrlMaps.InvalidateCache(proj, "map", &computev1.CacheInvalidationRule{Path: "/*"}).Do())
	resp, _ = get(t, cl, base+"/app.css")
	if resp.Header.Get("X-Cache-Status") != "miss" {
		t.Fatalf("after invalidation: cache=%q", resp.Header.Get("X-Cache-Status"))
	}

	// API path to the NEG backend, once the health check passes.
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, body = get(t, cl, base+"/api/users")
		if resp.StatusCode == 200 || time.Now().After(deadline) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if resp.StatusCode != 200 || body != "backend:app.example.test:8080/api/users" || resp.Header.Get("X-Served-By") != "gcpemu" {
		t.Fatalf("api: %d %q %v", resp.StatusCode, body, resp.Header)
	}
	if resp.Header.Get("X-Cache-Status") != "disabled" {
		t.Errorf("api cache status = %q", resp.Header.Get("X-Cache-Status"))
	}
	var seen *http.Request
	for len(origin.last) > 0 {
		seen = <-origin.last
	}
	if seen == nil || seen.Header.Get("X-Client-Region") != "ZZ" || seen.Header.Get("X-TLS") != "TLSv1.3" ||
		!strings.HasPrefix(seen.Header.Get("X-Forwarded-For"), "127.0.0.") || seen.Header.Get("X-Forwarded-Proto") != "https" {
		t.Fatalf("backend request headers: %v", seen)
	}

	// Redirect.
	resp, _ = get(t, cl, base+"/old?x=1")
	if resp.StatusCode != 302 || resp.Header.Get("Location") != "https://app.example.test:8080/new?x=1" {
		t.Fatalf("redirect: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}

	// SSL policy: TLS 1.1 is refused.
	old := &tls.Config{ServerName: "app.example.test", RootCAs: e.inst.Env.CA.Pool(), MaxVersion: tls.VersionTLS11, MinVersion: tls.VersionTLS10}
	if conn, err := tls.Dial("tcp", listener, old); err == nil {
		conn.Close()
		t.Error("TLS 1.1 handshake succeeded despite minTlsVersion TLS_1_2")
	}

	// getHealth.
	h, err := c.BackendServices.GetHealth(proj, "api", &computev1.ResourceGroupReference{Group: "zones/us-central1-a/networkEndpointGroups/neg"}).Do()
	if err != nil || len(h.HealthStatus) != 1 || h.HealthStatus[0].HealthState != "HEALTHY" || h.HealthStatus[0].ForwardingRuleIp != fr.IPAddress {
		t.Fatalf("getHealth = %+v, %v", h, err)
	}

	// Resource-in-use rules (FR-CORE-026).
	for name, call := range map[string]func() error{
		"backend service": func() error { _, err := c.BackendServices.Delete(proj, "api").Do(); return err },
		"url map":         func() error { _, err := c.UrlMaps.Delete(proj, "map").Do(); return err },
		"address":         func() error { _, err := c.GlobalAddresses.Delete(proj, "lb-ip").Do(); return err },
		"neg": func() error {
			_, err := c.NetworkEndpointGroups.Delete(proj, "us-central1-a", "neg").Do()
			return err
		},
		"health check": func() error { _, err := c.HealthChecks.Delete(proj, "hc").Do(); return err },
	} {
		if code, reason := apiErr(call()); code != 400 || reason != "resourceInUseByAnotherResource" {
			t.Errorf("delete %s: %d %s", name, code, reason)
		}
	}

	// Access log in Cloud Logging shape.
	entries := e.inst.Env.RequestLog.Entries("lb")
	if len(entries) < 5 {
		t.Errorf("request log has %d lb entries", len(entries))
	}

	// DNS → LB (FR-INT-004): an A record for the forwarding rule IP
	// resolves to the mapped listener.
	dc, err := dnsv1.NewService(context.Background(), option.WithEndpoint(e.inst.GatewayURL()+"/dns/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dc.ManagedZones.Create(proj, &dnsv1.ManagedZone{Name: "example", DnsName: "example.test.", Description: "x"}).Do(); err != nil {
		t.Fatal(err)
	}
	if _, err := dc.Changes.Create(proj, "example", &dnsv1.Change{Additions: []*dnsv1.ResourceRecordSet{
		{Name: "app.example.test.", Type: "A", Ttl: 60, Rrdatas: []string{fr.IPAddress}}}}).Do(); err != nil {
		t.Fatal(err)
	}
	ds, _ := e.inst.Env.Lookup("dns")
	rrs, err := ds.(*dns.Service).Resolve(context.Background(), "app.example.test", mdns.TypeA)
	lh, _, _ := net.SplitHostPort(listener)
	if err != nil || len(rrs) != 1 || rrs[0].(*mdns.A).A.String() != lh {
		t.Fatalf("resolve = %v, %v (listener %s)", rrs, err, listener)
	}

	// Teardown in dependency order succeeds.
	e.do(c.GlobalForwardingRules.Delete(proj, "https-fr").Do())
	if a, _ := c.GlobalAddresses.Get(proj, "lb-ip").Do(); a == nil || a.Status != "RESERVED" {
		t.Errorf("address after forwarding rule delete = %+v", a)
	}
	e.do(c.TargetHttpsProxies.Delete(proj, "https").Do())
	e.do(c.UrlMaps.Delete(proj, "map").Do())
	e.do(c.BackendServices.Delete(proj, "api").Do())
	e.do(c.BackendBuckets.Delete(proj, "static").Do())
	e.do(c.HealthChecks.Delete(proj, "hc").Do())
	if _, err := cl.Get(base + "/"); err == nil {
		t.Error("listener still serving after the forwarding rule was deleted")
	}
}

func TestManagedCertificate(t *testing.T) {
	e := start(t)
	c := e.c
	origin := newBackend(t)
	e.do(c.Networks.Insert(proj, &computev1.Network{Name: "vpc", AutoCreateSubnetworks: false, ForceSendFields: []string{"AutoCreateSubnetworks"}}).Do())
	e.do(c.NetworkEndpointGroups.Insert(proj, "us-central1-a", &computev1.NetworkEndpointGroup{
		Name: "neg", Network: "global/networks/vpc", DefaultPort: origin.port()}).Do())
	e.do(c.NetworkEndpointGroups.AttachNetworkEndpoints(proj, "us-central1-a", "neg", &computev1.NetworkEndpointGroupsAttachEndpointsRequest{
		NetworkEndpoints: []*computev1.NetworkEndpoint{{IpAddress: "127.0.0.1"}}}).Do())
	e.do(c.BackendServices.Insert(proj, &computev1.BackendService{Name: "bs", LoadBalancingScheme: "EXTERNAL_MANAGED",
		Backends: []*computev1.Backend{{Group: "zones/us-central1-a/networkEndpointGroups/neg"}}}).Do())
	e.do(c.UrlMaps.Insert(proj, &computev1.UrlMap{Name: "map", DefaultService: "global/backendServices/bs"}).Do())
	e.do(c.SslCertificates.Insert(proj, &computev1.SslCertificate{Name: "managed", Type: "MANAGED",
		Managed: &computev1.SslCertificateManagedSslCertificate{Domains: []string{"shop.example.test"}}}).Do())
	cert, err := c.SslCertificates.Get(proj, "managed").Do()
	if err != nil || cert.Managed.Status != "PROVISIONING" || cert.Managed.DomainStatus["shop.example.test"] != "PROVISIONING" {
		t.Fatalf("new managed cert = %+v, %v", cert.Managed, err)
	}
	e.do(c.TargetHttpsProxies.Insert(proj, &computev1.TargetHttpsProxy{Name: "p", UrlMap: "global/urlMaps/map",
		SslCertificates: []string{"global/sslCertificates/managed"}}).Do())
	e.do(c.GlobalForwardingRules.Insert(proj, &computev1.ForwardingRule{Name: "fr", LoadBalancingScheme: "EXTERNAL_MANAGED",
		PortRange: "8080", Target: "global/targetHttpsProxies/p"}).Do())
	fr, _ := c.GlobalForwardingRules.Get(proj, "fr").Do()
	time.Sleep(1500 * time.Millisecond)
	if cert, _ = c.SslCertificates.Get(proj, "managed").Do(); cert.Managed.Status != "PROVISIONING" {
		t.Fatalf("cert became %s before DNS pointed at the load balancer", cert.Managed.Status)
	}
	// Point DNS at the forwarding rule.
	dc, _ := dnsv1.NewService(context.Background(), option.WithEndpoint(e.inst.GatewayURL()+"/dns/"), option.WithoutAuthentication())
	if _, err := dc.ManagedZones.Create(proj, &dnsv1.ManagedZone{Name: "shop", DnsName: "example.test.", Description: "x"}).Do(); err != nil {
		t.Fatal(err)
	}
	if _, err := dc.Changes.Create(proj, "shop", &dnsv1.Change{Additions: []*dnsv1.ResourceRecordSet{
		{Name: "shop.example.test.", Type: "A", Ttl: 60, Rrdatas: []string{fr.IPAddress}}}}).Do(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for cert.Managed.Status != "ACTIVE" && time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
		cert, _ = c.SslCertificates.Get(proj, "managed").Do()
	}
	if cert.Managed.Status != "ACTIVE" || cert.Managed.DomainStatus["shop.example.test"] != "ACTIVE" ||
		!strings.Contains(cert.Certificate, "BEGIN CERTIFICATE") || cert.ExpireTime == "" || cert.PrivateKey != "" {
		t.Fatalf("managed cert = %+v", cert)
	}
	resp, body := get(t, e.client(e.inst.Endpoint("lb:fr")), "https://shop.example.test:8080/x")
	if resp.StatusCode != 200 || body != "backend:shop.example.test:8080/x" {
		t.Fatalf("via managed cert: %d %q", resp.StatusCode, body)
	}
}

func TestValidationAndLists(t *testing.T) {
	e := start(t)
	c := e.c
	e.do(c.BackendBuckets.Insert(proj, &computev1.BackendBucket{Name: "b1", BucketName: "x"}).Do())
	e.do(c.BackendBuckets.Insert(proj, &computev1.BackendBucket{Name: "b2", BucketName: "y"}).Do())
	// Unknown backend reference.
	_, err := c.UrlMaps.Insert(proj, &computev1.UrlMap{Name: "bad", DefaultService: "global/backendServices/nope"}).Do()
	if code, _ := apiErr(err); code != 404 {
		t.Errorf("unknown backend: %v", err)
	}
	// Failing tests reject the insert.
	_, err = c.UrlMaps.Insert(proj, &computev1.UrlMap{Name: "bad", DefaultService: "global/backendBuckets/b1",
		Tests: []*computev1.UrlMapTest{{Host: "h", Path: "/", Service: "global/backendBuckets/b2"}}}).Do()
	if code, _ := apiErr(err); code != 400 {
		t.Errorf("failing tests: %v", err)
	}
	// urlMaps.validate reports load errors and test failures.
	e.do(c.UrlMaps.Insert(proj, &computev1.UrlMap{Name: "m", DefaultService: "global/backendBuckets/b1"}).Do())
	res, err := c.UrlMaps.Validate(proj, "m", &computev1.UrlMapsValidateRequest{Resource: &computev1.UrlMap{
		Name: "m", DefaultService: "global/backendBuckets/b1",
		Tests: []*computev1.UrlMapTest{{Host: "h", Path: "/", Service: "global/backendBuckets/b2"}}}}).Do()
	if err != nil || res.Result.TestPassed || !res.Result.LoadSucceeded {
		t.Fatalf("validate = %+v, %v", res, err)
	}
	// Patch with a stale fingerprint is rejected.
	m, _ := c.UrlMaps.Get(proj, "m").Do()
	_, err = c.UrlMaps.Patch(proj, "m", &computev1.UrlMap{Fingerprint: "AAAAAAAAAAA=", Description: "x"}).Do()
	if code, _ := apiErr(err); code != 412 {
		t.Errorf("stale fingerprint: %v", err)
	}
	e.do(c.UrlMaps.Patch(proj, "m", &computev1.UrlMap{Fingerprint: m.Fingerprint, Description: "x"}).Do())
	// Regional variants share the implementation.
	e.do(c.RegionHealthChecks.Insert(proj, "us-central1", &computev1.HealthCheck{Name: "rhc", TcpHealthCheck: &computev1.TCPHealthCheck{Port: 80}}).Do())
	e.do(c.RegionBackendServices.Insert(proj, "us-central1", &computev1.BackendService{Name: "rbs", LoadBalancingScheme: "INTERNAL_MANAGED",
		HealthChecks: []string{"regions/us-central1/healthChecks/rhc"}}).Do())
	e.do(c.RegionUrlMaps.Insert(proj, "us-central1", &computev1.UrlMap{Name: "rm", DefaultService: "regions/us-central1/backendServices/rbs"}).Do())
	e.do(c.RegionTargetHttpProxies.Insert(proj, "us-central1", &computev1.TargetHttpProxy{Name: "rp", UrlMap: "regions/us-central1/urlMaps/rm"}).Do())
	_, err = c.RegionUrlMaps.Insert(proj, "us-central1", &computev1.UrlMap{Name: "x", DefaultService: "global/backendBuckets/b1"}).Do()
	if code, _ := apiErr(err); code != 400 && code != 404 {
		t.Errorf("regional map with global backend: %v", err)
	}
	hc, _ := c.RegionHealthChecks.Get(proj, "us-central1", "rhc").Do()
	if hc.Type != "TCP" || hc.CheckIntervalSec != 5 || hc.TcpHealthCheck.ProxyHeader != "NONE" || !strings.Contains(hc.Region, "/regions/us-central1") {
		t.Errorf("regional health check = %+v", hc)
	}
	// Lists with filters and aggregated lists.
	l, err := c.BackendBuckets.List(proj).Filter(`name = "b2"`).Do()
	if err != nil || len(l.Items) != 1 || l.Items[0].Name != "b2" {
		t.Fatalf("filtered list = %+v, %v", l, err)
	}
	agg, err := c.UrlMaps.AggregatedList(proj).Do()
	if err != nil || len(agg.Items["global"].UrlMaps) != 1 || len(agg.Items["regions/us-central1"].UrlMaps) != 1 {
		t.Fatalf("aggregated = %+v, %v", agg, err)
	}
	// Self-managed certificate PEM validation.
	_, err = c.SslCertificates.Insert(proj, &computev1.SslCertificate{Name: "bad", Certificate: "nope", PrivateKey: "nope"}).Do()
	if code, _ := apiErr(err); code != 400 {
		t.Errorf("bad PEM: %v", err)
	}
	feats, err := c.SslPolicies.ListAvailableFeatures(proj).Do()
	if err != nil || len(feats.Features) < 10 {
		t.Errorf("features = %v, %v", feats, err)
	}
}

func TestSeed(t *testing.T) {
	e := start(t)
	dir := t.TempDir()
	leaf, _ := e.inst.Env.CA.Issue(ca.Leaf{DNSNames: []string{"seed.example.test"}})
	chain, key, _ := ca.EncodeCert(leaf)
	_ = os.WriteFile(filepath.Join(dir, "c.pem"), chain, 0o600)
	_ = os.WriteFile(filepath.Join(dir, "k.pem"), key, 0o600)
	seed := `
lb:
  project: ` + proj + `
  healthChecks:
    - {name: hc, type: HTTP, httpHealthCheck: {portSpecification: USE_SERVING_PORT}}
  sslCertificates:
    - {name: app, certificateFile: c.pem, privateKeyFile: k.pem}
  backendBuckets:
    - {name: static, bucketName: assets, enableCdn: true}
  backendServices:
    - {name: api, loadBalancingScheme: EXTERNAL_MANAGED, healthChecks: [global/healthChecks/hc]}
  urlMaps:
    - name: web
      defaultService: global/backendBuckets/static
      hostRules: [{hosts: ["*"], pathMatcher: m}]
      pathMatchers:
        - {name: m, defaultService: global/backendBuckets/static, pathRules: [{paths: ["/api/*"], service: global/backendServices/api}]}
  targetHttpsProxies:
    - {name: web, urlMap: global/urlMaps/web, sslCertificates: [global/sslCertificates/app]}
  forwardingRules:
    - {name: web, loadBalancingScheme: EXTERNAL_MANAGED, portRange: "8080", target: global/targetHttpsProxies/web}
`
	path := filepath.Join(dir, "seed.yaml")
	_ = os.WriteFile(path, []byte(seed), 0o600)
	for i := 0; i < 2; i++ { // idempotent
		if err := e.inst.ApplySeed(context.Background(), path); err != nil {
			t.Fatalf("apply %d: %v", i, err)
		}
	}
	fr, err := e.c.GlobalForwardingRules.Get(proj, "web").Do()
	if err != nil || fr.IPAddress == "" {
		t.Fatalf("seeded forwarding rule = %+v, %v", fr, err)
	}
	if e.inst.Endpoint("lb:web") == "" {
		t.Error("seeded forwarding rule has no listener")
	}
}

// rest performs a JSON REST call on the gateway and fails on errors.
func (e *env) rest(method, path string, body any) map[string]any {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, e.inst.GatewayURL()+path, rd)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if resp.StatusCode != 200 || out["error"] != nil {
		e.t.Fatalf("%s %s: %d %v", method, path, resp.StatusCode, out)
	}
	return out
}

// backendMTLS configures Certificate Manager / Network Security resources
// for backend mTLS: the LB's client certificate (issued by clientCA) and a
// trust config trusting meshCA. It returns the authenticationConfig name.
func (e *env) backendMTLS(clientCA, meshCA *ca.CA) string {
	loc := "projects/" + proj + "/locations/global"
	leaf, err := clientCA.Issue(ca.Leaf{DNSNames: []string{"lb.client.test"}, ClientOnly: true})
	if err != nil {
		e.t.Fatal(err)
	}
	chain, key, _ := ca.EncodeCert(leaf)
	e.rest("POST", "/certificatemanager/v1/"+loc+"/certificates?certificateId=lb-client", map[string]any{
		"scope": "CLIENT_AUTH", "selfManaged": map[string]any{"pemCertificate": string(chain), "pemPrivateKey": string(key)}})
	e.rest("POST", "/certificatemanager/v1/"+loc+"/trustConfigs?trustConfigId=mesh", map[string]any{
		"trustStores": []any{map[string]any{"trustAnchors": []any{map[string]any{"pemCertificate": string(meshCA.PEM())}}}}})
	e.rest("POST", "/networksecurity/v1/"+loc+"/backendAuthenticationConfigs?backendAuthenticationConfigId=bac", map[string]any{
		"clientCertificate": loc + "/certificates/lb-client", "trustConfig": loc + "/trustConfigs/mesh", "wellKnownRoots": "NONE"})
	return "//networksecurity.googleapis.com/" + loc + "/backendAuthenticationConfigs/bac"
}

func TestBackendMTLS(t *testing.T) {
	e := start(t)
	c := e.c
	clientCA, _ := ca.Load(t.TempDir(), "lb-client-ca")
	meshCA, _ := ca.Load(t.TempDir(), "mesh-ca")
	auth := e.backendMTLS(clientCA, meshCA)

	// A backend that requires client certificates from clientCA, serving a
	// certificate from meshCA (like an Istio gateway with MUTUAL TLS).
	srvCert, _ := meshCA.Issue(ca.Leaf{DNSNames: []string{"gateway.mesh.test"}})
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "mtls ok from "+r.TLS.PeerCertificates[0].Subject.CommonName)
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{*srvCert}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: clientCA.Pool()}
	srv.StartTLS()
	defer srv.Close()
	_, ps, _ := net.SplitHostPort(srv.Listener.Addr().String())
	port, _ := strconv.Atoi(ps)

	e.do(c.Networks.Insert(proj, &computev1.Network{Name: "vpc", AutoCreateSubnetworks: false, ForceSendFields: []string{"AutoCreateSubnetworks"}}).Do())
	e.do(c.NetworkEndpointGroups.Insert(proj, "us-central1-a", &computev1.NetworkEndpointGroup{Name: "neg", Network: "global/networks/vpc", DefaultPort: int64(port)}).Do())
	e.do(c.NetworkEndpointGroups.AttachNetworkEndpoints(proj, "us-central1-a", "neg", &computev1.NetworkEndpointGroupsAttachEndpointsRequest{
		NetworkEndpoints: []*computev1.NetworkEndpoint{{IpAddress: "127.0.0.1"}}}).Do())
	e.do(c.BackendServices.Insert(proj, &computev1.BackendService{Name: "mesh", LoadBalancingScheme: "EXTERNAL_MANAGED", Protocol: "HTTPS",
		Backends:    []*computev1.Backend{{Group: "zones/us-central1-a/networkEndpointGroups/neg"}},
		TlsSettings: &computev1.BackendServiceTlsSettings{AuthenticationConfig: auth, Sni: "gateway.mesh.test"}}).Do())
	e.do(c.UrlMaps.Insert(proj, &computev1.UrlMap{Name: "map", DefaultService: "global/backendServices/mesh"}).Do())
	e.do(c.TargetHttpProxies.Insert(proj, &computev1.TargetHttpProxy{Name: "p", UrlMap: "global/urlMaps/map"}).Do())
	e.do(c.GlobalForwardingRules.Insert(proj, &computev1.ForwardingRule{Name: "fr", LoadBalancingScheme: "EXTERNAL_MANAGED", PortRange: "8080", Target: "global/targetHttpProxies/p"}).Do())

	cl := &http.Client{Timeout: 10 * time.Second}
	resp, body := get(t, cl, "http://"+e.inst.Endpoint("lb:fr")+"/api/health")
	if resp.StatusCode != 200 || body != "mtls ok from lb.client.test" {
		t.Fatalf("through LB: %d %q", resp.StatusCode, body)
	}
	// A direct client without the LB's certificate is refused.
	direct := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: meshCA.Pool(), ServerName: "gateway.mesh.test"}}}
	if r, err := direct.Get(srv.URL); err == nil {
		r.Body.Close()
		t.Fatal("direct request without client certificate succeeded")
	}
	// The LB rejects a backend whose certificate does not match the
	// configured subjectAltNames.
	bs, _ := c.BackendServices.Get(proj, "mesh").Do()
	bs.TlsSettings.SubjectAltNames = []*computev1.BackendServiceTlsSettingsSubjectAltName{{DnsName: "other.mesh.test"}}
	e.do(c.BackendServices.Update(proj, "mesh", bs).Do())
	resp, _ = get(t, cl, "http://"+e.inst.Endpoint("lb:fr")+"/api/health")
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("SAN mismatch: %d", resp.StatusCode)
	}
}

func TestFrontendMTLS(t *testing.T) {
	e := start(t)
	c := e.c
	origin := newBackend(t)
	clientCA, _ := ca.Load(t.TempDir(), "client-ca")
	loc := "projects/" + proj + "/locations/global"
	e.rest("POST", "/certificatemanager/v1/"+loc+"/trustConfigs?trustConfigId=clients", map[string]any{
		"trustStores": []any{map[string]any{"trustAnchors": []any{map[string]any{"pemCertificate": string(clientCA.PEM())}}}}})
	e.rest("POST", "/networksecurity/v1/"+loc+"/serverTlsPolicies?serverTlsPolicyId=strict", map[string]any{
		"mtlsPolicy": map[string]any{"clientValidationMode": "REJECT_INVALID", "clientValidationTrustConfig": loc + "/trustConfigs/clients"}})

	e.do(c.Networks.Insert(proj, &computev1.Network{Name: "vpc", AutoCreateSubnetworks: false, ForceSendFields: []string{"AutoCreateSubnetworks"}}).Do())
	e.do(c.NetworkEndpointGroups.Insert(proj, "us-central1-a", &computev1.NetworkEndpointGroup{Name: "neg", Network: "global/networks/vpc", DefaultPort: origin.port()}).Do())
	e.do(c.NetworkEndpointGroups.AttachNetworkEndpoints(proj, "us-central1-a", "neg", &computev1.NetworkEndpointGroupsAttachEndpointsRequest{
		NetworkEndpoints: []*computev1.NetworkEndpoint{{IpAddress: "127.0.0.1"}}}).Do())
	e.do(c.BackendServices.Insert(proj, &computev1.BackendService{Name: "bs", LoadBalancingScheme: "EXTERNAL_MANAGED",
		Backends:             []*computev1.Backend{{Group: "zones/us-central1-a/networkEndpointGroups/neg"}},
		CustomRequestHeaders: []string{"X-Client-Cert-Present:{client_cert_present}", "X-Client-Cert-Verified:{client_cert_chain_verified}"}}).Do())
	e.do(c.UrlMaps.Insert(proj, &computev1.UrlMap{Name: "map", DefaultService: "global/backendServices/bs"}).Do())
	leaf, _ := e.inst.Env.CA.Issue(ca.Leaf{DNSNames: []string{"secure.example.test"}})
	chain, key, _ := ca.EncodeCert(leaf)
	e.do(c.SslCertificates.Insert(proj, &computev1.SslCertificate{Name: "cert", Certificate: string(chain), PrivateKey: string(key)}).Do())
	e.do(c.TargetHttpsProxies.Insert(proj, &computev1.TargetHttpsProxy{Name: "p", UrlMap: "global/urlMaps/map",
		SslCertificates: []string{"global/sslCertificates/cert"}, ServerTlsPolicy: "//networksecurity.googleapis.com/" + loc + "/serverTlsPolicies/strict"}).Do())
	e.do(c.GlobalForwardingRules.Insert(proj, &computev1.ForwardingRule{Name: "fr", LoadBalancingScheme: "EXTERNAL_MANAGED", PortRange: "8080", Target: "global/targetHttpsProxies/p"}).Do())

	listener := e.inst.Endpoint("lb:fr")
	cl := e.client(listener)
	if _, err := cl.Get("https://secure.example.test:8080/"); err == nil {
		t.Fatal("request without client certificate accepted under REJECT_INVALID")
	}
	cc, _ := clientCA.Issue(ca.Leaf{DNSNames: []string{"user.test"}, ClientOnly: true})
	cl.Transport.(*http.Transport).TLSClientConfig.Certificates = []tls.Certificate{*cc}
	resp, _ := get(t, cl, "https://secure.example.test:8080/")
	if resp.StatusCode != 200 {
		t.Fatalf("with client certificate: %d", resp.StatusCode)
	}
	r := <-origin.last
	if r.Header.Get("X-Client-Cert-Present") != "true" || r.Header.Get("X-Client-Cert-Verified") != "true" {
		t.Fatalf("client cert headers: %v", r.Header)
	}
}

// TestProtocols covers WebSocket upgrades from clients and H2C backends
// (FR-LB-005).
func TestProtocols(t *testing.T) {
	e := start(t)
	c := e.c
	// Backend: H2C (HTTP/2 cleartext) with a WebSocket-style upgrade path.
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") == "websocket" {
			conn, rw, err := http.NewResponseController(w).Hijack()
			if err != nil {
				return
			}
			defer conn.Close()
			_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
			_ = rw.Flush()
			line, _ := rw.ReadString('\n')
			_, _ = rw.WriteString("echo:" + line)
			_ = rw.Flush()
			return
		}
		_, _ = io.WriteString(w, r.Proto)
	})
	srv := &http.Server{Handler: h, Protocols: new(http.Protocols)}
	srv.Protocols.SetHTTP1(true)
	srv.Protocols.SetUnencryptedHTTP2(true)
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	go srv.Serve(ln)
	defer srv.Close()
	_, ps, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(ps)

	e.do(c.Networks.Insert(proj, &computev1.Network{Name: "vpc", AutoCreateSubnetworks: false, ForceSendFields: []string{"AutoCreateSubnetworks"}}).Do())
	e.do(c.NetworkEndpointGroups.Insert(proj, "us-central1-a", &computev1.NetworkEndpointGroup{Name: "neg", Network: "global/networks/vpc", DefaultPort: int64(port)}).Do())
	e.do(c.NetworkEndpointGroups.AttachNetworkEndpoints(proj, "us-central1-a", "neg", &computev1.NetworkEndpointGroupsAttachEndpointsRequest{
		NetworkEndpoints: []*computev1.NetworkEndpoint{{IpAddress: "127.0.0.1"}}}).Do())
	for _, p := range []string{"h2c", "http1"} {
		proto := "H2C"
		if p == "http1" {
			proto = "HTTP"
		}
		e.do(c.BackendServices.Insert(proj, &computev1.BackendService{Name: p, LoadBalancingScheme: "EXTERNAL_MANAGED", Protocol: proto,
			Backends: []*computev1.Backend{{Group: "zones/us-central1-a/networkEndpointGroups/neg"}}}).Do())
	}
	e.do(c.UrlMaps.Insert(proj, &computev1.UrlMap{Name: "map", DefaultService: "global/backendServices/h2c",
		HostRules:    []*computev1.HostRule{{Hosts: []string{"*"}, PathMatcher: "m"}},
		PathMatchers: []*computev1.PathMatcher{{Name: "m", DefaultService: "global/backendServices/h2c", PathRules: []*computev1.PathRule{{Paths: []string{"/ws"}, Service: "global/backendServices/http1"}}}}}).Do())
	e.do(c.TargetHttpProxies.Insert(proj, &computev1.TargetHttpProxy{Name: "p", UrlMap: "global/urlMaps/map"}).Do())
	e.do(c.GlobalForwardingRules.Insert(proj, &computev1.ForwardingRule{Name: "fr", LoadBalancingScheme: "EXTERNAL_MANAGED", PortRange: "8080", Target: "global/targetHttpProxies/p"}).Do())
	listener := e.inst.Endpoint("lb:fr")

	_, body := get(t, &http.Client{}, "http://"+listener+"/")
	if body != "HTTP/2.0" {
		t.Errorf("H2C backend saw %q", body)
	}
	conn, err := net.Dial("tcp", listener)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, _ = io.WriteString(conn, "GET /ws HTTP/1.1\r\nHost: x\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\n\r\n")
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil || resp.StatusCode != 101 {
		t.Fatalf("upgrade: %v %v", resp, err)
	}
	_, _ = io.WriteString(conn, "hello\n")
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, _ := br.ReadString('\n')
	if line != "echo:hello\n" {
		t.Fatalf("websocket echo = %q", line)
	}
}

// TestPushViaLB is FR-INT-011: a Pub/Sub push subscription whose endpoint
// is served by the load balancer delivers through it with an OIDC token.
func TestPushViaLB(t *testing.T) {
	inst := emutest.Start(t, []string{"lb", "cdn", "pubsub"})
	svc, _ := inst.Env.Lookup("lb")
	svc.(*lb.Service).SetOptions(lb.Options{DirectDial: true})
	c, _ := computev1.NewService(context.Background(), option.WithEndpoint(inst.GatewayURL()+"/compute/v1/"), option.WithoutAuthentication())
	e := &env{t: t, inst: inst, c: c}
	origin := newBackend(t)
	fr := e.stack(origin)
	dc, _ := dnsv1.NewService(context.Background(), option.WithEndpoint(inst.GatewayURL()+"/dns/"), option.WithoutAuthentication())
	if _, err := dc.ManagedZones.Create(proj, &dnsv1.ManagedZone{Name: "example", DnsName: "example.test.", Description: "x"}).Do(); err != nil {
		t.Fatal(err)
	}
	if _, err := dc.Changes.Create(proj, "example", &dnsv1.Change{Additions: []*dnsv1.ResourceRecordSet{
		{Name: "app.example.test.", Type: "A", Ttl: 60, Rrdatas: []string{fr.IPAddress}}}}).Do(); err != nil {
		t.Fatal(err)
	}
	sa := "pusher@" + proj + ".iam.gserviceaccount.com"
	e.rest("POST", "/iam/v1/projects/"+proj+"/serviceAccounts", map[string]any{"accountId": "pusher"})
	e.rest("PUT", "/pubsub/v1/projects/"+proj+"/topics/events", map[string]any{})
	e.rest("PUT", "/pubsub/v1/projects/"+proj+"/subscriptions/events-push", map[string]any{
		"topic": "projects/" + proj + "/topics/events",
		"pushConfig": map[string]any{"pushEndpoint": "https://app.example.test:8080/api/push",
			"oidcToken": map[string]any{"serviceAccountEmail": sa, "audience": "app"}}})
	e.rest("POST", "/pubsub/v1/projects/"+proj+"/topics/events:publish", map[string]any{
		"messages": []any{map[string]any{"data": base64.StdEncoding.EncodeToString([]byte("hi"))}}})
	timeout := time.After(15 * time.Second)
	for {
		select {
		case r := <-origin.last:
			if r.URL.Path != "/api/push" {
				continue
			}
			if r.Method != "POST" || !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ey") {
				t.Fatalf("push request %s %v", r.Method, r.Header)
			}
			return
		case <-timeout:
			t.Fatal("push did not arrive through the load balancer")
		}
	}
}

// TestRetriesAndTimeouts covers routeAction.retryPolicy and timeouts
// (FR-LB-008) and statusDetails in access logs (FR-LB-010).
func TestRetriesAndTimeouts(t *testing.T) {
	e := start(t)
	c := e.c
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/flaky":
			if calls.Add(1)%2 == 1 {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			_, _ = io.WriteString(w, "ok")
		case "/slow":
			time.Sleep(3 * time.Second)
		}
	}))
	defer srv.Close()
	_, ps, _ := net.SplitHostPort(srv.Listener.Addr().String())
	port, _ := strconv.Atoi(ps)
	e.do(c.Networks.Insert(proj, &computev1.Network{Name: "vpc", AutoCreateSubnetworks: false, ForceSendFields: []string{"AutoCreateSubnetworks"}}).Do())
	e.do(c.NetworkEndpointGroups.Insert(proj, "us-central1-a", &computev1.NetworkEndpointGroup{Name: "neg", Network: "global/networks/vpc", DefaultPort: int64(port)}).Do())
	e.do(c.NetworkEndpointGroups.AttachNetworkEndpoints(proj, "us-central1-a", "neg", &computev1.NetworkEndpointGroupsAttachEndpointsRequest{
		NetworkEndpoints: []*computev1.NetworkEndpoint{{IpAddress: "127.0.0.1"}}}).Do())
	e.do(c.BackendServices.Insert(proj, &computev1.BackendService{Name: "bs", LoadBalancingScheme: "EXTERNAL_MANAGED", TimeoutSec: 1,
		LogConfig: &computev1.BackendServiceLogConfig{Enable: true},
		Backends:  []*computev1.Backend{{Group: "zones/us-central1-a/networkEndpointGroups/neg"}}}).Do())
	e.do(c.UrlMaps.Insert(proj, &computev1.UrlMap{Name: "map", DefaultService: "global/backendServices/bs",
		HostRules: []*computev1.HostRule{{Hosts: []string{"*"}, PathMatcher: "m"}},
		PathMatchers: []*computev1.PathMatcher{{Name: "m", DefaultService: "global/backendServices/bs",
			RouteRules: []*computev1.HttpRouteRule{{Priority: 1, MatchRules: []*computev1.HttpRouteRuleMatch{{PrefixMatch: "/flaky"}},
				Service:     "global/backendServices/bs",
				RouteAction: &computev1.HttpRouteAction{RetryPolicy: &computev1.HttpRetryPolicy{RetryConditions: []string{"5xx"}, NumRetries: 2}}}}}}}).Do())
	e.do(c.TargetHttpProxies.Insert(proj, &computev1.TargetHttpProxy{Name: "p", UrlMap: "global/urlMaps/map"}).Do())
	e.do(c.GlobalForwardingRules.Insert(proj, &computev1.ForwardingRule{Name: "fr", LoadBalancingScheme: "EXTERNAL_MANAGED", PortRange: "8080", Target: "global/targetHttpProxies/p"}).Do())
	base := "http://" + e.inst.Endpoint("lb:fr")
	cl := &http.Client{Timeout: 10 * time.Second}
	if resp, body := get(t, cl, base+"/flaky"); resp.StatusCode != 200 || body != "ok" {
		t.Fatalf("retried request: %d %q", resp.StatusCode, body)
	}
	if resp, _ := get(t, cl, base+"/slow"); resp.StatusCode != http.StatusGatewayTimeout {
		t.Fatalf("timeout: %d", resp.StatusCode)
	}
}

// TestInstanceGroupBackend is FR-LB-005: a backend service with an
// instance group backend routes to the group's instances at the named
// port its portName resolves to.
func TestInstanceGroupBackend(t *testing.T) {
	e := start(t)
	c := e.c
	origin := newBackend(t)
	const group = "projects/" + proj + "/zones/us-central1-a/instanceGroups/pool-grp"

	bs := &computev1.BackendService{Name: "ig", LoadBalancingScheme: "EXTERNAL_MANAGED", Protocol: "HTTP", PortName: "web",
		Backends: []*computev1.Backend{{Group: "zones/us-central1-a/instanceGroups/pool-grp", BalancingMode: "UTILIZATION"}}}
	if _, err := c.BackendServices.Insert(proj, bs).Do(); err == nil {
		t.Fatal("backend service with an unknown instance group was accepted")
	} else if code, _ := apiErr(err); code != 404 {
		t.Fatalf("unknown instance group: %v", err)
	}

	var ports atomic.Pointer[map[string]int]
	ports.Store(&map[string]int{"web": int(origin.port())})
	svc, _ := e.inst.Env.Lookup("compute")
	svc.(*compute.Service).AddInstanceGroupResolver(func(_ context.Context, p string) (compute.InstanceGroup, bool) {
		if p != group {
			return compute.InstanceGroup{}, false
		}
		return compute.InstanceGroup{
			Instances:  []emu.NEGEndpoint{{IP: "127.0.0.1", Instance: "node-1"}},
			NamedPorts: *ports.Load(),
		}, true
	})
	e.do(c.BackendServices.Insert(proj, bs).Do())
	e.do(c.UrlMaps.Insert(proj, &computev1.UrlMap{Name: "map", DefaultService: "global/backendServices/ig"}).Do())
	e.do(c.TargetHttpProxies.Insert(proj, &computev1.TargetHttpProxy{Name: "p", UrlMap: "global/urlMaps/map"}).Do())
	e.do(c.GlobalForwardingRules.Insert(proj, &computev1.ForwardingRule{Name: "fr", LoadBalancingScheme: "EXTERNAL_MANAGED", PortRange: "8080", Target: "global/targetHttpProxies/p"}).Do())
	url := "http://" + e.inst.Endpoint("lb:fr") + "/hello"

	poll := func(want func(int, string) bool) (int, string) {
		t.Helper()
		var code int
		var body string
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
			resp, b := get(t, &http.Client{Timeout: 5 * time.Second}, url)
			code, body = resp.StatusCode, b
			if want(code, body) {
				break
			}
		}
		return code, body
	}
	if code, body := poll(func(c int, _ string) bool { return c == 200 }); code != 200 || !strings.HasSuffix(body, "/hello") {
		t.Fatalf("via instance group: %d %q", code, body)
	}

	// Without the named port the group has no endpoints.
	ports.Store(&map[string]int{"http": 80})
	if code, _ := poll(func(c int, _ string) bool { return c != 200 }); code == 200 {
		t.Fatal("still routed after the named port was removed")
	}
}

// TestHealthCheckTransitions is FR-LB-007 for every probe type: an
// endpoint that starts failing its health check goes UNHEALTHY in
// getHealth and stops receiving traffic, then recovers.
func TestHealthCheckTransitions(t *testing.T) {
	for _, kind := range []string{"HTTP", "HTTP2", "TCP", "GRPC"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			testHealthTransitions(t, kind)
		})
	}
}

// toggled is an endpoint whose health a test flips.
type toggled struct {
	port      int64
	setHealth func(bool)
}

// healthOrigin starts an origin for a health check kind answering
// requests with name. For TCP, unhealthy means the port is closed.
func healthOrigin(t *testing.T, kind, name string) toggled {
	t.Helper()
	var healthy atomic.Bool
	healthy.Store(true)
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" && !healthy.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, name)
	})
	portOf := func(addr net.Addr) int64 { return int64(addr.(*net.TCPAddr).Port) }
	switch kind {
	case "HTTP":
		s := httptest.NewServer(h)
		t.Cleanup(s.Close)
		return toggled{portOf(s.Listener.Addr()), healthy.Store}
	case "HTTP2":
		s := httptest.NewUnstartedServer(h)
		s.EnableHTTP2 = true
		s.StartTLS()
		t.Cleanup(s.Close)
		return toggled{portOf(s.Listener.Addr()), healthy.Store}
	case "TCP":
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := ln.Addr().String()
		var mu sync.Mutex
		srv := &http.Server{Handler: h}
		go srv.Serve(ln)
		t.Cleanup(func() { mu.Lock(); srv.Close(); mu.Unlock() })
		return toggled{portOf(ln.Addr()), func(up bool) {
			mu.Lock()
			defer mu.Unlock()
			srv.Close()
			if up {
				ln, err := net.Listen("tcp", addr)
				if err != nil {
					t.Errorf("relisten %s: %v", addr, err)
					return
				}
				srv = &http.Server{Handler: h}
				go srv.Serve(ln)
			}
		}}
	default: // GRPC
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		gs := grpc.NewServer()
		hs := health.NewServer()
		healthpb.RegisterHealthServer(gs, hs)
		hs.SetServingStatus("app", healthpb.HealthCheckResponse_SERVING)
		go gs.Serve(ln)
		t.Cleanup(gs.Stop)
		return toggled{portOf(ln.Addr()), func(up bool) {
			st := healthpb.HealthCheckResponse_NOT_SERVING
			if up {
				st = healthpb.HealthCheckResponse_SERVING
			}
			hs.SetServingStatus("app", st)
		}}
	}
}

func testHealthTransitions(t *testing.T, kind string) {
	e := start(t)
	c := e.c
	a := healthOrigin(t, kind, "A")
	eps := []*computev1.NetworkEndpoint{{IpAddress: "127.0.0.1", Port: a.port}}
	traffic := kind != "GRPC"
	if traffic {
		b := healthOrigin(t, kind, "B")
		eps = append(eps, &computev1.NetworkEndpoint{IpAddress: "127.0.0.1", Port: b.port})
	}
	hc := &computev1.HealthCheck{Name: "hc", Type: kind, CheckIntervalSec: 1, TimeoutSec: 1, HealthyThreshold: 2, UnhealthyThreshold: 2}
	switch kind {
	case "HTTP":
		hc.HttpHealthCheck = &computev1.HTTPHealthCheck{PortSpecification: "USE_SERVING_PORT", RequestPath: "/healthz"}
	case "HTTP2":
		hc.Http2HealthCheck = &computev1.HTTP2HealthCheck{PortSpecification: "USE_SERVING_PORT", RequestPath: "/healthz"}
	case "TCP":
		hc.TcpHealthCheck = &computev1.TCPHealthCheck{PortSpecification: "USE_SERVING_PORT"}
	case "GRPC":
		hc.GrpcHealthCheck = &computev1.GRPCHealthCheck{PortSpecification: "USE_SERVING_PORT", GrpcServiceName: "app"}
	}
	protocol := map[string]string{"HTTP": "HTTP", "HTTP2": "HTTP2", "TCP": "HTTP", "GRPC": "GRPC"}[kind]
	e.do(c.Networks.Insert(proj, &computev1.Network{Name: "vpc", AutoCreateSubnetworks: false, ForceSendFields: []string{"AutoCreateSubnetworks"}}).Do())
	e.do(c.NetworkEndpointGroups.Insert(proj, "us-central1-a", &computev1.NetworkEndpointGroup{Name: "neg", Network: "global/networks/vpc"}).Do())
	e.do(c.NetworkEndpointGroups.AttachNetworkEndpoints(proj, "us-central1-a", "neg", &computev1.NetworkEndpointGroupsAttachEndpointsRequest{NetworkEndpoints: eps}).Do())
	e.do(c.HealthChecks.Insert(proj, hc).Do())
	e.do(c.BackendServices.Insert(proj, &computev1.BackendService{Name: "svc", LoadBalancingScheme: "EXTERNAL_MANAGED", Protocol: protocol,
		HealthChecks: []string{"global/healthChecks/hc"}, Backends: []*computev1.Backend{{Group: "zones/us-central1-a/networkEndpointGroups/neg"}}}).Do())
	e.do(c.UrlMaps.Insert(proj, &computev1.UrlMap{Name: "map", DefaultService: "global/backendServices/svc"}).Do())
	e.do(c.TargetHttpProxies.Insert(proj, &computev1.TargetHttpProxy{Name: "p", UrlMap: "global/urlMaps/map"}).Do())
	e.do(c.GlobalForwardingRules.Insert(proj, &computev1.ForwardingRule{Name: "fr", LoadBalancingScheme: "EXTERNAL_MANAGED", PortRange: "8080", Target: "global/targetHttpProxies/p"}).Do())
	url := "http://" + e.inst.Endpoint("lb:fr") + "/"

	stateOfA := func() string {
		h, err := c.BackendServices.GetHealth(proj, "svc", &computev1.ResourceGroupReference{Group: "zones/us-central1-a/networkEndpointGroups/neg"}).Do()
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range h.HealthStatus {
			if s.Port == a.port {
				return s.HealthState
			}
		}
		return ""
	}
	waitState := func(want string) {
		t.Helper()
		deadline := time.Now().Add(15 * time.Second)
		for stateOfA() != want {
			if time.Now().After(deadline) {
				t.Fatalf("endpoint A is %s, want %s", stateOfA(), want)
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
	served := func() map[string]int {
		got := map[string]int{}
		for range 12 {
			resp, body := get(t, &http.Client{Timeout: 5 * time.Second}, url)
			got[fmt.Sprintf("%d %s", resp.StatusCode, body)]++
		}
		return got
	}

	waitState("HEALTHY")
	a.setHealth(false)
	waitState("UNHEALTHY")
	if traffic {
		if got := served(); got["200 B"] != 12 {
			t.Errorf("traffic with A unhealthy = %v, want only B", got)
		}
	}
	a.setHealth(true)
	waitState("HEALTHY")
	if traffic {
		if got := served(); got["200 A"] == 0 || got["200 B"] == 0 {
			t.Errorf("traffic after A recovered = %v, want both", got)
		}
	}
}
