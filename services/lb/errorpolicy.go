package lb

import (
	"io"
	"net/http"
	"strconv"

	computev1 "google.golang.org/api/compute/v1"

	"github.com/linuxuser586/gcpemu/services/cdn"
)

// Custom error responses (FR-LB-012): on a global external Application
// Load Balancer, a backend status that the URL map's
// customErrorResponsePolicy lists is answered with the rule's page from the
// policy's errorService, with overrideResponseCode as the status. The
// handler sits inside Cloud CDN, so the cache stores the error page.

// detailsFetchFailed is the statusDetails of a request whose error page
// could not be fetched. GCP doesn't name one; the emulator made it up.
const detailsFetchFailed = "custom_error_response_fetch_failed"

// errorFetchDrop are the client headers an error page fetch leaves out.
var errorFetchDrop = []string{"Range", "If-None-Match", "If-Modified-Since", "If-Match", "If-Unmodified-Since", "If-Range"}

// errorPageHeaders are the error page's headers the client gets; the
// backend's own headers are discarded.
var errorPageHeaders = []string{"Content-Type", "Content-Encoding", "Content-Language", "Content-Disposition",
	"Cache-Control", "Expires", "Etag", "Last-Modified"}

// customErrors reports whether fe serves custom error responses: only
// global EXTERNAL_MANAGED forwarding rules do.
func (fe *frontend) customErrors() bool {
	return fe.region == "" && fe.fr.LoadBalancingScheme == "EXTERNAL_MANAGED"
}

// errorPolicy wraps origin, the matched backend's handler, with the
// route's custom error response policies. Requests with Authorization and
// protocol switches pass through, as GCP doesn't support them.
func (d *dataplane) errorPolicy(cfg *config, rs *reqState, origin http.Handler) http.Handler {
	r := rs.req
	if !rs.fe.customErrors() || rs.route == nil || len(rs.route.ErrorPolicies) == 0 ||
		r.Header.Get("Authorization") != "" || r.Header.Get("Upgrade") != "" {
		return origin
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ew := &errorWriter{ResponseWriter: w, d: d, cfg: cfg, rs: rs, r: r, header: http.Header{}}
		origin.ServeHTTP(ew, r)
		if ew.state == errUndecided {
			// Nothing written (a client that went away): leave the
			// headers as the origin set them.
			for k, v := range ew.header {
				w.Header()[k] = v
			}
		}
	})
}

const (
	errUndecided = iota
	errPassed    // the backend's response goes to the client
	errReplaced  // the error page was served; the backend's body is dropped
)

// errorWriter holds back the backend's header until its status is known,
// then passes the response through or replaces it with the error page.
type errorWriter struct {
	http.ResponseWriter
	d      *dataplane
	cfg    *config
	rs     *reqState
	r      *http.Request
	header http.Header
	state  int
}

func (w *errorWriter) Header() http.Header {
	if w.state == errPassed {
		return w.ResponseWriter.Header()
	}
	return w.header
}

func (w *errorWriter) WriteHeader(code int) {
	switch {
	case w.state == errPassed:
		w.ResponseWriter.WriteHeader(code)
		return
	case w.state == errReplaced, code < 200:
		return
	}
	// Only statuses from the backend count, never the data plane's own
	// answers (no healthy upstream, timeouts, ...).
	rule, svc := w.rs.route.ErrorRule(code)
	if rule == nil || w.rs.details != "response_sent_by_backend" {
		w.state = errPassed
		h := w.ResponseWriter.Header()
		for k, v := range w.header {
			h[k] = v
		}
		w.ResponseWriter.WriteHeader(code)
		return
	}
	w.state = errReplaced
	w.d.serveErrorPage(w.ResponseWriter, w.cfg, w.rs, w.r, code, rule, svc)
}

func (w *errorWriter) Write(b []byte) (int, error) {
	if w.state == errUndecided {
		w.WriteHeader(http.StatusOK)
	}
	if w.state == errReplaced {
		return len(b), nil
	}
	return w.ResponseWriter.Write(b)
}

