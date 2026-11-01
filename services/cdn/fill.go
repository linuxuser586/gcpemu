package cdn

import (
	"compress/gzip"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/andybalholm/brotli"
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
	noStore       bool // never store, whatever the cache mode (NoStore)

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
	if f.noStore {
		f.d = no("no-store")
		f.pass()
		return
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

// NoStore keeps the response the origin is about to write to w out of the
// cache, whatever the cache mode (FORCE_CACHE_ALL ignores no-store). It
// finds the cache fill through w's Unwrap chain, and does nothing when w
// isn't one. Call it before writing the header.
func NoStore(w http.ResponseWriter) {
	for w != nil {
		if f, ok := w.(*fillWriter); ok {
			f.noStore = true
			return
		}
		u, ok := w.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			return
		}
		w = u.Unwrap()
	}
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

// compressWriter implements compressionMode AUTOMATIC: Brotli or gzip for
// clients that accept them (Brotli preferred, as GCP does), on compressible,
// not yet encoded 200 responses with a Content-Length from 1 KiB to 10 MiB.
type compressWriter struct {
	http.ResponseWriter
	coding  string
	zw      encoder
	decided bool
}

// encoder is the part of gzip.Writer and brotli.Writer compressWriter uses.
type encoder interface {
	io.WriteCloser
	Flush() error
	Reset(io.Writer)
}

var encPools = map[string]*sync.Pool{
	"br":   {New: func() any { return brotli.NewWriterLevel(nil, brotli.DefaultCompression) }},
	"gzip": {New: func() any { gz, _ := gzip.NewWriterLevel(nil, gzip.DefaultCompression); return gz }},
}

func newCompressWriter(w http.ResponseWriter, r *http.Request) *compressWriter {
	if r.Method != http.MethodGet {
		return nil
	}
	coding := pickCoding(r.Header.Values("Accept-Encoding"))
	if coding == "" {
		return nil
	}
	return &compressWriter{ResponseWriter: w, coding: coding}
}

// pickCoding returns "br" if the client accepts it, else "gzip" if it
// accepts that, else "". A zero weight refuses a coding, and a refusal wins
// over an acceptance of the same coding; "*" and other weights are ignored.
func pickCoding(vals []string) string {
	accepted, refused := map[string]bool{}, map[string]bool{}
	for _, line := range vals {
		for _, v := range strings.Split(line, ",") {
			name, params, _ := strings.Cut(v, ";")
			name = strings.ToLower(strings.TrimSpace(name))
			if zeroWeight(params) {
				refused[name] = true
			} else {
				accepted[name] = true
			}
		}
	}
	for _, coding := range []string{"br", "gzip"} {
		if accepted[coding] && !refused[coding] {
			return coding
		}
	}
	return ""
}

// zeroWeight reports whether an Accept-Encoding member's parameters carry a
// q of zero in any spelling RFC 9110 allows (q=0, Q=0.0, q=0.000).
func zeroWeight(params string) bool {
	for _, p := range strings.Split(params, ";") {
		k, v, _ := strings.Cut(p, "=")
		if strings.EqualFold(strings.TrimSpace(k), "q") {
			q, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
			return err == nil && q == 0
		}
	}
	return false
}

// compressibleTypes are the media types GCP compresses by exact match,
// lowercased; compressible also matches its patterns.
var compressibleTypes = map[string]bool{
	"application/x-javascript": true, "application/x-sdch-dictionary": true, "application/javascript": true,
	"application/xml": true, "application/csv": true, "application/json": true, "application/json+protobuf": true,
	"application/signed-exchange": true, "application/vnd.apple.mpegurl": true, "application/wasm": true,
	"application/x-plist": true, "application/x-protobuffer": true, "application/x-protobuf": true,
	"application/x-nacl": true, "application/x-pnacl": true, "font/ttf": true, "font/otf": true, "font/eot": true,
	"image/svg+xml": true, "image/pwg-raster": true, "image/x-icon": true, "image/vnd.microsoft.icon": true,
	"video/vnd.mpeg.dash.mpd": true, "audio/mpegurl": true, "application/dash+xml": true,
	"application/vnd.ms-sstr+xml": true,
}

// compressible reports whether GCP compresses content type ct: one of
// compressibleTypes, or text/*, application/*+json, application/*+xml or
// application/*mpegURL, ignoring case and parameters.
func compressible(ct string) bool {
	mt, _, _ := strings.Cut(ct, ";")
	mt = strings.ToLower(strings.TrimSpace(mt))
	if compressibleTypes[mt] || strings.HasPrefix(mt, "text/") {
		return true
	}
	sub, ok := strings.CutPrefix(mt, "application/")
	return ok && (strings.HasSuffix(sub, "+json") || strings.HasSuffix(sub, "+xml") || strings.HasSuffix(sub, "mpegurl"))
}

// GCP compresses only responses whose Content-Length is in this range.
const (
	minCompressed = 1 << 10
	maxCompressed = 10 << 20
)

// willCompress reports whether c compresses a response with this status
// and header.
func (c *compressWriter) willCompress(code int, h http.Header) bool {
	cl, err := strconv.ParseInt(h.Get("Content-Length"), 10, 64)
	return code == http.StatusOK && err == nil && cl >= minCompressed && cl <= maxCompressed &&
		h.Get("Content-Encoding") == "" && compressible(h.Get("Content-Type")) &&
		!parseCacheControl(h.Values("Cache-Control")).noTransform
}

func (c *compressWriter) WriteHeader(code int) {
	if c.decided {
		c.ResponseWriter.WriteHeader(code)
		return
	}
	c.decided = true
	h := c.Header()
	if c.willCompress(code, h) {
		h.Set("Content-Encoding", c.coding)
		h.Del("Content-Length")
		h.Set("Accept-Ranges", "none")
		h.Add("Vary", "Accept-Encoding")
		if et := h.Get("Etag"); et != "" && !strings.HasPrefix(et, "W/") {
			h.Set("Etag", "W/"+et)
		}
		zw := encPools[c.coding].Get().(encoder)
		zw.Reset(c.ResponseWriter)
		c.zw = zw
	}
	c.ResponseWriter.WriteHeader(code)
}

func (c *compressWriter) Write(b []byte) (int, error) {
	if !c.decided {
		c.WriteHeader(http.StatusOK)
	}
	if c.zw != nil {
		return c.zw.Write(b)
	}
	return c.ResponseWriter.Write(b)
}

func (c *compressWriter) Flush() {
	if c.zw != nil {
		c.zw.Flush()
	}
	if fl, ok := c.ResponseWriter.(http.Flusher); ok {
		fl.Flush()
	}
}

// Close finishes the compressed stream.
func (c *compressWriter) Close() {
	if c.zw != nil {
		c.zw.Close()
		encPools[c.coding].Put(c.zw)
		c.zw = nil
	}
}
