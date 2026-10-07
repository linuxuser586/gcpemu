package cdn

import (
	"context"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	compute "google.golang.org/api/compute/v1"
)

// Values of the {cdn_cache_status} header variable (FR-CDN-004).
const (
	StatusHit         = "hit"
	StatusMiss        = "miss"
	StatusRevalidated = "revalidated"
	StatusUncacheable = "uncacheable"
)

// DefaultCacheSize is the default cache size limit (FR-CDN-008).
const DefaultCacheSize = 1 << 30

// Backend describes the CDN-enabled backend service or backend bucket a
// request is served by. lb builds it from the compute resource.
type Backend struct {
	// ID is the backend service or backend bucket selfLink; it namespaces
	// the cache and is the unit of invalidation.
	ID string
	// Kind is "backendService" or "backendBucket".
	Kind string
	// ServicePolicy or BucketPolicy is the resource's cdnPolicy.
	ServicePolicy *compute.BackendServiceCdnPolicy
	BucketPolicy  *compute.BackendBucketCdnPolicy
	// CompressionMode is the resource's compressionMode
	// ("AUTOMATIC" enables dynamic gzip compression).
	CompressionMode string
	// SignedURLKeys maps signed URL key names to their raw 16-byte
	// values (FR-CDN-007). Empty means no signature validation.
	SignedURLKeys map[string][]byte
	// SignedURLCacheMaxAgeSec overrides cdnPolicy.signedUrlCacheMaxAgeSec
	// when non-zero (default 3600).
	SignedURLCacheMaxAgeSec int64
}

// Clock is the time source of a Cache.
type Clock interface{ Now() time.Time }

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

// Options configure a Cache.
type Options struct {
	// Dir is the disk tier directory; "" keeps everything in memory.
	Dir string
	// MaxBytes is the size limit (default DefaultCacheSize).
	MaxBytes int64
	// Clock defaults to the wall clock.
	Clock Clock
}

// Cache is the Cloud CDN HTTP cache shared by every CDN-enabled backend.
type Cache struct {
	st    *store
	clock Clock

	mu       sync.Mutex
	inflight map[string]*flight
	// bg tracks background revalidations (stale-while-revalidate).
	bg sync.WaitGroup
}

type flight struct{ done chan struct{} }

// NewCache returns a cache; with a Dir it rebuilds its index from disk.
func NewCache(o Options) (*Cache, error) {
	if o.MaxBytes <= 0 {
		o.MaxBytes = DefaultCacheSize
	}
	if o.Clock == nil {
		o.Clock = realClock{}
	}
	c := &Cache{st: newStore(o.Dir, o.MaxBytes), clock: o.Clock, inflight: map[string]*flight{}}
	return c, c.st.load()
}

// Purge removes every cached object (FR-CDN-008) and returns the count.
func (c *Cache) Purge() { c.purge() }

func (c *Cache) purge() int { return c.st.purge() }

// Stats reports the number of entries and the bytes they use.
func (c *Cache) Stats() (entries int, bytes int64) {
	n, used, _ := c.st.stats()
	return n, used
}

// Limit returns the size limit in bytes.
func (c *Cache) Limit() int64 { return c.st.limit }

// Wait blocks until background revalidations finish (tests, shutdown).
func (c *Cache) Wait() { c.bg.Wait() }

// Invalidate removes the entries of the given backends (all backends when
// empty) whose host matches host ("" = any) and whose path matches path:
// exact, or a prefix when path ends in "/*" ("" = any). With cacheTags,
// only entries carrying one of the tags (Cache-Tag response header) match.
// It is effective before it returns (FR-CDN-005).
func (c *Cache) Invalidate(backendIDs []string, host, path string, cacheTags []string) {
	c.invalidate(backendIDs, host, path, cacheTags)
}

