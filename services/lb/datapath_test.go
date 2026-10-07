package lb_test

import (
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	computev1 "google.golang.org/api/compute/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"

	"github.com/linuxuser586/gcpemu/internal/ca"
)

// Data-path tests of features the control-plane tests only configure
// (FR-LB-001/004/005/008/010, FR-CDN-001/007).

// named is an origin answering every request with its name.
func named(t *testing.T, name string, h http.HandlerFunc) int64 {
	t.Helper()
	if h == nil {
		h = func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			_, _ = io.WriteString(w, name)
		}
	}
	s := &http.Server{Handler: h}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go s.Serve(ln)
	t.Cleanup(func() { s.Close() })
	return int64(ln.Addr().(*net.TCPAddr).Port)
}

// neg creates network "vpc" and a zonal NEG "neg" with 127.0.0.1 endpoints.
func (e *env) neg(ports ...int64) {
	c := e.c
	e.do(c.Networks.Insert(proj, &computev1.Network{Name: "vpc", AutoCreateSubnetworks: false, ForceSendFields: []string{"AutoCreateSubnetworks"}}).Do())
	e.do(c.NetworkEndpointGroups.Insert(proj, "us-central1-a", &computev1.NetworkEndpointGroup{Name: "neg", Network: "global/networks/vpc"}).Do())
	var eps []*computev1.NetworkEndpoint
	for _, p := range ports {
		eps = append(eps, &computev1.NetworkEndpoint{IpAddress: "127.0.0.1", Port: p})
	}
	e.do(c.NetworkEndpointGroups.AttachNetworkEndpoints(proj, "us-central1-a", "neg", &computev1.NetworkEndpointGroupsAttachEndpointsRequest{NetworkEndpoints: eps}).Do())
}

// httpLB puts bs (backed by "neg") behind a global HTTP load balancer and
// returns its listener address.
func (e *env) httpLB(bs *computev1.BackendService) string {
	c := e.c
	bs.Backends = []*computev1.Backend{{Group: "zones/us-central1-a/networkEndpointGroups/neg"}}
	if bs.LoadBalancingScheme == "" {
		bs.LoadBalancingScheme = "EXTERNAL_MANAGED"
	}
	e.do(c.BackendServices.Insert(proj, bs).Do())
	e.do(c.UrlMaps.Insert(proj, &computev1.UrlMap{Name: "map", DefaultService: "global/backendServices/" + bs.Name}).Do())
	e.do(c.TargetHttpProxies.Insert(proj, &computev1.TargetHttpProxy{Name: "p", UrlMap: "global/urlMaps/map"}).Do())
	e.do(c.GlobalForwardingRules.Insert(proj, &computev1.ForwardingRule{Name: "fr", LoadBalancingScheme: bs.LoadBalancingScheme, PortRange: "8080", Target: "global/targetHttpProxies/p"}).Do())
	return e.inst.Endpoint("lb:fr")
}

