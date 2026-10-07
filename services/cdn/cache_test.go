package cdn

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	compute "google.golang.org/api/compute/v1"
)

// testClock is a settable clock.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func newTestClock() *testClock {
	return &testClock{t: time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)}
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// origin is a scriptable test origin that counts requests.
type origin struct {
	mu    sync.Mutex
	hits  int
	reqs  []*http.Request
	serve func(w http.ResponseWriter, r *http.Request)
}

func (o *origin) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	o.mu.Lock()
	o.hits++
	o.reqs = append(o.reqs, r)
	f := o.serve
	o.mu.Unlock()
	f(w, r)
}

func (o *origin) count() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.hits
}

func (o *origin) last() *http.Request {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.reqs[len(o.reqs)-1]
}

// static returns a handler answering with fixed status, headers and body.
func static(status int, body string, hdr ...string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		for i := 0; i+1 < len(hdr); i += 2 {
			w.Header().Add(hdr[i], hdr[i+1])
		}
		w.WriteHeader(status)
		w.Write([]byte(body))
	}
}

func newCache(t testing.TB, dir string, limit int64) (*Cache, *testClock) {
	t.Helper()
	clk := newTestClock()
	c, err := NewCache(Options{Dir: dir, MaxBytes: limit, Clock: clk})
	if err != nil {
		t.Fatal(err)
	}
	return c, clk
}

func svc(mode string) Backend {
	return Backend{ID: "bs1", Kind: "backendService", ServicePolicy: &compute.BackendServiceCdnPolicy{CacheMode: mode, RequestCoalescing: true}}
}

func get(target string, hdr ...string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, target, nil)
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Add(hdr[i], hdr[i+1])
	}
	return r
}

func do(c *Cache, b Backend, o http.Handler, r *http.Request) (*httptest.ResponseRecorder, string) {
	w := httptest.NewRecorder()
	st := c.Serve(w, r, b, o)
	return w, st
}