func (c *Cache) invalidate(backendIDs []string, host, path string, cacheTags []string) int {
	host = strings.ToLower(host)
	prefix, wild := strings.CutSuffix(path, "*")
	return c.st.invalidate(func(e *entry) bool {
		if len(backendIDs) > 0 && !slices.Contains(backendIDs, e.BackendID) {
			return false
		}
		if host != "" && e.Host != host {
			return false
		}
		if path != "" {
			if wild {
				if !strings.HasPrefix(e.Path, prefix) {
					return false
				}
			} else if e.Path != path {
				return false
			}
		}
		if len(cacheTags) > 0 && !slices.ContainsFunc(e.Tags, func(t string) bool { return slices.Contains(cacheTags, t) }) {
			return false
		}
		return true
	})
}

// request is one Serve call's evaluated state.
type request struct {
	r       *http.Request // client request (signing params stripped)
	b       *Backend
	p       *policy
	signed  bool
	primary string
	path    string
}

// Serve answers r from cache or via origin (the LB's backend handler) per
// Cloud CDN rules and returns the {cdn_cache_status} value: "hit", "miss",
// "revalidated" or "uncacheable" (FR-CDN-001..007).
func (c *Cache) Serve(w http.ResponseWriter, r *http.Request, b Backend, origin http.Handler) string {
	now := c.clock.Now()
	signed, err := verifySigned(r, b.SignedURLKeys, now)
	if err != nil {
		http.Error(w, "403 Forbidden: invalid signed request", http.StatusForbidden)
		return StatusUncacheable
	}
	if signed && r.URL.RawQuery != "" {
		// Signing params never reach the origin.
		r2 := r.Clone(r.Context())
		r2.URL.RawQuery = stripSigning(r.URL.RawQuery)
		r2.RequestURI = ""
		r = r2
	}
	if b.CompressionMode == "AUTOMATIC" {
		if cw := newCompressWriter(w, r); cw != nil {
			defer cw.Close()
			w = cw
		}
	}
	p := resolvePolicy(&b)
	if (r.Method != http.MethodGet && r.Method != http.MethodHead) || bypass(r, p) {
		origin.ServeHTTP(w, r)
		return StatusUncacheable
	}
	q := &request{r: r, b: &b, p: p, signed: signed, path: r.URL.Path}
	q.primary = b.ID + "\x00" + p.key.cacheKey(r, signed)

	if e := c.st.lookup(q.primary, r); e != nil {
		m := c.st.snapshot(e)
		age := m.age(now)
		switch {
		case age < m.TTL:
			if c.serveEntry(w, r, e, &m, age, true) {
				return StatusHit
			}
		case !m.NoStale && m.SWR > 0 && age < m.TTL+m.SWR:
			// stale-while-revalidate: serve stale, refresh in background.
			if c.serveEntry(w, r, e, &m, age, true) {
				c.revalidateAsync(q, origin)
				return StatusHit
			}
		}
	}
	if r.Method == http.MethodHead {
		// Only GET fills the cache; a HEAD miss goes to the origin as is.
		origin.ServeHTTP(w, r)
		return StatusMiss
	}
	return c.fetch(w, q, origin)
}

func bypass(r *http.Request, p *policy) bool {
	for _, h := range p.bypass {
		if r.Header.Get(h) != "" {
			return true
		}
	}
	return parseCacheControl(r.Header.Values("Cache-Control")).noStore
}

// fetch goes to the origin for a miss or a stale entry, coalescing
// concurrent fills of the same key (FR-CDN-006).
func (c *Cache) fetch(w http.ResponseWriter, q *request, origin http.Handler) string {
	if q.p.coalesce {
		c.mu.Lock()
		f, busy := c.inflight[q.primary]
		if !busy {
			f = &flight{done: make(chan struct{})}
			c.inflight[q.primary] = f
		}
		c.mu.Unlock()
		if busy {
			select {
			case <-f.done:
			case <-q.r.Context().Done():
				return StatusMiss
			}
			// The leader's response, if it was cached, serves us too.
			if e := c.st.lookup(q.primary, q.r); e != nil {
				m := c.st.snapshot(e)
				now := c.clock.Now()
				if age := m.age(now); age < m.TTL || m.TTL == 0 {
					if c.serveEntry(w, q.r, e, &m, age, false) {
						return StatusMiss
					}
				}
			}
			return c.fill(w, q, origin)
		}
		defer func() {
			c.mu.Lock()
			delete(c.inflight, q.primary)
			c.mu.Unlock()
			close(f.done)
		}()
	}
	return c.fill(w, q, origin)
}