// TestCertificateMapSNI is FR-LB-004: a target HTTPS proxy with a
// Certificate Manager certificate map picks the certificate by SNI, and
// the PRIMARY entry for other names.
func TestCertificateMapSNI(t *testing.T) {
	e := start(t)
	c := e.c
	loc := "projects/" + proj + "/locations/global"
	for id, host := range map[string]string{"a": "a.example.test", "b": "b.example.test", "p": "primary.example.test"} {
		leaf, err := e.inst.Env.CA.Issue(ca.Leaf{DNSNames: []string{host}})
		if err != nil {
			t.Fatal(err)
		}
		chain, key, _ := ca.EncodeCert(leaf)
		e.rest("POST", "/certificatemanager/v1/"+loc+"/certificates?certificateId="+id, map[string]any{
			"selfManaged": map[string]any{"pemCertificate": string(chain), "pemPrivateKey": string(key)}})
	}
	e.rest("POST", "/certificatemanager/v1/"+loc+"/certificateMaps?certificateMapId=m", map[string]any{})
	for id, ent := range map[string]map[string]any{
		"ea": {"hostname": "a.example.test", "certificates": []string{loc + "/certificates/a"}},
		"eb": {"hostname": "b.example.test", "certificates": []string{loc + "/certificates/b"}},
		"ep": {"matcher": "PRIMARY", "certificates": []string{loc + "/certificates/p"}},
	} {
		e.rest("POST", "/certificatemanager/v1/"+loc+"/certificateMaps/m/certificateMapEntries?certificateMapEntryId="+id, ent)
	}
	e.neg(named(t, "origin", nil))
	e.do(c.BackendServices.Insert(proj, &computev1.BackendService{Name: "svc", LoadBalancingScheme: "EXTERNAL_MANAGED",
		Backends: []*computev1.Backend{{Group: "zones/us-central1-a/networkEndpointGroups/neg"}}}).Do())
	e.do(c.UrlMaps.Insert(proj, &computev1.UrlMap{Name: "map", DefaultService: "global/backendServices/svc"}).Do())
	e.do(c.TargetHttpsProxies.Insert(proj, &computev1.TargetHttpsProxy{Name: "https", UrlMap: "global/urlMaps/map",
		CertificateMap: "//certificatemanager.googleapis.com/" + loc + "/certificateMaps/m"}).Do())
	e.do(c.GlobalForwardingRules.Insert(proj, &computev1.ForwardingRule{Name: "fr", LoadBalancingScheme: "EXTERNAL_MANAGED", PortRange: "443", Target: "global/targetHttpsProxies/https"}).Do())
	listener := e.inst.Endpoint("lb:fr")

	for sni, want := range map[string]string{"a.example.test": "a.example.test", "b.example.test": "b.example.test", "other.test": "primary.example.test"} {
		conn, err := tls.Dial("tcp", listener, &tls.Config{ServerName: sni, RootCAs: e.inst.Env.CA.Pool(), InsecureSkipVerify: sni == "other.test"})
		if err != nil {
			t.Fatalf("SNI %s: %v", sni, err)
		}
		got := conn.ConnectionState().PeerCertificates[0].DNSNames
		conn.Close()
		if len(got) != 1 || got[0] != want {
			t.Errorf("SNI %s served %v, want %s", sni, got, want)
		}
	}
	resp, body := get(t, e.client(listener), "https://b.example.test/")
	if resp.StatusCode != 200 || body != "origin" {
		t.Fatalf("request: %d %q", resp.StatusCode, body)
	}
}

// TestRegionalLoadBalancers is FR-LB-001: regional external and internal
// Application Load Balancers carry traffic, and their access logs use the
// regional resource types.
func TestRegionalLoadBalancers(t *testing.T) {
	e := start(t)
	c := e.c
	e.neg(named(t, "origin", nil))
	e.do(c.Subnetworks.Insert(proj, "us-central1", &computev1.Subnetwork{Name: "sub", Network: "global/networks/vpc", IpCidrRange: "10.40.0.0/24"}).Do())
	for _, scheme := range []string{"EXTERNAL_MANAGED", "INTERNAL_MANAGED"} {
		n := strings.ToLower(strings.Split(scheme, "_")[0])
		e.do(c.RegionBackendServices.Insert(proj, "us-central1", &computev1.BackendService{Name: n, LoadBalancingScheme: scheme, Protocol: "HTTP",
			LogConfig: &computev1.BackendServiceLogConfig{Enable: true},
			Backends:  []*computev1.Backend{{Group: "zones/us-central1-a/networkEndpointGroups/neg", BalancingMode: "RATE", MaxRatePerEndpoint: 100}}}).Do())
		e.do(c.RegionUrlMaps.Insert(proj, "us-central1", &computev1.UrlMap{Name: n, DefaultService: "regions/us-central1/backendServices/" + n}).Do())
		e.do(c.RegionTargetHttpProxies.Insert(proj, "us-central1", &computev1.TargetHttpProxy{Name: n, UrlMap: "regions/us-central1/urlMaps/" + n}).Do())
		fr := &computev1.ForwardingRule{Name: n + "-fr", LoadBalancingScheme: scheme, PortRange: "8080", Target: "regions/us-central1/targetHttpProxies/" + n}
		if scheme == "INTERNAL_MANAGED" {
			fr.Network, fr.Subnetwork = "global/networks/vpc", "regions/us-central1/subnetworks/sub"
		}
		e.do(c.ForwardingRules.Insert(proj, "us-central1", fr).Do())
		listener := e.inst.Endpoint("lb:" + fr.Name)
		if listener == "" {
			t.Fatalf("%s: no listener", scheme)
		}
		resp, body := get(t, &http.Client{Timeout: 5 * time.Second}, "http://"+listener+"/x")
		if resp.StatusCode != 200 || body != "origin" {
			t.Fatalf("%s: %d %q", scheme, resp.StatusCode, body)
		}
	}
	want := map[string]string{"external-fr": "http_external_regional_lb_rule", "internal-fr": "internal_http_lb_rule"}
	got := map[string]string{}
	for _, l := range accessLog(t, e, 2) {
		got[l.Resource.Labels["forwarding_rule_name"]] = l.Resource.Type
	}
	for fr, typ := range want {
		if got[fr] != typ {
			t.Errorf("access log resource type for %s = %q, want %q (all: %v)", fr, got[fr], typ, got)
		}
	}
}