func TestCacheability(t *testing.T) {
	cases := []struct {
		name    string
		mode    string
		mutate  func(*Backend)
		method  string
		reqHdr  []string
		status  int
		hdr     []string
		want    [2]string
		cc      string // expected client Cache-Control on the second response ("" = don't check)
		origins int
	}{
		{name: "static image default ttl", mode: CacheAllStatic, status: 200, hdr: []string{"Content-Type", "image/png"},
			want: [2]string{"miss", "hit"}, cc: "public, max-age=3600", origins: 1},
		{name: "static css", mode: CacheAllStatic, status: 200, hdr: []string{"Content-Type", "text/css; charset=utf-8"}, want: [2]string{"miss", "hit"}, origins: 1},
		{name: "static font", mode: CacheAllStatic, status: 200, hdr: []string{"Content-Type", "font/woff2"}, want: [2]string{"miss", "hit"}, origins: 1},
		{name: "static pdf", mode: "", status: 200, hdr: []string{"Content-Type", "application/pdf"}, want: [2]string{"miss", "hit"}, origins: 1},
		{name: "html not static", mode: CacheAllStatic, status: 200, hdr: []string{"Content-Type", "text/html"},
			want: [2]string{"uncacheable", "uncacheable"}, origins: 2},
		{name: "json not static", mode: CacheAllStatic, status: 200, hdr: []string{"Content-Type", "application/json"},
			want: [2]string{"uncacheable", "uncacheable"}, origins: 2},
		{name: "html with max-age", mode: CacheAllStatic, status: 200, hdr: []string{"Content-Type", "text/html", "Cache-Control", "max-age=60"},
			want: [2]string{"miss", "hit"}, cc: "max-age=60", origins: 1},
		{name: "client ttl clamps max-age", mode: CacheAllStatic, status: 200, hdr: []string{"Content-Type", "text/html", "Cache-Control", "public, max-age=7200"},
			want: [2]string{"miss", "hit"}, cc: "public, max-age=3600", origins: 1},
		{name: "expires future", mode: CacheAllStatic, status: 200,
			hdr:  []string{"Content-Type", "text/html", "Date", "Fri, 01 May 2026 12:00:00 GMT", "Expires", "Fri, 01 May 2026 12:10:00 GMT"},
			want: [2]string{"miss", "hit"}, cc: "public, max-age=600", origins: 1},
		{name: "private static", mode: CacheAllStatic, status: 200, hdr: []string{"Content-Type", "image/png", "Cache-Control", "private, max-age=60"},
			want: [2]string{"uncacheable", "uncacheable"}, origins: 2},
		{name: "no-store static", mode: CacheAllStatic, status: 200, hdr: []string{"Content-Type", "image/png", "Cache-Control", "no-store"},
			want: [2]string{"uncacheable", "uncacheable"}, origins: 2},
		{name: "set-cookie static", mode: CacheAllStatic, status: 200, hdr: []string{"Content-Type", "image/png", "Set-Cookie", "a=b"},
			want: [2]string{"uncacheable", "uncacheable"}, origins: 2},
		{name: "set-cookie force", mode: ForceCacheAll, status: 200, hdr: []string{"Content-Type", "image/png", "Set-Cookie", "a=b"},
			want: [2]string{"uncacheable", "uncacheable"}, origins: 2},
		{name: "vary user-agent", mode: CacheAllStatic, status: 200, hdr: []string{"Content-Type", "image/png", "Vary", "User-Agent"},
			want: [2]string{"uncacheable", "uncacheable"}, origins: 2},
		{name: "vary star", mode: ForceCacheAll, status: 200, hdr: []string{"Content-Type", "image/png", "Vary", "*"},
			want: [2]string{"uncacheable", "uncacheable"}, origins: 2},
		{name: "vary accept-encoding", mode: CacheAllStatic, status: 200, hdr: []string{"Content-Type", "image/png", "Vary", "Accept-Encoding, Origin"},
			want: [2]string{"miss", "hit"}, origins: 1},
		{name: "vary key header", mode: CacheAllStatic, mutate: func(b *Backend) {
			b.ServicePolicy.CacheKeyPolicy = &compute.CacheKeyPolicy{IncludeHost: true, IncludeProtocol: true, IncludeQueryString: true, IncludeHttpHeaders: []string{"X-Tenant"}}
		}, status: 200, hdr: []string{"Content-Type", "image/png", "Vary", "x-tenant"}, want: [2]string{"miss", "hit"}, origins: 1},
		{name: "authorization not public", mode: CacheAllStatic, reqHdr: []string{"Authorization", "Bearer x"}, status: 200,
			hdr: []string{"Content-Type", "image/png", "Cache-Control", "max-age=60"}, want: [2]string{"uncacheable", "uncacheable"}, origins: 2},
		{name: "authorization public", mode: CacheAllStatic, reqHdr: []string{"Authorization", "Bearer x"}, status: 200,
			hdr: []string{"Content-Type", "image/png", "Cache-Control", "public, max-age=60"}, want: [2]string{"miss", "hit"}, origins: 1},
		{name: "authorization s-maxage", mode: UseOriginHeaders, reqHdr: []string{"Authorization", "Bearer x"}, status: 200,
			hdr: []string{"Content-Type", "text/html", "Cache-Control", "s-maxage=60"}, want: [2]string{"miss", "hit"}, origins: 1},
		{name: "authorization force", mode: ForceCacheAll, reqHdr: []string{"Authorization", "Bearer x"}, status: 200,
			hdr: []string{"Content-Type", "text/html"}, want: [2]string{"miss", "hit"}, origins: 1},
		{name: "origin headers none", mode: UseOriginHeaders, status: 200, hdr: []string{"Content-Type", "image/png"},
			want: [2]string{"uncacheable", "uncacheable"}, origins: 2},
		{name: "origin headers max-age keeps cc", mode: UseOriginHeaders, status: 200, hdr: []string{"Content-Type", "text/html", "Cache-Control", "max-age=7200"},
			want: [2]string{"miss", "hit"}, cc: "max-age=7200", origins: 1},
		{name: "cdn-cache-control wins", mode: UseOriginHeaders, status: 200, hdr: []string{"Content-Type", "text/html", "Cache-Control", "no-store", "CDN-Cache-Control", "max-age=60"},
			want: [2]string{"miss", "hit"}, origins: 1},
		{name: "force private html", mode: ForceCacheAll, status: 200, hdr: []string{"Content-Type", "text/html", "Cache-Control", "private, no-store"},
			want: [2]string{"miss", "hit"}, cc: "public, max-age=3600", origins: 1},
		{name: "force 404 without negative caching", mode: ForceCacheAll, status: 404, hdr: []string{"Content-Type", "text/html"},
			want: [2]string{"uncacheable", "uncacheable"}, origins: 2},
		{name: "500 never", mode: ForceCacheAll, status: 500, want: [2]string{"uncacheable", "uncacheable"}, origins: 2},
		{name: "404 with explicit max-age", mode: CacheAllStatic, status: 404, hdr: []string{"Cache-Control", "max-age=30"},
			want: [2]string{"miss", "hit"}, origins: 1},
		{name: "static 404 without headers", mode: CacheAllStatic, status: 404, hdr: []string{"Content-Type", "image/png"},
			want: [2]string{"uncacheable", "uncacheable"}, origins: 2},
		{name: "post", mode: ForceCacheAll, method: http.MethodPost, status: 200, want: [2]string{"uncacheable", "uncacheable"}, origins: 2},
		{name: "request no-store", mode: ForceCacheAll, reqHdr: []string{"Cache-Control", "no-store"}, status: 200,
			want: [2]string{"uncacheable", "uncacheable"}, origins: 2},
		{name: "bypass header", mode: ForceCacheAll, mutate: func(b *Backend) {
			b.ServicePolicy.BypassCacheOnRequestHeaders = []*compute.BackendServiceCdnPolicyBypassCacheOnRequestHeader{{HeaderName: "X-Bypass"}}
		}, reqHdr: []string{"X-Bypass", "1"}, status: 200, want: [2]string{"uncacheable", "uncacheable"}, origins: 2},
		{name: "ttl 0 without validators", mode: ForceCacheAll, mutate: func(b *Backend) {
			b.ServicePolicy.DefaultTtl = 0
			b.ServicePolicy.ForceSendFields = []string{"DefaultTtl"}
		}, status: 200, want: [2]string{"uncacheable", "uncacheable"}, origins: 2},
		{name: "backend bucket", mode: "", mutate: func(b *Backend) {
			b.Kind, b.ServicePolicy = "backendBucket", nil
			b.BucketPolicy = &compute.BackendBucketCdnPolicy{CacheMode: CacheAllStatic}
		}, status: 200, hdr: []string{"Content-Type", "image/jpeg"}, want: [2]string{"miss", "hit"}, origins: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := newCache(t, "", 0)
			b := svc(tc.mode)
			if tc.mutate != nil {
				tc.mutate(&b)
			}
			o := &origin{serve: static(tc.status, "body", tc.hdr...)}
			var got [2]string
			var w *httptest.ResponseRecorder
			for i := range 2 {
				method := tc.method
				if method == "" {
					method = http.MethodGet
				}
				r := httptest.NewRequest(method, "http://example.com/a/b.png", nil)
				for j := 0; j+1 < len(tc.reqHdr); j += 2 {
					r.Header.Set(tc.reqHdr[j], tc.reqHdr[j+1])
				}
				w, got[i] = do(c, b, o, r)
				if w.Code != tc.status {
					t.Fatalf("request %d: status %d, want %d", i, w.Code, tc.status)
				}
				if w.Body.String() != "body" {
					t.Fatalf("request %d: body %q", i, w.Body.String())
				}
			}
			if got != tc.want {
				t.Errorf("statuses %v, want %v", got, tc.want)
			}
			if n := o.count(); n != tc.origins {
				t.Errorf("origin requests %d, want %d", n, tc.origins)
			}
			if tc.cc != "" {
				if cc := w.Header().Get("Cache-Control"); cc != tc.cc {
					t.Errorf("Cache-Control %q, want %q", cc, tc.cc)
				}
				if w.Header().Get("Expires") != "" {
					t.Errorf("Expires not removed")
				}
			}
			if tc.want[1] == "hit" && w.Header().Get("Age") == "" {
				t.Errorf("hit without Age")
			}
		})
	}
}

