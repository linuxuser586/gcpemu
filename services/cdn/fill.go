package cdn

import (
	"compress/gzip"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
)

// fill modes, decided when the origin writes its header.
const (
	modeUndecided   = iota
	modeStore       // cacheable: buffer, then store and serve from cache
	modePass        // uncacheable: stream straight to the client
	modeRetry       // uncacheable but the client's Range was stripped: discard, re-request
	modeRevalidated // 304 for a stale entry: discard body
	modeStale       // origin error with a stale entry we may serve
)

// fillWriter is the ResponseWriter handed to the origin on cache fills.
type fillWriter struct {
	client        http.ResponseWriter
	c             *Cache
	q             *request
	header        http.Header
	stale         *entry
	staleMeta     *entryMeta
	rangeStripped bool

	mode   int
	status int
	d      decision
	limit  int64
	buf    []byte
	file   *os.File
	size   int64
}

func (f *fillWriter) Header() http.Header { return f.header }

func (f *fillWriter) WriteHeader(code int) {
	if f.mode != modeUndecided {
		return
	}
	if code >= 100 && code < 200 {
		return // informational; not forwarded
	}
	f.status = code
	now := f.c.clock.Now()
	if f.stale != nil {
		if code == http.StatusNotModified {
			f.mode = modeRevalidated
			return
		}
		sm := f.staleMeta
		if code >= 500 && !sm.NoStale && f.q.p.serveWhileStale > 0 &&
			sm.age(now)-sm.TTL <= f.q.p.serveWhileStale {
			f.mode, f.limit = modeStale, maxObjectNoRanges
			return
		}
	}
	f.d = f.q.p.decide(f.q.r, code, f.header, f.q.signed, now)
	if f.d.cacheable && f.d.ttl == 0 && f.header.Get("Etag") == "" && f.header.Get("Last-Modified") == "" {
		// Always stale and impossible to revalidate: storing is useless.
		f.d = no("ttl 0 without validators")
	}
	if f.d.cacheable {
		f.limit = min(maxObjectSize(f.header), f.c.st.limit-entryOverhead)
		if cl, err := strconv.ParseInt(f.header.Get("Content-Length"), 10, 64); err == nil && cl > f.limit {
			f.d = no("too large")
		}
	}
	if f.d.cacheable {
		f.mode = modeStore
		return
	}
	f.pass()
}

// pass switches to streaming the response to the client.
func (f *fillWriter) pass() {
	if f.rangeStripped && f.status == http.StatusOK {
		f.mode = modeRetry
		return
	}
	f.mode = modePass
	h := f.client.Header()
	for k, v := range f.header {
		h[k] = v
	}
	f.client.WriteHeader(f.status)
}

func (f *fillWriter) Write(b []byte) (int, error) {
	if f.mode == modeUndecided {
		f.WriteHeader(http.StatusOK)
	}
	switch f.mode {
	case modePass:
		return f.client.Write(b)
	case modeRetry, modeRevalidated:
		return len(b), nil
	}
	if f.size+int64(len(b)) > f.limit {
		if f.mode == modeStale {
			return len(b), nil
		}
		// Too large to cache: give up and stream what we have.
		f.d = no("too large")
		f.pass()
		if f.mode == modeRetry {
			f.dropBuffer()
			return len(b), nil
		}
		if err := f.flushBuffer(); err != nil {
			return 0, err
		}
		return f.client.Write(b)
	}
	f.size += int64(len(b))
	if f.file != nil {
		return f.file.Write(b)
	}
	f.buf = append(f.buf, b...)
	if len(f.buf) > memObjectMax && f.c.st.dir != "" {
		if fl, err := f.c.st.newBodyFile(); err == nil {
			if _, err := fl.Write(f.buf); err != nil {
				fl.Close()
				os.Remove(fl.Name())
			} else {
				f.file, f.buf = fl, nil
			}
		}
	}
	return len(b), nil
}

// Flush forwards flushes when streaming.
func (f *fillWriter) Flush() {
	if f.mode == modePass {
		if fl, ok := f.client.(http.Flusher); ok {
			fl.Flush()
		}
	}
}

// flushBuffer writes what was buffered to the client.
func (f *fillWriter) flushBuffer() error {
	if f.file != nil {
		defer f.dropBuffer()
		if _, err := f.file.Seek(0, 0); err != nil {
			return err
		}
		buf := make([]byte, 32<<10)
		for {
			n, err := f.file.Read(buf)
			if n > 0 {
				if _, werr := f.client.Write(buf[:n]); werr != nil {
					return werr
				}
			}
			if err != nil {
				return nil
			}
		}
	}
	_, err := f.client.Write(f.buf)
	f.buf = nil
	return err
}