// logLine is the part of an access log entry the tests check.
type logLine struct {
	Resource struct {
		Type   string            `json:"type"`
		Labels map[string]string `json:"labels"`
	} `json:"resource"`
	HTTPRequest map[string]any `json:"httpRequest"`
	JSONPayload map[string]any `json:"jsonPayload"`
}

// accessLog waits for at least n entries in <data-dir>/lb/requests.log.
func accessLog(t *testing.T, e *env, n int) []logLine {
	t.Helper()
	dir, err := e.inst.Env.ServiceDir("lb")
	if err != nil {
		t.Fatal(err)
	}
	var out []logLine
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		out = out[:0]
		f, err := os.Open(filepath.Join(dir, "requests.log"))
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			var l logLine
			if json.Unmarshal(sc.Bytes(), &l) == nil {
				out = append(out, l)
			}
		}
		f.Close()
		if len(out) >= n {
			return out
		}
	}
	t.Fatalf("access log has %d entries, want %d", len(out), n)
	return nil
}

// TestAccessLogFields is FR-LB-010: entries carry Cloud Logging's
// httpRequest, statusDetails and backend latency.
func TestAccessLogFields(t *testing.T) {
	e := start(t)
	e.neg(named(t, "origin", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(20 * time.Millisecond)
		w.WriteHeader(http.StatusTeapot)
	}))
	listener := e.httpLB(&computev1.BackendService{Name: "svc", Protocol: "HTTP", LogConfig: &computev1.BackendServiceLogConfig{Enable: true}})
	req, _ := http.NewRequest("POST", "http://"+listener+"/path?q=1", strings.NewReader("body"))
	req.Host = "logs.example.test"
	req.Header.Set("User-Agent", "log-test/1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	l := accessLog(t, e, 1)[0]
	h := l.HTTPRequest
	if h["requestMethod"] != "POST" || h["requestUrl"] != "http://logs.example.test/path?q=1" || h["status"] != float64(418) ||
		h["userAgent"] != "log-test/1" || h["requestSize"] != "4" || h["remoteIp"] == "" || !strings.HasSuffix(fmt.Sprint(h["latency"]), "s") {
		t.Errorf("httpRequest = %v", h)
	}
	p := l.JSONPayload
	bl, _ := strconv.ParseFloat(strings.TrimSuffix(fmt.Sprint(p["backendLatency"]), "s"), 64)
	if p["statusDetails"] != "response_sent_by_backend" || bl < 0.02 || p["@type"] != "type.googleapis.com/google.cloud.loadbalancing.type.LoadBalancerLogEntry" {
		t.Errorf("jsonPayload = %v", p)
	}
	if l.Resource.Type != "http_load_balancer" || l.Resource.Labels["backend_service_name"] != "svc" || l.Resource.Labels["url_map_name"] != "map" {
		t.Errorf("resource = %+v", l.Resource)
	}
}