// revalidateAsync refreshes a stale entry in the background.
func (c *Cache) revalidateAsync(q *request, origin http.Handler) {
	c.mu.Lock()
	if _, busy := c.inflight[q.primary]; busy {
		c.mu.Unlock()
		return
	}
	f := &flight{done: make(chan struct{})}
	c.inflight[q.primary] = f
	c.mu.Unlock()
	bq := *q
	bq.r = q.r.Clone(context.WithoutCancel(q.r.Context()))
	bq.r.Header.Del("Range")
	c.bg.Add(1)
	go func() {
		defer c.bg.Done()
		defer func() {
			c.mu.Lock()
			delete(c.inflight, q.primary)
			c.mu.Unlock()
			close(f.done)
		}()
		c.fill(discardWriter{h: http.Header{}}, &bq, origin)
	}()
}

// conditionalHeaders are stripped from cache fill requests; the cache
// answers client conditionals itself.
var conditionalHeaders = []string{"Range", "If-Range", "If-None-Match", "If-Modified-Since", "If-Match", "If-Unmodified-Since"}

// fill sends the origin request (a revalidation when a stale entry with
// validators exists) and serves the client.
func (c *Cache) fill(w http.ResponseWriter, q *request, origin http.Handler) string {
	r := q.r
	stale := c.st.lookup(q.primary, r)
	var sm entryMeta
	if stale != nil {
		sm = c.st.snapshot(stale)
	}
	or := r.Clone(r.Context())
	or.Method = http.MethodGet
	for _, h := range conditionalHeaders {
		or.Header.Del(h)
	}
	if stale != nil {
		if et := sm.Header.Get("Etag"); et != "" {
			or.Header.Set("If-None-Match", et)
		}
		if lm := sm.Header.Get("Last-Modified"); lm != "" {
			or.Header.Set("If-Modified-Since", lm)
		}
	}
	fw := &fillWriter{
		client:        w,
		c:             c,
		q:             q,
		header:        http.Header{},
		stale:         stale,
		staleMeta:     &sm,
		rangeStripped: r.Header.Get("Range") != "",
	}
	origin.ServeHTTP(fw, or)
	fw.finish()

	switch fw.mode {
	case modePass:
		if stale != nil {
			c.st.remove(stale)
		}
		return StatusUncacheable
	case modeRetry:
		if stale != nil {
			c.st.remove(stale)
		}
		origin.ServeHTTP(w, r)
		return StatusUncacheable
	case modeRevalidated:
		now := c.clock.Now()
		h := mergeHeaders(sm.Header, fw.header)
		d := q.p.decide(r, sm.Status, h, q.signed, now)
		if !d.cacheable {
			c.st.remove(stale)
			// Serve the old body once; it was valid per the origin.
			if c.serveEntry(w, r, stale, &sm, 0, false) {
				return StatusUncacheable
			}
			return c.fill(w, q, origin)
		}
		c.st.refresh(stale, h, d, now, originAge(fw.header))
		m := c.st.snapshot(stale)
		if c.serveEntry(w, r, stale, &m, m.age(now), true) {
			return StatusRevalidated
		}
		return c.fill(w, q, origin)
	case modeStale:
		// Origin failed: serve while stale (FR-CDN-006).
		defer fw.dropBuffer()
		if c.serveEntry(w, r, stale, &sm, sm.age(c.clock.Now()), true) {
			return StatusHit
		}
		writeBuffered(w, fw)
		return StatusUncacheable
	case modeStore:
		// The spill file (if any) was adopted or removed by store; the
		// open handle still serves writeBuffered fallbacks.
		defer fw.closeFile()
		e, err := c.store(q, fw)
		if err != nil || e == nil {
			writeBuffered(w, fw)
			return StatusUncacheable
		}
		m := c.st.snapshot(e)
		if c.serveEntry(w, r, e, &m, 0, false) {
			return StatusMiss
		}
		writeBuffered(w, fw)
		return StatusMiss
	}
	return StatusUncacheable
}

