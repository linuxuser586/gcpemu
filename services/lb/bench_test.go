package lb_test

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"cloud.google.com/go/storage"
	computev1 "google.golang.org/api/compute/v1"
	"google.golang.org/api/option"

	"github.com/linuxuser586/gcpemu/emutest"
	"github.com/linuxuser586/gcpemu/services/lb"
)

// Performance targets (Section 8.1): NFR-PERF-007 cache-hit added latency
// p99 ≤ 2 ms; NFR-PERF-008 ≥ 5,000 req/s on cache misses to a local
// backend. The benchmarks report p99-ms and req/s; TestPerformanceTargets
// asserts them when GCPEMU_PERF_TESTS=1.

type perfEnv struct {
	*env
	url    string
	client *http.Client
}

// perfStack builds an HTTP load balancer on port 8080 routing /static/* to
// a CDN-enabled backend bucket and everything else to a NEG backend.
func perfStack(tb testing.TB) *perfEnv {
	t, ok := tb.(*testing.T)
	if !ok {
		t = &testing.T{}
	}
	inst := emutest.Start(tb, []string{"lb", "cdn"})
	svc, _ := inst.Env.Lookup("lb")
	svc.(*lb.Service).SetOptions(lb.Options{DirectDial: true})
	c, err := computev1.NewService(context.Background(), option.WithEndpoint(inst.GatewayURL()+"/compute/v1/"), option.WithoutAuthentication())
	if err != nil {
		tb.Fatal(err)
	}
	e := &env{t: t, inst: inst, c: c}
	do := func(op *computev1.Operation, err error) {
		tb.Helper()
		if err != nil || op.Error != nil {
			tb.Fatalf("%v %+v", err, op)
		}
	}
	origin := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	})}
	ol, _ := net.Listen("tcp", "127.0.0.1:0")
	go origin.Serve(ol)
	tb.Cleanup(func() { origin.Close() })
	_, ps, _ := net.SplitHostPort(ol.Addr().String())
	port := int64(atoi(ps))

	do(c.Networks.Insert(proj, &computev1.Network{Name: "vpc", AutoCreateSubnetworks: false, ForceSendFields: []string{"AutoCreateSubnetworks"}}).Do())
	do(c.NetworkEndpointGroups.Insert(proj, "us-central1-a", &computev1.NetworkEndpointGroup{Name: "neg", Network: "global/networks/vpc", DefaultPort: port}).Do())
	do(c.NetworkEndpointGroups.AttachNetworkEndpoints(proj, "us-central1-a", "neg", &computev1.NetworkEndpointGroupsAttachEndpointsRequest{
		NetworkEndpoints: []*computev1.NetworkEndpoint{{IpAddress: "127.0.0.1"}}}).Do())
	do(c.BackendServices.Insert(proj, &computev1.BackendService{Name: "api", LoadBalancingScheme: "EXTERNAL_MANAGED",
		Backends: []*computev1.Backend{{Group: "zones/us-central1-a/networkEndpointGroups/neg"}}}).Do())
	ctx := context.Background()
	sc, err := storage.NewClient(ctx, option.WithEndpoint("http://"+inst.Endpoint("gcs")+"/storage/v1/"), option.WithoutAuthentication())
	if err != nil {
		tb.Fatal(err)
	}
	if err := sc.Bucket("assets").Create(ctx, proj, nil); err != nil {
		tb.Fatal(err)
	}
	w := sc.Bucket("assets").Object("static/app.js").NewWriter(ctx)
	w.ContentType = "application/javascript"
	_, _ = w.Write(make([]byte, 4096))
	if err := w.Close(); err != nil {
		tb.Fatal(err)
	}
	do(c.BackendBuckets.Insert(proj, &computev1.BackendBucket{Name: "assets", BucketName: "assets", EnableCdn: true}).Do())
	do(c.UrlMaps.Insert(proj, &computev1.UrlMap{Name: "map", DefaultService: "global/backendServices/api",
		HostRules:    []*computev1.HostRule{{Hosts: []string{"*"}, PathMatcher: "m"}},
		PathMatchers: []*computev1.PathMatcher{{Name: "m", DefaultService: "global/backendServices/api", PathRules: []*computev1.PathRule{{Paths: []string{"/static/*"}, Service: "global/backendBuckets/assets"}}}},
	}).Do())
	do(c.TargetHttpProxies.Insert(proj, &computev1.TargetHttpProxy{Name: "p", UrlMap: "global/urlMaps/map"}).Do())
	do(c.GlobalForwardingRules.Insert(proj, &computev1.ForwardingRule{Name: "fr", LoadBalancingScheme: "EXTERNAL_MANAGED", PortRange: "8080", Target: "global/targetHttpProxies/p"}).Do())
	tr := &http.Transport{MaxIdleConnsPerHost: 256, MaxIdleConns: 256}
	tb.Cleanup(tr.CloseIdleConnections)
	return &perfEnv{env: e, url: "http://" + inst.Endpoint("lb:fr"), client: &http.Client{Transport: tr}}
}