func TestTTLAndAge(t *testing.T) {
	c, clk := newCache(t, "", 0)
	o := &origin{serve: static(200, "x", "Content-Type", "text/plain", "Cache-Control", "max-age=10", "Age", "3")}
	b := svc(UseOriginHeaders)
	if _, st := do(c, b, o, get("http://h/a")); st != StatusMiss {
		t.Fatalf("first %s", st)
	}
	clk.Advance(4 * time.Second)
	w, st := do(c, b, o, get("http://h/a"))
	if st != StatusHit || w.Header().Get("Age") != "7" {
		t.Fatalf("second %s age %q", st, w.Header().Get("Age"))
	}
	clk.Advance(4 * time.Second) // age 11 > 10
	if _, st := do(c, b, o, get("http://h/a")); st != StatusMiss {
		t.Fatalf("expired: %s", st)
	}
	if o.count() != 2 {
		t.Fatalf("origin %d", o.count())
	}
}

func TestMaxTTLAndDefaultTTL(t *testing.T) {
	c, clk := newCache(t, "", 0)
	b := svc(CacheAllStatic)
	b.ServicePolicy.MaxTtl, b.ServicePolicy.DefaultTtl, b.ServicePolicy.ClientTtl = 100, 20, 5
	long := &origin{serve: static(200, "x", "Content-Type", "text/html", "Cache-Control", "max-age=100000")}
	img := &origin{serve: static(200, "x", "Content-Type", "image/gif")}
	do(c, b, long, get("http://h/long"))
	w, _ := do(c, b, img, get("http://h/img"))
	if cc := w.Header().Get("Cache-Control"); cc != "public, max-age=5" {
		t.Fatalf("client cc %q", cc)
	}
	clk.Advance(21 * time.Second)
	if _, st := do(c, b, img, get("http://h/img")); st != StatusMiss {
		t.Fatalf("defaultTtl not applied: %s", st)
	}
	if _, st := do(c, b, long, get("http://h/long")); st != StatusHit {
		t.Fatalf("long: %s", st)
	}
	clk.Advance(80 * time.Second)
	if _, st := do(c, b, long, get("http://h/long")); st != StatusMiss {
		t.Fatalf("maxTtl not applied: %s", st)
	}
}

// etagOrigin answers conditionals with 304.
func etagOrigin(etag string, hdr ...string) *origin {
	return &origin{serve: func(w http.ResponseWriter, r *http.Request) {
		for i := 0; i+1 < len(hdr); i += 2 {
			w.Header().Set(hdr[i], hdr[i+1])
		}
		w.Header().Set("Etag", etag)
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Write([]byte("payload"))
	}}
}

func TestRevalidation(t *testing.T) {
	c, clk := newCache(t, "", 0)
	o := etagOrigin(`"v1"`, "Content-Type", "text/plain", "Cache-Control", "max-age=10")
	b := svc(UseOriginHeaders)
	do(c, b, o, get("http://h/r"))
	clk.Advance(11 * time.Second)
	w, st := do(c, b, o, get("http://h/r"))
	if st != StatusRevalidated || w.Body.String() != "payload" || w.Code != 200 {
		t.Fatalf("got %s %d %q", st, w.Code, w.Body.String())
	}
	if o.last().Header.Get("If-None-Match") != `"v1"` {
		t.Fatalf("no conditional sent")
	}
	if _, st := do(c, b, o, get("http://h/r")); st != StatusHit {
		t.Fatalf("after revalidation: %s", st)
	}

	// no-cache: stored but revalidated on every request.
	nc := etagOrigin(`"n"`, "Content-Type", "text/plain", "Cache-Control", "no-cache")
	if _, st := do(c, b, nc, get("http://h/nc")); st != StatusMiss {
		t.Fatalf("no-cache first: %s", st)
	}
	for range 2 {
		if _, st := do(c, b, nc, get("http://h/nc")); st != StatusRevalidated {
			t.Fatalf("no-cache: %s", st)
		}
	}

	// Last-Modified validators.
	lm := "Fri, 01 May 2026 10:00:00 GMT"
	lo := &origin{serve: func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Last-Modified", lm)
		w.Header().Set("Cache-Control", "max-age=1")
		if r.Header.Get("If-Modified-Since") == lm {
			w.WriteHeader(304)
			return
		}
		w.Write([]byte("lm"))
	}}
	do(c, b, lo, get("http://h/lm"))
	clk.Advance(2 * time.Second)
	if w, st := do(c, b, lo, get("http://h/lm")); st != StatusRevalidated || w.Body.String() != "lm" {
		t.Fatalf("lm: %s %q", st, w.Body.String())
	}

	// A changed object replaces the entry.
	ch := etagOrigin(`"a"`, "Cache-Control", "max-age=1")
	do(c, b, ch, get("http://h/ch"))
	clk.Advance(2 * time.Second)
	ch.serve = static(200, "new", "Etag", `"b"`, "Cache-Control", "max-age=60")
	if w, st := do(c, b, ch, get("http://h/ch")); st != StatusMiss || w.Body.String() != "new" {
		t.Fatalf("changed: %s %q", st, w.Body.String())
	}
}