// store inserts a buffered fill into the cache.
func (c *Cache) store(q *request, fw *fillWriter) (*entry, error) {
	h := fw.header.Clone()
	for _, k := range hopHeaders {
		h.Del(k)
	}
	h.Del("Age")
	vary := varyNames(h)
	m := entryMeta{
		Primary:    q.primary,
		BackendID:  q.b.ID,
		Host:       hostname(q.r),
		Path:       q.path,
		VaryNames:  vary,
		VaryValues: varyValues(q.r, vary),
		Tags:       cacheTags(h),
		Status:     fw.status,
		Header:     h,
		Size:       fw.size,
		Stored:     c.clock.Now(),
		InitialAge: originAge(fw.header),
		TTL:        fw.d.ttl,
		NoStale:    fw.d.noStale,
		SWR:        fw.d.swr,
		ClientCC:   fw.d.clientCC,
	}
	m.ID = m.Primary + "\x00" + m.VaryValues
	var body []byte
	tmp := ""
	if fw.file != nil {
		tmp = fw.file.Name()
	} else {
		body = fw.buf
	}
	if err := c.st.put(m, body, tmp); err != nil {
		return nil, err
	}
	return c.st.lookup(q.primary, q.r), nil
}

// serveEntry writes e to w, answering Range and conditional requests from
// the cached object. It returns false (writing nothing) when the body is
// no longer readable.
func (c *Cache) serveEntry(w http.ResponseWriter, r *http.Request, e *entry, m *entryMeta, age int64, withAge bool) bool {
	rd, closeFn, err := c.st.open(e)
	if err != nil {
		c.st.remove(e)
		return false
	}
	defer closeFn()
	h := w.Header()
	for k, v := range m.Header {
		h[k] = v
	}
	if m.ClientCC != "" {
		h.Set("Cache-Control", m.ClientCC)
		h.Del("Expires")
	}
	if withAge {
		h.Set("Age", strconv.FormatInt(age, 10))
	}
	if m.Status == http.StatusOK {
		var mod time.Time
		if lm := m.Header.Get("Last-Modified"); lm != "" {
			mod, _ = http.ParseTime(lm)
		}
		http.ServeContent(w, r, "", mod, rd)
		return true
	}
	h.Set("Content-Length", strconv.FormatInt(m.Size, 10))
	w.WriteHeader(m.Status)
	if r.Method != http.MethodHead {
		io.Copy(w, rd)
	}
	return true
}

// writeBuffered writes a buffered origin response to w unchanged.
func writeBuffered(w http.ResponseWriter, fw *fillWriter) {
	h := w.Header()
	for k, v := range fw.header {
		h[k] = v
	}
	w.WriteHeader(fw.status)
	if fw.file != nil {
		fw.file.Seek(0, io.SeekStart)
		io.Copy(w, fw.file)
		return
	}
	w.Write(fw.buf)
}

var hopHeaders = []string{"Connection", "Keep-Alive", "Proxy-Connection", "Transfer-Encoding", "Te", "Trailer", "Upgrade"}

// mergeHeaders updates a cached header with a 304's headers (RFC 9111 4.3.4).
func mergeHeaders(old, upd http.Header) http.Header {
	h := old.Clone()
	for k, v := range upd {
		switch k {
		case "Content-Length", "Content-Type", "Content-Encoding", "Content-Range":
			continue
		}
		h[k] = v
	}
	for _, k := range hopHeaders {
		h.Del(k)
	}
	h.Del("Age")
	return h
}

func originAge(h http.Header) int64 {
	if a := h.Get("Age"); a != "" {
		return parseSeconds(a)
	}
	return 0
}

// cacheTags returns the Cache-Tag values of a response.
func cacheTags(h http.Header) []string {
	var out []string
	for _, line := range h.Values("Cache-Tag") {
		for _, t := range strings.Split(line, ",") {
			if t = strings.TrimSpace(t); t != "" {
				out = append(out, t)
			}
		}
	}
	return out
}

// discardWriter swallows a background revalidation's client output.
type discardWriter struct{ h http.Header }

func (d discardWriter) Header() http.Header         { return d.h }
func (d discardWriter) Write(b []byte) (int, error) { return len(b), nil }
func (d discardWriter) WriteHeader(int)             {}