func atoi(s string) int {
	n := 0
	for _, c := range s {
		n = n*10 + int(c-'0')
	}
	return n
}

func (p *perfEnv) get(tb testing.TB, path string) {
	resp, err := p.client.Get(p.url + path)
	if err != nil {
		tb.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		tb.Fatalf("%s: %d", path, resp.StatusCode)
	}
}

// cacheHitP99 measures n sequential cache hits and returns the p99.
func (p *perfEnv) cacheHitP99(tb testing.TB, n int) time.Duration {
	p.get(tb, "/static/app.js") // fill
	lat := make([]time.Duration, n)
	for i := range lat {
		t0 := time.Now()
		p.get(tb, "/static/app.js")
		lat[i] = time.Since(t0)
	}
	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	return lat[n*99/100]
}

// missThroughput runs workers issuing cache-miss requests for d.
func (p *perfEnv) missThroughput(tb testing.TB, workers int, d time.Duration) float64 {
	var n atomic.Int64
	stop := time.Now().Add(d)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for time.Now().Before(stop) {
				p.get(tb, "/api/items")
				n.Add(1)
			}
		}()
	}
	wg.Wait()
	return float64(n.Load()) / d.Seconds()
}

func BenchmarkCacheHit(b *testing.B) {
	p := perfStack(b)
	p.get(b, "/static/app.js")
	b.ResetTimer()
	var lat []time.Duration
	for b.Loop() {
		t0 := time.Now()
		p.get(b, "/static/app.js")
		lat = append(lat, time.Since(t0))
	}
	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	b.ReportMetric(float64(lat[len(lat)*99/100].Microseconds())/1000, "p99-ms")
}

func BenchmarkCacheMiss(b *testing.B) {
	p := perfStack(b)
	b.ResetTimer()
	t0 := time.Now()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			p.get(b, "/api/items")
		}
	})
	b.ReportMetric(float64(b.N)/time.Since(t0).Seconds(), "req/s")
}

func TestPerformanceTargets(t *testing.T) {
	if os.Getenv("GCPEMU_PERF_TESTS") != "1" {
		t.Skip("set GCPEMU_PERF_TESTS=1 to check NFR-PERF-007/008")
	}
	p := perfStack(t)
	if p99 := p.cacheHitP99(t, 2000); p99 > 2*time.Millisecond {
		t.Errorf("cache hit p99 = %v, want ≤ 2ms (NFR-PERF-007)", p99)
	} else {
		t.Logf("cache hit p99 = %v", p99)
	}
	if rps := p.missThroughput(t, 32, 3*time.Second); rps < 5000 {
		t.Errorf("cache-miss throughput = %.0f req/s, want ≥ 5000 (NFR-PERF-008)", rps)
	} else {
		t.Logf("cache-miss throughput = %.0f req/s", rps)
	}
}