// TestGRPCThroughLB is FR-LB-005: gRPC from a client over TLS (HTTP/2) is
// passed through to an H2C backend, status codes and all.
func TestGRPCThroughLB(t *testing.T) {
	e := start(t)
	c := e.c
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	gs := grpc.NewServer()
	hs := health.NewServer()
	hs.SetServingStatus("app", healthpb.HealthCheckResponse_SERVING)
	healthpb.RegisterHealthServer(gs, hs)
	go gs.Serve(ln)
	t.Cleanup(gs.Stop)
	e.neg(int64(ln.Addr().(*net.TCPAddr).Port))
	leaf, _ := e.inst.Env.CA.Issue(ca.Leaf{DNSNames: []string{"grpc.example.test"}})
	chain, key, _ := ca.EncodeCert(leaf)
	e.do(c.SslCertificates.Insert(proj, &computev1.SslCertificate{Name: "cert", Certificate: string(chain), PrivateKey: string(key)}).Do())
	e.do(c.BackendServices.Insert(proj, &computev1.BackendService{Name: "svc", LoadBalancingScheme: "EXTERNAL_MANAGED", Protocol: "H2C",
		Backends: []*computev1.Backend{{Group: "zones/us-central1-a/networkEndpointGroups/neg"}}}).Do())
	e.do(c.UrlMaps.Insert(proj, &computev1.UrlMap{Name: "map", DefaultService: "global/backendServices/svc"}).Do())
	e.do(c.TargetHttpsProxies.Insert(proj, &computev1.TargetHttpsProxy{Name: "https", UrlMap: "global/urlMaps/map", SslCertificates: []string{"global/sslCertificates/cert"}}).Do())
	e.do(c.GlobalForwardingRules.Insert(proj, &computev1.ForwardingRule{Name: "fr", LoadBalancingScheme: "EXTERNAL_MANAGED", PortRange: "443", Target: "global/targetHttpsProxies/https"}).Do())

	conn, err := grpc.NewClient("passthrough:///"+e.inst.Endpoint("lb:fr"),
		grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{ServerName: "grpc.example.test", RootCAs: e.inst.Env.CA.Pool()})))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	hc := healthpb.NewHealthClient(conn)
	if r, err := hc.Check(ctx, &healthpb.HealthCheckRequest{Service: "app"}); err != nil || r.Status != healthpb.HealthCheckResponse_SERVING {
		t.Fatalf("Check(app) = %v, %v", r, err)
	}
	if _, err := hc.Check(ctx, &healthpb.HealthCheckRequest{Service: "nope"}); status.Code(err) != codes.NotFound {
		t.Fatalf("Check(nope) = %v, want NotFound", err)
	}
	// Server streaming through the proxy.
	w, err := hc.Watch(ctx, &healthpb.HealthCheckRequest{Service: "app"})
	if err != nil {
		t.Fatal(err)
	}
	if r, err := w.Recv(); err != nil || r.Status != healthpb.HealthCheckResponse_SERVING {
		t.Fatalf("Watch = %v, %v", r, err)
	}
	hs.SetServingStatus("app", healthpb.HealthCheckResponse_NOT_SERVING)
	if r, err := w.Recv(); err != nil || r.Status != healthpb.HealthCheckResponse_NOT_SERVING {
		t.Fatalf("Watch update = %v, %v", r, err)
	}
}