func TestClientConditionalAndRange(t *testing.T) {
	c, _ := newCache(t, "", 0)
	body := "0123456789abcdefghij"
	o := &origin{serve: func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Etag", `"e1"`)
		w.Header().Set("Last-Modified", "Fri, 01 May 2026 10:00:00 GMT")
		http.ServeContent(w, r, "", time.Time{}, strings.NewReader(body))
	}}
	b := svc(CacheAllStatic)

	w, st := do(c, b, o, get("http://h/img", "Range", "bytes=2-5"))
	if st != StatusMiss || w.Code != 206 || w.Body.String() != "2345" {
		t.Fatalf("range miss: %s %d %q", st, w.Code, w.Body.String())
	}
	if o.last().Header.Get("Range") != "" {
		t.Fatalf("Range forwarded on cache fill")
	}
	w, st = do(c, b, o, get("http://h/img", "Range", "bytes=-3"))
	if st != StatusHit || w.Code != 206 || w.Body.String() != "hij" || w.Header().Get("Content-Range") != "bytes 17-19/20" {
		t.Fatalf("range hit: %s %d %q %q", st, w.Code, w.Body.String(), w.Header().Get("Content-Range"))
	}
	w, _ = do(c, b, o, get("http://h/img", "Range", "bytes=50-"))
	if w.Code != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("unsatisfiable: %d", w.Code)
	}
	w, st = do(c, b, o, get("http://h/img", "If-None-Match", `"e1"`))
	if st != StatusHit || w.Code != 304 {
		t.Fatalf("inm: %s %d", st, w.Code)
	}
	w, _ = do(c, b, o, get("http://h/img", "If-Modified-Since", "Fri, 01 May 2026 11:00:00 GMT"))
	if w.Code != 304 {
		t.Fatalf("ims: %d", w.Code)
	}
	w, _ = do(c, b, o, get("http://h/img", "If-Range", `"other"`, "Range", "bytes=0-1"))
	if w.Code != 200 || w.Body.String() != body {
		t.Fatalf("if-range mismatch: %d", w.Code)
	}
	head := httptest.NewRequest(http.MethodHead, "http://h/img", nil)
	w, st = do(c, b, o, head)
	if st != StatusHit || w.Body.Len() != 0 || w.Header().Get("Content-Length") != "20" {
		t.Fatalf("head: %s %d %q", st, w.Body.Len(), w.Header().Get("Content-Length"))
	}
	if o.count() != 1 {
		t.Fatalf("origin %d", o.count())
	}

	// Uncacheable range: the original Range goes to the origin.
	h := &origin{serve: func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		http.ServeContent(w, r, "", time.Time{}, strings.NewReader(body))
	}}
	w, st = do(c, b, h, get("http://h/page", "Range", "bytes=0-2"))
	if st != StatusUncacheable || w.Code != 206 || w.Body.String() != "012" {
		t.Fatalf("uncacheable range: %s %d %q", st, w.Code, w.Body.String())
	}

	// HEAD miss goes to the origin as HEAD.
	hm := httptest.NewRequest(http.MethodHead, "http://h/other", nil)
	if _, st := do(c, b, o, hm); st != StatusMiss || o.last().Method != http.MethodHead {
		t.Fatalf("head miss: %s %s", st, o.last().Method)
	}
}

func TestCacheKeyPolicy(t *testing.T) {
	type pair struct{ a, b *http.Request }
	cases := []struct {
		name string
		b    Backend
		p    pair
		same bool
	}{
		{"default query order", svc(""), pair{get("http://h/p?a=1&b=2"), get("http://h/p?b=2&a=1")}, true},
		{"default query differs", svc(""), pair{get("http://h/p?a=1"), get("http://h/p?a=2")}, false},
		{"default host differs", svc(""), pair{get("http://h1/p"), get("http://h2/p")}, false},
		{"default protocol differs", svc(""), pair{get("http://h/p"), get("https://h/p")}, false},
		{"exclude host", withKey(&compute.CacheKeyPolicy{IncludeProtocol: true, IncludeQueryString: true}),
			pair{get("http://h1/p"), get("http://h2/p")}, true},
		{"exclude protocol", withKey(&compute.CacheKeyPolicy{IncludeHost: true, IncludeQueryString: true}),
			pair{get("http://h/p"), get("https://h/p")}, true},
		{"exclude query", withKey(&compute.CacheKeyPolicy{IncludeHost: true, IncludeProtocol: true}),
			pair{get("http://h/p?a=1"), get("http://h/p?a=2")}, true},
		{"allowlist", withKey(&compute.CacheKeyPolicy{IncludeQueryString: true, QueryStringWhitelist: []string{"user"}}),
			pair{get("http://h/p?user=1&color=blue"), get("http://h/p?color=red&user=1")}, true},
		{"allowlist differs", withKey(&compute.CacheKeyPolicy{IncludeQueryString: true, QueryStringWhitelist: []string{"user"}}),
			pair{get("http://h/p?user=1"), get("http://h/p?user=2")}, false},
		{"denylist", withKey(&compute.CacheKeyPolicy{IncludeQueryString: true, QueryStringBlacklist: []string{"utm"}}),
			pair{get("http://h/p?a=1&utm=x"), get("http://h/p?a=1&utm=y")}, true},
		{"denylist keeps others", withKey(&compute.CacheKeyPolicy{IncludeQueryString: true, QueryStringBlacklist: []string{"utm"}}),
			pair{get("http://h/p?a=1"), get("http://h/p?a=2")}, false},
		{"header", withKey(&compute.CacheKeyPolicy{IncludeHttpHeaders: []string{"x-country"}}),
			pair{get("http://h/p", "X-Country", "DE"), get("http://h/p", "X-Country", "FR")}, false},
		{"header same", withKey(&compute.CacheKeyPolicy{IncludeHttpHeaders: []string{"x-country"}}),
			pair{get("http://h/p", "X-Country", "DE"), get("http://h/p", "X-Country", "DE", "X-Other", "1")}, true},
		{"cookie", withKey(&compute.CacheKeyPolicy{IncludeNamedCookies: []string{"ab"}}),
			pair{get("http://h/p", "Cookie", "ab=1; other=2"), get("http://h/p", "Cookie", "ab=2")}, false},
		{"cookie same", withKey(&compute.CacheKeyPolicy{IncludeNamedCookies: []string{"ab"}}),
			pair{get("http://h/p", "Cookie", "ab=1; other=2"), get("http://h/p", "Cookie", "other=3; ab=1")}, true},
		{"bucket ignores host and protocol", bucket(nil), pair{get("http://h1/p"), get("https://h2/p")}, true},
		{"bucket query included", bucket(nil), pair{get("http://h/p?v=1"), get("http://h/p?v=2")}, false},
		{"bucket allowlist", bucket(&compute.BackendBucketCdnPolicyCacheKeyPolicy{QueryStringWhitelist: []string{"v"}}),
			pair{get("http://h/p?v=1&x=1"), get("http://h/p?x=2&v=1")}, true},
		{"bucket header", bucket(&compute.BackendBucketCdnPolicyCacheKeyPolicy{IncludeHttpHeaders: []string{"X-Device"}}),
			pair{get("http://h/p", "X-Device", "m"), get("http://h/p", "X-Device", "d")}, false},
		{"path differs", svc(""), pair{get("http://h/a"), get("http://h/b")}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := newCache(t, "", 0)
			o := &origin{serve: static(200, "x", "Content-Type", "image/png")}
			do(c, tc.b, o, tc.p.a)
			_, st := do(c, tc.b, o, tc.p.b)
			if (st == StatusHit) != tc.same {
				t.Fatalf("second request %s, same key want %v", st, tc.same)
			}
		})
	}
}