func (w *errorWriter) Flush() {
	if w.state == errUndecided {
		w.WriteHeader(http.StatusOK)
	}
	if w.state == errPassed {
		_ = http.NewResponseController(w.ResponseWriter).Flush()
	}
}

func (w *errorWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// serveErrorPage answers r, whose backend replied status, with rule's
// page from errorService, or with a plain-text error when the page can't
// be fetched.
func (d *dataplane) serveErrorPage(w http.ResponseWriter, cfg *config, rs *reqState, r *http.Request, status int,
	rule *computev1.CustomErrorResponsePolicyCustomErrorResponseRule, errorService string) {
	h := w.Header()
	page := d.fetchErrorPage(cfg, rs, r, rule.Path, errorService)
	if page == nil {
		rs.details = detailsFetchFailed
		h.Set("Content-Type", "text/plain; charset=utf-8")
		h.Set("Cache-Control", "no-store")
		// An outage is never cached, even by FORCE_CACHE_ALL.
		cdn.NoStore(w)
		w.WriteHeader(status)
		if r.Method != http.MethodHead {
			_, _ = io.WriteString(w, strconv.Itoa(status)+" "+http.StatusText(status)+"\n")
		}
		return
	}
	for _, k := range errorPageHeaders {
		if v := page.header.Values(k); len(v) > 0 {
			h[k] = v
		}
	}
	if r.Method == http.MethodHead {
		if v := page.header.Get("Content-Length"); v != "" {
			h.Set("Content-Length", v)
		}
	} else {
		h.Set("Content-Length", strconv.Itoa(len(page.body)))
	}
	if c := int(rule.OverrideResponseCode); c != 0 {
		status = c
		rs.override = c
	}
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		_, _ = w.Write(page.body)
	}
}

// fetchErrorPage gets path from errorService, a backend bucket or backend
// service, straight from its origin (never its Cloud CDN cache). The fetch
// is a GET (HEAD for HEAD) without query, body, Range or conditional
// headers. It returns nil unless the service answers 2xx.
func (d *dataplane) fetchErrorPage(cfg *config, rs *reqState, r *http.Request, path, errorService string) *pageRecorder {
	// The fetch has its own request state so that it doesn't overwrite
	// the matched backend's in the access log.
	frs := &reqState{fe: rs.fe, req: rs.req, start: rs.start, sw: rs.sw, clientIP: rs.clientIP, clientPort: rs.clientPort, route: rs.route}
	var origin http.Handler
	p := relPath(errorService)
	switch collOf(p) {
	case "backendServices":
		if bs := cfg.svcs[p]; bs != nil {
			origin = bs.handler(frs, nil)
		}
	case "backendBuckets":
		if b := cfg.buckets[p]; b != nil {
			origin = d.bucketHandler(frs, b)
		}
	}
	if origin == nil {
		return nil
	}
	fr := r.Clone(r.Context())
	if fr.Method != http.MethodHead {
		fr.Method = http.MethodGet
	}
	u := *r.URL
	u.Path, u.RawPath, u.RawQuery = path, "", ""
	fr.URL = &u
	fr.Body, fr.GetBody, fr.ContentLength = http.NoBody, nil, 0
	for _, k := range errorFetchDrop {
		fr.Header.Del(k)
	}
	rec := &pageRecorder{header: http.Header{}}
	origin.ServeHTTP(rec, fr)
	if rec.status == 0 {
		rec.status = http.StatusOK
	}
	if rec.status < 200 || rec.status > 299 {
		return nil
	}
	return rec
}

// pageRecorder buffers an error page.
type pageRecorder struct {
	header http.Header
	status int
	body   []byte
}

func (p *pageRecorder) Header() http.Header { return p.header }

func (p *pageRecorder) WriteHeader(code int) {
	if p.status == 0 && code >= 200 {
		p.status = code
	}
}

func (p *pageRecorder) Write(b []byte) (int, error) {
	if p.status == 0 {
		p.status = http.StatusOK
	}
	p.body = append(p.body, b...)
	return len(b), nil
}