// TestLeastRequest is FR-LB-008: localityLbPolicy LEAST_REQUEST is
// power-of-two-choices on in-flight requests, so an endpoint that holds
// every request it gets receives far fewer than round robin's share. With
// one stuck endpoint of four, round robin sends it ~25% of requests and
// least-request ~6% (both random picks must land on it).
func TestLeastRequest(t *testing.T) {
	e := start(t)
	release := make(chan struct{})
	var stuck atomic.Int64
	slowPort := named(t, "slow", func(w http.ResponseWriter, r *http.Request) {
		stuck.Add(1)
		<-release
	})
	e.neg(slowPort, named(t, "fast1", nil), named(t, "fast2", nil), named(t, "fast3", nil))
	listener := e.httpLB(&computev1.BackendService{Name: "svc", Protocol: "HTTP", LocalityLbPolicy: "LEAST_REQUEST", TimeoutSec: 30})
	cl := &http.Client{Timeout: 30 * time.Second}
	const n = 100
	fast := make(chan struct{}, n)
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := cl.Get("http://" + listener + "/")
			if err != nil {
				return
			}
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if strings.HasPrefix(string(b), "fast") {
				fast <- struct{}{}
			}
		}()
		time.Sleep(2 * time.Millisecond)
	}
	deadline := time.After(20 * time.Second)
	for got := 0; int64(got)+stuck.Load() < n; got++ {
		select {
		case <-fast:
		case <-deadline:
			t.Fatalf("only %d fast responses and %d stuck requests", got, stuck.Load())
		}
	}
	close(release)
	wg.Wait()
	if s := stuck.Load(); s > 15 {
		t.Errorf("the stuck endpoint received %d of %d requests (round robin would give ~25, least-request ~6)", s, n)
	}
}

// TestCookieAffinity is FR-LB-008: GENERATED_COOKIE and HTTP_COOKIE pin a
// client to one endpoint.
func TestCookieAffinity(t *testing.T) {
	for _, tc := range []struct {
		affinity, cookie string
	}{{"GENERATED_COOKIE", "GCLB"}, {"HTTP_COOKIE", "sticky"}} {
		t.Run(tc.affinity, func(t *testing.T) {
			t.Parallel()
			e := start(t)
			e.neg(named(t, "A", nil), named(t, "B", nil), named(t, "C", nil))
			bs := &computev1.BackendService{Name: "svc", Protocol: "HTTP", SessionAffinity: tc.affinity, AffinityCookieTtlSec: 60}
			if tc.affinity == "HTTP_COOKIE" {
				bs.LocalityLbPolicy = "RING_HASH"
				bs.ConsistentHash = &computev1.ConsistentHashLoadBalancerSettings{HttpCookie: &computev1.ConsistentHashLoadBalancerSettingsHttpCookie{
					Name: "sticky", Ttl: &computev1.Duration{Seconds: 60}}}
			}
			url := "http://" + e.httpLB(bs) + "/"
			cl := &http.Client{Timeout: 5 * time.Second}
			resp, first := get(t, cl, url)
			var ck *http.Cookie
			for _, c := range resp.Cookies() {
				if c.Name == tc.cookie {
					ck = c
				}
			}
			if ck == nil || ck.Value == "" || ck.MaxAge <= 0 && ck.Expires.IsZero() {
				t.Fatalf("no %s cookie: %v", tc.cookie, resp.Header.Values("Set-Cookie"))
			}
			for range 15 {
				req, _ := http.NewRequest("GET", url, nil)
				req.AddCookie(&http.Cookie{Name: ck.Name, Value: ck.Value})
				resp, err := cl.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				b, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				if string(b) != first {
					t.Fatalf("with cookie went to %s, first request to %s", b, first)
				}
			}
			// Without the cookie requests spread over the endpoints.
			seen := map[string]bool{}
			for range 30 {
				_, b := get(t, cl, url)
				seen[b] = true
			}
			if len(seen) < 2 {
				t.Errorf("without cookies only %v served", seen)
			}
		})
	}
}