func withKey(k *compute.CacheKeyPolicy) Backend {
	b := svc("")
	b.ServicePolicy.CacheKeyPolicy = k
	return b
}

func bucket(k *compute.BackendBucketCdnPolicyCacheKeyPolicy) Backend {
	return Backend{ID: "bb", Kind: "backendBucket", BucketPolicy: &compute.BackendBucketCdnPolicy{CacheKeyPolicy: k}}
}

func TestVaryVariants(t *testing.T) {
	c, _ := newCache(t, "", 0)
	o := &origin{serve: func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Vary", "Accept")
		w.Write([]byte(r.Header.Get("Accept")))
	}}
	b := svc("")
	do(c, b, o, get("http://h/v", "Accept", "image/webp"))
	do(c, b, o, get("http://h/v", "Accept", "image/avif"))
	w, st := do(c, b, o, get("http://h/v", "Accept", "image/webp"))
	if st != StatusHit || w.Body.String() != "image/webp" {
		t.Fatalf("%s %q", st, w.Body.String())
	}
	w, st = do(c, b, o, get("http://h/v", "Accept", "image/avif"))
	if st != StatusHit || w.Body.String() != "image/avif" {
		t.Fatalf("%s %q", st, w.Body.String())
	}
}

func TestNegativeCaching(t *testing.T) {
	cases := []struct {
		name    string
		mode    string
		policy  []*compute.BackendServiceCdnPolicyNegativeCachingPolicy
		status  int
		hdr     []string
		advance time.Duration
		want    [2]string
	}{
		{"404 default ttl", CacheAllStatic, nil, 404, nil, 119 * time.Second, [2]string{"miss", "hit"}},
		{"404 default ttl expired", CacheAllStatic, nil, 404, nil, 121 * time.Second, [2]string{"miss", "miss"}},
		{"301 default 10m", UseOriginHeaders, nil, 301, []string{"Location", "/x"}, 599 * time.Second, [2]string{"miss", "hit"}},
		{"405 default 60s", CacheAllStatic, nil, 405, nil, 61 * time.Second, [2]string{"miss", "miss"}},
		{"302 not by default", CacheAllStatic, nil, 302, []string{"Location", "/x"}, 0, [2]string{"uncacheable", "uncacheable"}},
		{"policy ttl", CacheAllStatic, []*compute.BackendServiceCdnPolicyNegativeCachingPolicy{{Code: 404, Ttl: 5}}, 404, nil, 6 * time.Second, [2]string{"miss", "miss"}},
		{"policy replaces defaults", CacheAllStatic, []*compute.BackendServiceCdnPolicyNegativeCachingPolicy{{Code: 404, Ttl: 5}}, 410, nil, 0, [2]string{"uncacheable", "uncacheable"}},
		{"policy 302", CacheAllStatic, []*compute.BackendServiceCdnPolicyNegativeCachingPolicy{{Code: 302, Ttl: 50}}, 302, nil, 10 * time.Second, [2]string{"miss", "hit"}},
		{"origin headers win", CacheAllStatic, nil, 404, []string{"Cache-Control", "max-age=5"}, 6 * time.Second, [2]string{"miss", "miss"}},
		{"force overrides headers", ForceCacheAll, nil, 404, []string{"Cache-Control", "max-age=5"}, 6 * time.Second, [2]string{"miss", "hit"}},
		{"500 never", CacheAllStatic, nil, 500, nil, 0, [2]string{"uncacheable", "uncacheable"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, clk := newCache(t, "", 0)
			b := svc(tc.mode)
			b.ServicePolicy.NegativeCaching = true
			b.ServicePolicy.NegativeCachingPolicy = tc.policy
			o := &origin{serve: static(tc.status, "err", tc.hdr...)}
			var got [2]string
			for i := range 2 {
				w, st := do(c, b, o, get("http://h/missing"))
				if w.Code != tc.status {
					t.Fatalf("status %d", w.Code)
				}
				got[i] = st
				clk.Advance(tc.advance)
			}
			if got != tc.want {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
}

func TestServeWhileStale(t *testing.T) {
	c, clk := newCache(t, "", 0)
	b := svc(UseOriginHeaders)
	b.ServicePolicy.ServeWhileStale = 100
	o := &origin{serve: static(200, "good", "Cache-Control", "max-age=10")}
	do(c, b, o, get("http://h/s"))
	clk.Advance(50 * time.Second)
	o.serve = static(503, "down")
	w, st := do(c, b, o, get("http://h/s"))
	if st != StatusHit || w.Code != 200 || w.Body.String() != "good" {
		t.Fatalf("stale: %s %d %q", st, w.Code, w.Body.String())
	}
	clk.Advance(100 * time.Second) // beyond serveWhileStale
	w, st = do(c, b, o, get("http://h/s"))
	if w.Code != 503 || st != StatusUncacheable {
		t.Fatalf("too stale: %s %d", st, w.Code)
	}

	// must-revalidate responses are never served stale.
	c2, clk2 := newCache(t, "", 0)
	o2 := &origin{serve: static(200, "good", "Cache-Control", "max-age=10, must-revalidate")}
	do(c2, b, o2, get("http://h/s"))
	clk2.Advance(20 * time.Second)
	o2.serve = static(502, "down")
	if w, _ := do(c2, b, o2, get("http://h/s")); w.Code != 502 {
		t.Fatalf("must-revalidate served stale")
	}

	// Without serveWhileStale errors pass through.
	c3, clk3 := newCache(t, "", 0)
	o3 := &origin{serve: static(200, "good", "Cache-Control", "max-age=10")}
	do(c3, svc(UseOriginHeaders), o3, get("http://h/s"))
	clk3.Advance(20 * time.Second)
	o3.serve = static(504, "down")
	if w, _ := do(c3, svc(UseOriginHeaders), o3, get("http://h/s")); w.Code != 504 {
		t.Fatalf("served stale without serveWhileStale")
	}
}

func TestStaleWhileRevalidate(t *testing.T) {
	c, clk := newCache(t, "", 0)
	b := svc(UseOriginHeaders)
	o := &origin{serve: static(200, "v1", "Cache-Control", "max-age=10, stale-while-revalidate=30")}
	do(c, b, o, get("http://h/swr"))
	clk.Advance(15 * time.Second)
	o.serve = static(200, "v2", "Cache-Control", "max-age=10, stale-while-revalidate=30")
	w, st := do(c, b, o, get("http://h/swr"))
	if st != StatusHit || w.Body.String() != "v1" {
		t.Fatalf("swr: %s %q", st, w.Body.String())
	}
	c.Wait()
	w, st = do(c, b, o, get("http://h/swr"))
	if st != StatusHit || w.Body.String() != "v2" {
		t.Fatalf("after background refresh: %s %q", st, w.Body.String())
	}
	if o.count() != 2 {
		t.Fatalf("origin %d", o.count())
	}
}

func TestRequestCoalescing(t *testing.T) {
	for _, coalesce := range []bool{true, false} {
		t.Run(fmt.Sprint(coalesce), func(t *testing.T) {
			c, _ := newCache(t, "", 0)
			b := svc(CacheAllStatic)
			b.ServicePolicy.RequestCoalescing = coalesce
			release := make(chan struct{})
			var entered atomic.Int32
			o := &origin{serve: func(w http.ResponseWriter, r *http.Request) {
				entered.Add(1)
				<-release
				w.Header().Set("Content-Type", "image/png")
				w.Write([]byte("img"))
			}}
			const n = 20
			var wg sync.WaitGroup
			statuses := make(chan string, n)
			for range n {
				wg.Add(1)
				go func() {
					defer wg.Done()
					w, st := do(c, b, o, get("http://h/c.png"))
					if w.Body.String() != "img" {
						t.Errorf("body %q", w.Body.String())
					}
					statuses <- st
				}()
			}
			// Let requests pile up behind the leader.
			deadline := time.Now().Add(2 * time.Second)
			for entered.Load() == 0 && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			time.Sleep(50 * time.Millisecond)
			close(release)
			wg.Wait()
			close(statuses)
			if coalesce && o.count() != 1 {
				t.Fatalf("coalesced origin requests %d, want 1", o.count())
			}
			if !coalesce && o.count() < 2 {
				t.Fatalf("uncoalesced origin requests %d", o.count())
			}
			for st := range statuses {
				if st != StatusMiss && st != StatusHit {
					t.Errorf("status %s", st)
				}
			}
		})
	}
}

func TestInvalidate(t *testing.T) {
	type req struct{ backend, url string }
	all := []req{
		{"bs1", "http://a.example/img/1.png"},
		{"bs1", "http://a.example/img/2.png?x=1"},
		{"bs1", "http://b.example/img/1.png"},
		{"bs1", "http://a.example/css/s.css"},
		{"bs2", "http://a.example/img/1.png"},
	}
	cases := []struct {
		name     string
		backends []string
		host     string
		path     string
		tags     []string
		gone     []int
	}{
		{"exact path all hosts", nil, "", "/img/1.png", nil, []int{0, 2, 4}},
		{"exact path query variants", []string{"bs1"}, "", "/img/2.png", nil, []int{1}},
		{"prefix", []string{"bs1"}, "a.example", "/img/*", nil, []int{0, 1}},
		{"everything", nil, "", "/*", nil, []int{0, 1, 2, 3, 4}},
		{"host only", nil, "b.example", "", nil, []int{2}},
		{"backend scope", []string{"bs2"}, "", "/*", nil, []int{4}},
		{"tags", nil, "", "", []string{"css"}, []int{3}},
		{"no match", nil, "", "/nope", nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := newCache(t, "", 0)
			o := &origin{serve: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "image/png")
				if strings.HasSuffix(r.URL.Path, ".css") {
					w.Header().Set("Cache-Tag", "css, style")
				}
				w.Write([]byte("x"))
			}}
			backend := func(id string) Backend { b := svc(""); b.ID = id; return b }
			for _, r := range all {
				do(c, backend(r.backend), o, get(r.url))
			}
			c.Invalidate(tc.backends, tc.host, tc.path, tc.tags)
			for i, r := range all {
				_, st := do(c, backend(r.backend), o, get(r.url))
				wantGone := false
				for _, g := range tc.gone {
					wantGone = wantGone || g == i
				}
				if gone := st == StatusMiss; gone != wantGone {
					t.Errorf("%s %s: status %s, want gone=%v", r.backend, r.url, st, wantGone)
				}
			}
		})
	}
}

func TestLRUEvictionAndPurge(t *testing.T) {
	// Each object costs 1000 + entryOverhead; three fit in the limit.
	limit := int64(3 * (1000 + entryOverhead))
	for _, dir := range []string{"", "disk"} {
		t.Run("dir="+dir, func(t *testing.T) {
			d := ""
			if dir != "" {
				d = t.TempDir()
			}
			c, _ := newCache(t, d, limit)
			o := &origin{serve: static(200, strings.Repeat("x", 1000), "Content-Type", "image/png")}
			b := svc("")
			for _, p := range []string{"/1", "/2", "/3"} {
				do(c, b, o, get("http://h"+p))
			}
			do(c, b, o, get("http://h/1")) // /1 most recently used
			do(c, b, o, get("http://h/4")) // evicts /2
			if n, used := c.Stats(); n != 3 || used > limit {
				t.Fatalf("stats %d %d", n, used)
			}
			for p, want := range map[string]string{"/1": "hit", "/3": "hit", "/4": "hit"} {
				if _, st := do(c, b, o, get("http://h"+p)); st != want {
					t.Errorf("%s: %s", p, st)
				}
			}
			if _, st := do(c, b, o, get("http://h/2")); st != StatusMiss {
				t.Errorf("/2 not evicted: %s", st)
			}
			// Objects larger than the cache are not cached.
			big := &origin{serve: static(200, strings.Repeat("y", int(limit)), "Content-Type", "image/png")}
			if w, st := do(c, b, big, get("http://h/big")); st != StatusUncacheable || w.Body.Len() != int(limit) {
				t.Errorf("big: %s %d", st, w.Body.Len())
			}
			c.Purge()
			if n, used := c.Stats(); n != 0 || used != 0 {
				t.Fatalf("after purge %d %d", n, used)
			}
			if _, st := do(c, b, o, get("http://h/1")); st != StatusMiss {
				t.Fatalf("after purge: %s", st)
			}
		})
	}
}

func TestObjectSizeLimits(t *testing.T) {
	c, _ := newCache(t, t.TempDir(), 0)
	b := svc("")
	// > 10 MiB without byte-range support: not cached, streamed through.
	big := strings.Repeat("z", maxObjectNoRanges+1)
	o := &origin{serve: static(200, big, "Content-Type", "video/mp4")}
	if w, st := do(c, b, o, get("http://h/v1")); st != StatusUncacheable || w.Body.Len() != len(big) {
		t.Fatalf("no-range big: %s %d", st, w.Body.Len())
	}
	// With range support it is cached (spilled to disk) and ranges work.
	or := &origin{serve: func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("Etag", `"big"`)
		http.ServeContent(w, r, "", time.Time{}, strings.NewReader(big))
	}}
	if w, st := do(c, b, or, get("http://h/v2")); st != StatusMiss || w.Body.Len() != len(big) {
		t.Fatalf("range big: %s %d", st, w.Body.Len())
	}
	w, st := do(c, b, or, get("http://h/v2", "Range", fmt.Sprintf("bytes=%d-", len(big)-3)))
	if st != StatusHit || w.Body.String() != "zzz" {
		t.Fatalf("range big hit: %s %q", st, w.Body.String())
	}
}