// closeFile closes the spill file handle without removing its path.
func (f *fillWriter) closeFile() {
	if f.file != nil {
		f.file.Close()
		f.file = nil
	}
}

func (f *fillWriter) dropBuffer() {
	if f.file != nil {
		f.file.Close()
		os.Remove(f.file.Name())
		f.file = nil
	}
	f.buf = nil
}

// finish completes a response whose handler wrote nothing.
func (f *fillWriter) finish() {
	if f.mode == modeUndecided {
		f.WriteHeader(http.StatusOK)
	}
	if f.mode != modeStore && f.mode != modeStale {
		f.dropBuffer()
	}
}

// maxObjectSize is the largest cacheable object for a response: 100 GiB
// when the origin supports byte ranges, 10 MiB otherwise.
func maxObjectSize(h http.Header) int64 {
	et := h.Get("Etag")
	strong := et != "" && !strings.HasPrefix(et, "W/")
	if h.Get("Accept-Ranges") == "bytes" && h.Get("Content-Length") != "" && (strong || h.Get("Last-Modified") != "") {
		return maxObjectRanges
	}
	return maxObjectNoRanges
}

// compressWriter implements compressionMode AUTOMATIC: gzip for clients
// that accept it, on compressible, not yet encoded 200 responses.
type compressWriter struct {
	http.ResponseWriter
	gz      *gzip.Writer
	decided bool
}

var gzPool = sync.Pool{New: func() any { gz, _ := gzip.NewWriterLevel(nil, gzip.DefaultCompression); return gz }}

func newCompressWriter(w http.ResponseWriter, r *http.Request) *compressWriter {
	if r.Method != http.MethodGet || r.Header.Get("Range") != "" || !acceptsGzip(r.Header.Values("Accept-Encoding")) {
		return nil
	}
	return &compressWriter{ResponseWriter: w}
}

func acceptsGzip(vals []string) bool {
	for _, line := range vals {
		for _, v := range strings.Split(line, ",") {
			name, q, _ := strings.Cut(strings.TrimSpace(v), ";")
			if strings.EqualFold(strings.TrimSpace(name), "gzip") {
				return strings.ReplaceAll(strings.TrimSpace(q), " ", "") != "q=0"
			}
		}
	}
	return false
}

func compressible(ct string) bool {
	mt, _, _ := strings.Cut(ct, ";")
	mt = strings.ToLower(strings.TrimSpace(mt))
	if strings.HasPrefix(mt, "text/") {
		return true
	}
	switch mt {
	case "application/javascript", "application/x-javascript", "application/json", "application/xml",
		"application/xhtml+xml", "application/rss+xml", "application/atom+xml", "application/manifest+json",
		"application/ld+json", "application/wasm", "image/svg+xml", "image/x-icon", "font/ttf", "font/otf":
		return true
	}
	return false
}

func (c *compressWriter) WriteHeader(code int) {
	if c.decided {
		c.ResponseWriter.WriteHeader(code)
		return
	}
	c.decided = true
	h := c.Header()
	cl, clErr := strconv.ParseInt(h.Get("Content-Length"), 10, 64)
	if code == http.StatusOK && h.Get("Content-Encoding") == "" && compressible(h.Get("Content-Type")) &&
		!parseCacheControl(h.Values("Cache-Control")).noTransform && (clErr != nil || cl >= 1024) {
		h.Set("Content-Encoding", "gzip")
		h.Del("Content-Length")
		h.Del("Accept-Ranges")
		h.Add("Vary", "Accept-Encoding")
		if et := h.Get("Etag"); et != "" && !strings.HasPrefix(et, "W/") {
			h.Set("Etag", "W/"+et)
		}
		gz := gzPool.Get().(*gzip.Writer)
		gz.Reset(c.ResponseWriter)
		c.gz = gz
	}
	c.ResponseWriter.WriteHeader(code)
}

func (c *compressWriter) Write(b []byte) (int, error) {
	if !c.decided {
		c.WriteHeader(http.StatusOK)
	}
	if c.gz != nil {
		return c.gz.Write(b)
	}
	return c.ResponseWriter.Write(b)
}

func (c *compressWriter) Flush() {
	if c.gz != nil {
		c.gz.Flush()
	}
	if fl, ok := c.ResponseWriter.(http.Flusher); ok {
		fl.Flush()
	}
}

// Close finishes the gzip stream.
func (c *compressWriter) Close() {
	if c.gz != nil {
		c.gz.Close()
		gzPool.Put(c.gz)
		c.gz = nil
	}
}