// TestCDNOnBackendService is FR-CDN-001 for backend services: enableCdn
// caches origin responses in front of a NEG backend.
func TestCDNOnBackendService(t *testing.T) {
	e := start(t)
	var hits int
	var mu sync.Mutex
	e.neg(named(t, "origin", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		w.Header().Set("Content-Type", "text/css")
		w.Header().Set("Cache-Control", "public, max-age=60")
		_, _ = io.WriteString(w, "body{}")
	}))
	listener := e.httpLB(&computev1.BackendService{Name: "svc", Protocol: "HTTP", EnableCDN: true,
		CdnPolicy:             &computev1.BackendServiceCdnPolicy{CacheMode: "CACHE_ALL_STATIC"},
		CustomResponseHeaders: []string{"X-Cache-Status:{cdn_cache_status}"}})
	cl := &http.Client{Timeout: 5 * time.Second}
	var statuses []string
	for range 3 {
		resp, body := get(t, cl, "http://"+listener+"/app.css")
		if resp.StatusCode != 200 || body != "body{}" {
			t.Fatalf("GET: %d %q", resp.StatusCode, body)
		}
		statuses = append(statuses, resp.Header.Get("X-Cache-Status"))
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(statuses, ",") != "miss,hit,hit" || hits != 1 {
		t.Errorf("cache statuses %v, origin hits %d", statuses, hits)
	}
}

// TestSignedURLKeys is FR-CDN-007 through the compute API: keys added with
// addSignedUrlKey validate signed URLs, are listed by name only, and stop
// applying once deleted.
func TestSignedURLKeys(t *testing.T) {
	e := start(t)
	c := e.c
	e.neg(named(t, "origin", nil))
	listener := e.httpLB(&computev1.BackendService{Name: "svc", Protocol: "HTTP", EnableCDN: true,
		CdnPolicy: &computev1.BackendServiceCdnPolicy{CacheMode: "USE_ORIGIN_HEADERS"}})
	key := make([]byte, 16)
	_, _ = rand.Read(key)
	e.do(c.BackendServices.AddSignedUrlKey(proj, "svc", &computev1.SignedUrlKey{KeyName: "k1", KeyValue: base64.URLEncoding.EncodeToString(key)}).Do())
	bs, err := c.BackendServices.Get(proj, "svc").Do()
	if err != nil || bs.CdnPolicy == nil || len(bs.CdnPolicy.SignedUrlKeyNames) != 1 || bs.CdnPolicy.SignedUrlKeyNames[0] != "k1" {
		t.Fatalf("signedUrlKeyNames = %+v, %v", bs.CdnPolicy, err)
	}
	if _, err := c.BackendServices.AddSignedUrlKey(proj, "svc", &computev1.SignedUrlKey{KeyName: "bad", KeyValue: "short"}).Do(); err == nil {
		t.Error("invalid key value accepted")
	}

	sign := func(u string, k []byte, exp time.Time) string {
		s := u + "?Expires=" + strconv.FormatInt(exp.Unix(), 10) + "&KeyName=k1"
		m := hmac.New(sha1.New, k)
		m.Write([]byte(s))
		return s + "&Signature=" + base64.URLEncoding.EncodeToString(m.Sum(nil))
	}
	base := "http://signed.example.test:8080/file"
	fetch := func(u string) int {
		t.Helper()
		req, _ := http.NewRequest("GET", strings.Replace(u, "signed.example.test:8080", listener, 1), nil)
		req.Host = "signed.example.test:8080"
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	wrong := make([]byte, 16)
	if code := fetch(sign(base, key, time.Now().Add(time.Hour))); code != 200 {
		t.Errorf("signed URL: %d", code)
	}
	if code := fetch(sign(base, wrong, time.Now().Add(time.Hour))); code != 403 {
		t.Errorf("wrong key: %d", code)
	}
	if code := fetch(sign(base, key, time.Now().Add(-time.Minute))); code != 403 {
		t.Errorf("expired: %d", code)
	}
	e.do(c.BackendServices.DeleteSignedUrlKey(proj, "svc", "k1").Do())
	if bs, _ := c.BackendServices.Get(proj, "svc").Do(); bs.CdnPolicy != nil && len(bs.CdnPolicy.SignedUrlKeyNames) != 0 {
		t.Errorf("after delete: %v", bs.CdnPolicy.SignedUrlKeyNames)
	}
	// Without keys the backend no longer validates signatures.
	if code := fetch(sign(base, wrong, time.Now().Add(time.Hour))); code != 200 {
		t.Errorf("after key deletion: %d", code)
	}
}