func TestDiskPersistence(t *testing.T) {
	dir := t.TempDir()
	c, _ := newCache(t, dir, 0)
	small := &origin{serve: static(200, "small", "Content-Type", "image/png", "Cache-Control", "max-age=3600")}
	large := &origin{serve: static(200, strings.Repeat("L", memObjectMax+10), "Content-Type", "image/png", "Cache-Control", "max-age=3600")}
	b := svc("")
	do(c, b, small, get("http://h/s"))
	do(c, b, large, get("http://h/l"))

	// A restart rebuilds the index from disk.
	c2, err := NewCache(Options{Dir: dir, Clock: c.clock})
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := c2.Stats(); n != 2 {
		t.Fatalf("rebuilt %d entries", n)
	}
	w, st := do(c2, b, small, get("http://h/s"))
	if st != StatusHit || w.Body.String() != "small" {
		t.Fatalf("small: %s %q", st, w.Body.String())
	}
	w, st = do(c2, b, large, get("http://h/l"))
	if st != StatusHit || w.Body.Len() != memObjectMax+10 {
		t.Fatalf("large: %s %d", st, w.Body.Len())
	}
	if small.count() != 1 || large.count() != 1 {
		t.Fatal("origin re-fetched")
	}
	c2.Invalidate(nil, "", "/*", nil)
	c3, _ := NewCache(Options{Dir: dir, Clock: c.clock})
	if n, _ := c3.Stats(); n != 0 {
		t.Fatalf("invalidated entries persisted: %d", n)
	}
}

