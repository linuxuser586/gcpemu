package cdn

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
)

// hitSetup primes a cache with one 16 KiB static object.
func hitSetup(t testing.TB, dir string) (*Cache, Backend, http.Handler) {
	c, _ := newCache(t, dir, 0)
	b := svc(CacheAllStatic)
	o := &origin{serve: static(200, strings.Repeat("a", 16<<10), "Content-Type", "image/png", "Etag", `"x"`)}
	if _, st := do(c, b, o, get("http://h/asset.png?v=1")); st != StatusMiss {
		t.Fatalf("prime: %s", st)
	}
	return c, b, o
}

// TestHitLatencyP99 checks NFR-PERF-007: a cache hit adds at most 2 ms
// at p99 (measured for the whole Serve call, which is the latency the
// CDN adds in front of the LB's response path).
func TestHitLatencyP99(t *testing.T) {
	if testing.Short() || raceEnabled {
		t.Skip("latency assertion skipped in -short / -race")
	}
	for _, dir := range []string{"", t.TempDir()} {
		c, b, o := hitSetup(t, dir)
		const n = 5000
		lat := make([]time.Duration, 0, n)
		for range n {
			w := httptest.NewRecorder()
			r := get("http://h/asset.png?v=1", "Accept-Encoding", "identity")
			start := time.Now()
			st := c.Serve(w, r, b, o)
			lat = append(lat, time.Since(start))
			if st != StatusHit {
				t.Fatalf("status %s", st)
			}
		}
		slices.Sort(lat)
		p50, p99 := lat[n/2], lat[n*99/100]
		t.Logf("dir=%q hit latency p50=%v p99=%v", dir, p50, p99)
		if p99 > 2*time.Millisecond {
			t.Errorf("p99 hit latency %v > 2ms (NFR-PERF-007)", p99)
		}
	}
}

func BenchmarkHit(b *testing.B) {
	c, be, o := hitSetup(b, "")
	r := get("http://h/asset.png?v=1")
	b.ReportAllocs()
	b.ResetTimer()
	lat := make([]time.Duration, 0, b.N)
	for i := 0; i < b.N; i++ {
		w := httptest.NewRecorder()
		start := time.Now()
		c.Serve(w, r, be, o)
		lat = append(lat, time.Since(start))
	}
	b.StopTimer()
	slices.Sort(lat)
	b.ReportMetric(float64(lat[len(lat)*99/100].Microseconds()), "p99-µs")
}

func BenchmarkHitParallel(b *testing.B) {
	c, be, o := hitSetup(b, b.TempDir())
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			c.Serve(httptest.NewRecorder(), get("http://h/asset.png?v=1"), be, o)
		}
	})
}