func TestSignedRequests(t *testing.T) {
	key := []byte("0123456789abcdef")
	c, clk := newCache(t, "", 0)
	b := svc(UseOriginHeaders)
	b.SignedURLKeys = map[string][]byte{"k1": key}
	b.ServicePolicy.SignedUrlCacheMaxAgeSec = 60
	// The origin requires no caching headers: signed responses are cached
	// as public, max-age=signedUrlCacheMaxAgeSec.
	o := &origin{serve: static(200, "secret", "Content-Type", "text/plain", "Cache-Control", "private")}
	exp := clk.Now().Add(time.Hour)

	signed := SignURL("http://cdn.example/file.txt?v=1", "k1", key, exp)
	w, st := do(c, b, o, get(signed))
	if w.Code != 200 || st != StatusMiss {
		t.Fatalf("signed: %d %s", w.Code, st)
	}
	if q := o.last().URL.RawQuery; q != "v=1" {
		t.Fatalf("signing params forwarded: %q", q)
	}
	if w.Header().Get("Cache-Control") != "private" {
		t.Fatalf("client headers altered: %q", w.Header().Get("Cache-Control"))
	}
	// A differently signed URL for the same object hits.
	signed2 := SignURL("http://cdn.example/file.txt?v=1", "k1", key, exp.Add(time.Minute))
	if _, st := do(c, b, o, get(signed2)); st != StatusHit {
		t.Fatalf("signed2: %s", st)
	}
	// Unsigned requests are not blocked but never served signed content.
	if _, st := do(c, b, o, get("http://cdn.example/file.txt?v=1")); st != StatusUncacheable {
		t.Fatalf("unsigned: %s", st)
	}
	clk.Advance(61 * time.Second)
	if _, st := do(c, b, o, get(signed2)); st != StatusMiss {
		t.Fatalf("signedUrlCacheMaxAgeSec: %s", st)
	}

	reject := []struct{ name, url string }{
		{"tampered", strings.Replace(signed, "v=1", "v=2", 1)},
		{"bad sig", signed[:len(signed)-4] + "AAA="},
		{"unknown key", SignURL("http://cdn.example/file.txt", "k2", key, exp)},
		{"wrong key", SignURL("http://cdn.example/file.txt", "k1", []byte("fedcba9876543210"), exp)},
		{"expired", SignURL("http://cdn.example/file.txt", "k1", key, clk.Now().Add(-time.Second))},
		{"missing keyname", "http://cdn.example/file.txt?Expires=9999999999&Signature=abc"},
		{"prefix mismatch", "http://cdn.example/other/x?" + SignURLPrefix("http://cdn.example/data/", "k1", key, exp)},
	}
	for _, tc := range reject {
		if w, st := do(c, b, o, get(tc.url)); w.Code != 403 || st != StatusUncacheable {
			t.Errorf("%s: %d %s", tc.name, w.Code, st)
		}
	}

	// URL prefix signing.
	pq := SignURLPrefix("http://cdn.example/data/", "k1", key, exp)
	if w, _ := do(c, b, o, get("http://cdn.example/data/a/b.txt?"+pq)); w.Code != 200 {
		t.Fatalf("prefix: %d", w.Code)
	}
	if o.last().URL.RawQuery != "" {
		t.Fatalf("prefix params forwarded: %q", o.last().URL.RawQuery)
	}

	// Signed cookies.
	cookie := SignCookie("http://cdn.example/media/", "k1", key, exp)
	r := get("http://cdn.example/media/v.mp4")
	r.AddCookie(&http.Cookie{Name: SignedCookieName, Value: cookie})
	if w, st := do(c, b, o, r); w.Code != 200 || st != StatusMiss {
		t.Fatalf("cookie: %d %s", w.Code, st)
	}
	r = get("http://cdn.example/elsewhere/v.mp4")
	r.AddCookie(&http.Cookie{Name: SignedCookieName, Value: cookie})
	if w, _ := do(c, b, o, r); w.Code != 403 {
		t.Fatalf("cookie outside prefix: %d", w.Code)
	}

	// Backends without keys do not validate.
	nb := svc(UseOriginHeaders)
	if w, _ := do(c, nb, o, get("http://cdn.example/f?Expires=1&KeyName=x&Signature=bad")); w.Code != 200 {
		t.Fatalf("no keys: %d", w.Code)
	}
}

func TestCompression(t *testing.T) {
	c, _ := newCache(t, "", 0)
	b := svc(ForceCacheAll)
	b.CompressionMode = "AUTOMATIC"
	text := strings.Repeat("hello world ", 200)
	o := &origin{serve: static(200, text, "Content-Type", "text/html", "Etag", `"t"`)}
	for i := range 2 {
		w, _ := do(c, b, o, get("http://h/t", "Accept-Encoding", "gzip, br"))
		if w.Header().Get("Content-Encoding") != "gzip" || w.Header().Get("Etag") != `W/"t"` {
			t.Fatalf("%d: headers %v", i, w.Header())
		}
		if w.Body.Len() >= len(text) {
			t.Fatalf("not compressed")
		}
	}
	w, _ := do(c, b, o, get("http://h/t"))
	if w.Header().Get("Content-Encoding") != "" || w.Body.String() != text {
		t.Fatalf("identity: %v", w.Header())
	}
	w, _ = do(c, b, o, get("http://h/t", "Accept-Encoding", "gzip", "Range", "bytes=0-4"))
	if w.Header().Get("Content-Encoding") != "" || w.Body.String() != "hello" {
		t.Fatalf("range compressed")
	}
}

func TestParseSize(t *testing.T) {
	for in, want := range map[string]int64{"": 1 << 30, "1GiB": 1 << 30, "512MiB": 512 << 20, "100MB": 100e6, "4096": 4096, "64k": 64 << 10, "2g": 2 << 30} {
		got, err := ParseSize(in)
		if err != nil || got != want {
			t.Errorf("%q: %d %v", in, got, err)
		}
	}
	for _, bad := range []string{"x", "-1", "10XB", "0"} {
		if _, err := ParseSize(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// chunked writes body in 64 KiB pieces without a Content-Length.
func chunked(body string, hdr ...string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		for i := 0; i+1 < len(hdr); i += 2 {
			w.Header().Set(hdr[i], hdr[i+1])
		}
		for i := 0; i < len(body); i += 64 << 10 {
			w.Write([]byte(body[i:min(i+64<<10, len(body))]))
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
	}
}

func TestStreamingFills(t *testing.T) {
	for _, dir := range []string{"", "disk"} {
		t.Run("dir="+dir, func(t *testing.T) {
			d := ""
			if dir != "" {
				d = t.TempDir()
			}
			c, _ := newCache(t, d, 0)
			b := svc("")
			// Exceeds 10 MiB mid-stream (after spilling to disk): the
			// buffered part and the rest reach the client intact.
			big := strings.Repeat("0123456789abcdef", (maxObjectNoRanges+200<<10)/16)
			o := &origin{serve: chunked(big, "Content-Type", "image/png")}
			w, st := do(c, b, o, get("http://h/big"))
			if st != StatusUncacheable || w.Body.String() != big {
				t.Fatalf("big: %s len %d", st, w.Body.Len())
			}
			// A 2 MiB streamed object is cached.
			mid := big[:2<<20]
			o2 := &origin{serve: chunked(mid, "Content-Type", "image/png")}
			do(c, b, o2, get("http://h/mid"))
			if w, st := do(c, b, o2, get("http://h/mid")); st != StatusHit || w.Body.String() != mid {
				t.Fatalf("mid: %s %d", st, w.Body.Len())
			}
			// Uncacheable responses stream through with flushes.
			o3 := &origin{serve: chunked(mid, "Content-Type", "text/html")}
			if w, st := do(c, b, o3, get("http://h/page")); st != StatusUncacheable || w.Body.String() != mid || !w.Flushed {
				t.Fatalf("pass: %s %v", st, w.Flushed)
			}
			if d != "" {
				des, _ := os.ReadDir(d)
				for _, de := range des {
					if strings.HasSuffix(de.Name(), ".tmp") {
						t.Errorf("leftover temp file %s", de.Name())
					}
				}
			}
		})
	}
}

func TestStaleOnLargeError(t *testing.T) {
	c, clk := newCache(t, t.TempDir(), 0)
	b := svc(UseOriginHeaders)
	b.ServicePolicy.ServeWhileStale = 3600
	o := &origin{serve: static(200, "good", "Cache-Control", "max-age=1")}
	do(c, b, o, get("http://h/x"))
	clk.Advance(5 * time.Second)
	o.serve = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		chunked(strings.Repeat("e", 3<<20))(w, r)
	}
	if w, st := do(c, b, o, get("http://h/x")); st != StatusHit || w.Body.String() != "good" {
		t.Fatalf("%s %q", st, w.Body.String())
	}
}
