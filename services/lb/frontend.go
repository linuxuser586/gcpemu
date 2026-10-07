package lb

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"math/rand/v2"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	computev1 "google.golang.org/api/compute/v1"

	"github.com/linuxuser586/gcpemu/services/cdn"
	"github.com/linuxuser586/gcpemu/services/lb/urlmap"
)

// The request pipeline of a frontend: TLS termination with SNI/certificate
// map selection and SSL policy (FR-LB-004, FR-LB-009), URL map routing
// (FR-LB-003), redirects, header actions with variables, then the backend
// service or bucket, optionally behind Cloud CDN (FR-CDN-001), and an
// access log entry per request (FR-LB-010).

// reqState carries per-request facts for header variables and logs.
type reqState struct {
	fe         *frontend
	req        *http.Request
	start      time.Time
	clientIP   string
	clientPort string
	certErr    string

	service string // backend service/bucket path
	kind    string // "backendService" or "backendBucket"
	details string // statusDetails
	backend string // backend endpoint ip:port
	bLat    time.Duration

	logEnable bool
	sample    float64

	// CDN.
	cdnOn             bool
	cdnStatus         string // final value from cdn.Serve
	originCalled      atomic.Bool
	originDone        atomic.Bool
	originStatus      atomic.Int64
	clientConditional bool
}

// cacheStatusNow infers {cdn_cache_status} at the moment the response
// header is written (cdn.Serve reports its verdict only afterwards): no
// origin fetch means a hit; an origin 304 to the cache's own conditional
// request means revalidated; an origin response completed before the client
// header means the cache stored it (miss); otherwise it streamed through
// uncached.
func (rs *reqState) cacheStatusNow() string {
	if rs.cdnStatus != "" {
		return rs.cdnStatus
	}
	if !rs.cdnOn {
		return "disabled"
	}
	switch {
	case !rs.originCalled.Load():
		return cdn.StatusHit
	case rs.originStatus.Load() == http.StatusNotModified && !rs.clientConditional:
		return cdn.StatusRevalidated
	case rs.originDone.Load():
		return cdn.StatusMiss
	}
	return cdn.StatusUncacheable
}

// respWriter applies response header actions when the header is written
// and records status and size.
type respWriter struct {
	http.ResponseWriter
	rs       *reqState
	status   int
	bytes    int64
	wrote    bool
	onHeader func(h http.Header)
}

func (w *respWriter) WriteHeader(code int) {
	if w.wrote {
		return
	}
	if code >= 100 && code < 200 && code != http.StatusSwitchingProtocols {
		w.ResponseWriter.WriteHeader(code)
		return
	}
	w.wrote, w.status = true, code
	if w.onHeader != nil {
		w.onHeader(w.Header())
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *respWriter) Write(b []byte) (int, error) {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, err
}

func (w *respWriter) Flush() {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	_ = http.NewResponseController(w.ResponseWriter).Flush()
}

func (w *respWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return http.NewResponseController(w.ResponseWriter).Hijack()
}

func (w *respWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

type connKey struct{}

// connContext records the connection for TLS details.
func connContext(ctx context.Context, c net.Conn) context.Context {
	return context.WithValue(ctx, connKey{}, c)
}

// handler serves one frontend (looked up per request so configuration
// changes apply without rebinding).
func (d *dataplane) handler(key string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cfg := d.cfg.Load()
		fe := cfg.fronts[key]
		if fe == nil {
			http.Error(w, "no forwarding rule", http.StatusServiceUnavailable)
			return
		}
		rs := &reqState{fe: fe, req: r, start: time.Now()}
		rs.clientIP, rs.clientPort, _ = net.SplitHostPort(r.RemoteAddr)
		rw := &respWriter{ResponseWriter: w, rs: rs}
		d.serve(cfg, fe, rw, r, rs)
		if !rw.wrote {
			rw.WriteHeader(http.StatusOK)
		}
		d.logs.log(rs, rw)
	})
}

// serve routes and dispatches a request.
func (d *dataplane) serve(cfg *config, fe *frontend, w *respWriter, r *http.Request, rs *reqState) {
	if fe.mtlsMode != "" && r.TLS != nil {
		rs.certErr = verifyClientChain(r.TLS, fe.mtlsRoots)
	}
	if fe.router == nil {
		rs.details = "invalid_url_map"
		http.Error(w, "The URL map of this load balancer is missing or invalid.", http.StatusBadGateway)
		return
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	host := r.Host
	res := fe.router.Route(&urlmap.Request{Scheme: scheme, Method: r.Method, Host: host, Path: r.URL.Path,
		RawQuery: r.URL.RawQuery, Header: r.Header}, nil)
	if res.Redirect != nil {
		rs.details = "redirected_by_load_balancer"
		w.onHeader = func(h http.Header) { applyResponseActions(h, res.HeaderActions, rs) }
		w.Header().Set("Location", res.Redirect.Location)
		w.Header().Set("Content-Type", "text/html; charset=UTF-8")
		w.WriteHeader(res.Redirect.Code)
		if r.Method != http.MethodHead {
			_, _ = w.Write([]byte("<HTML><HEAD><meta http-equiv=\"content-type\" content=\"text/html;charset=utf-8\">\n<TITLE>Redirect</TITLE></HEAD><BODY>\n<A HREF=\"" + res.Redirect.Location + "\">here</A>.\r\n</BODY></HTML>\r\n"))
		}
		return
	}
	rs.service = relPath(res.Service)
	// Fault injection (routeAction.faultInjectionPolicy).
	if a := res.Action; a != nil && a.FaultInjectionPolicy != nil {
		fi := a.FaultInjectionPolicy
		if fi.Delay != nil && fi.Delay.FixedDelay != nil && rand.Float64()*100 < fi.Delay.Percentage {
			select {
			case <-time.After(durationOf(fi.Delay.FixedDelay)):
			case <-r.Context().Done():
				return
			}
		}
		if fi.Abort != nil && rand.Float64()*100 < fi.Abort.Percentage {
			rs.details = "fault_filter_abort"
			http.Error(w, "fault filter abort", int(fi.Abort.HttpStatus))
			return
		}
	}
	out := r.Clone(r.Context())
	out.Host = res.Host
	if res.Path != r.URL.Path {
		u := *r.URL
		u.Path, u.RawPath = res.Path, ""
		out.URL = &u
	}
	out.RequestURI = ""
	h := out.Header
	if prior := h.Get("X-Forwarded-For"); prior != "" {
		h.Set("X-Forwarded-For", prior+", "+rs.clientIP+","+fe.ip)
	} else {
		h.Set("X-Forwarded-For", rs.clientIP+","+fe.ip)
	}
	h.Set("X-Forwarded-Proto", scheme)
	h.Add("Via", "1.1 google")
	if h.Get("X-Cloud-Trace-Context") == "" {
		h.Set("X-Cloud-Trace-Context", traceID()+"/"+itoa(int(rand.Uint32()))+";o=0")
	}
	applyRequestActions(h, res.HeaderActions, rs)
	rs.clientConditional = r.Header.Get("If-None-Match") != "" || r.Header.Get("If-Modified-Since") != ""

	switch collOf(rs.service) {
	case "backendServices":
		bs := cfg.svcs[rs.service]
		if bs == nil {
			rs.details = "failed_to_pick_backend"
			http.Error(w, "no healthy upstream", http.StatusServiceUnavailable)
			return
		}
		rs.kind = "backendService"
		conf := bs.conf()
		applyCustomHeaders(h, conf.CustomRequestHeaders, rs)
		w.onHeader = func(rh http.Header) {
			applyCustomHeaders(rh, conf.CustomResponseHeaders, rs)
			applyResponseActions(rh, res.HeaderActions, rs)
		}
		rs.logEnable = conf.LogConfig != nil && conf.LogConfig.Enable
		if rs.logEnable {
			rs.sample = conf.LogConfig.SampleRate
		}
		origin := bs.handler(rs, res.Action)
		if conf.EnableCDN {
			if c := d.s.cdnCache(); c != nil {
				rs.cdnOn = true
				rs.cdnStatus = c.Serve(w, out, cdn.Backend{ID: conf.SelfLink, Kind: "backendService", ServicePolicy: cdnServicePolicy(conf.CdnPolicy),
					CompressionMode: conf.CompressionMode, SignedURLKeys: d.s.SignedURLKeys(conf.SelfLink)}, d.originTracker(rs, origin))
				return
			}
		}
		origin.ServeHTTP(w, out)
	case "backendBuckets":
		b := cfg.buckets[rs.service]
		if b == nil {
			rs.details = "failed_to_pick_backend"
			http.Error(w, "no healthy upstream", http.StatusServiceUnavailable)
			return
		}
		rs.kind = "backendBucket"
		rs.logEnable, rs.sample = true, 1
		w.onHeader = func(rh http.Header) {
			applyCustomHeaders(rh, b.CustomResponseHeaders, rs)
			applyResponseActions(rh, res.HeaderActions, rs)
		}
		origin := d.bucketHandler(rs, b)
		if b.EnableCdn {
			if c := d.s.cdnCache(); c != nil {
				rs.cdnOn = true
				rs.cdnStatus = c.Serve(w, out, cdn.Backend{ID: b.SelfLink, Kind: "backendBucket", BucketPolicy: cdnBucketPolicy(b.CdnPolicy),
					CompressionMode: b.CompressionMode, SignedURLKeys: d.s.SignedURLKeys(b.SelfLink)}, d.originTracker(rs, origin))
				return
			}
		}
		origin.ServeHTTP(w, out)
	default:
		rs.details = "failed_to_pick_backend"
		http.Error(w, "no backend", http.StatusServiceUnavailable)
	}
}

// originTracker records origin activity for {cdn_cache_status}.
func (d *dataplane) originTracker(rs *reqState, origin http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rs.originCalled.Store(true)
		ow := &statusTap{ResponseWriter: w, rs: rs}
		origin.ServeHTTP(ow, r)
		rs.originDone.Store(true)
	})
}

type statusTap struct {
	http.ResponseWriter
	rs *reqState
}

func (t *statusTap) WriteHeader(code int) {
	if code >= 200 {
		t.rs.originStatus.Store(int64(code))
	}
	t.ResponseWriter.WriteHeader(code)
}

func (t *statusTap) Flush() { _ = http.NewResponseController(t.ResponseWriter).Flush() }

func (t *statusTap) Unwrap() http.ResponseWriter { return t.ResponseWriter }

func durationOf(d *computev1.Duration) time.Duration {
	if d == nil {
		return 0
	}
	return time.Duration(d.Seconds)*time.Second + time.Duration(d.Nanos)
}

func traceID() string {
	const hexd = "0123456789abcdef"
	b := make([]byte, 32)
	for i := range b {
		b[i] = hexd[rand.IntN(16)]
	}
	return string(b)
}

// --- TLS ---

// tlsConfig selects the certificate for a handshake (SNI across the
// proxy's certificates, or the certificate map) and applies the SSL policy
// and server TLS policy.
func (d *dataplane) tlsConfig(key string, hello *tls.ClientHelloInfo) (*tls.Config, error) {
	fe := d.cfg.Load().fronts[key]
	if fe == nil {
		return nil, errors.New("no forwarding rule")
	}
	cfg := &tls.Config{NextProtos: []string{"h2", "http/1.1"}}
	applySSLPolicy(cfg, fe.policy)
	var cert *tls.Certificate
	if fe.certMap != "" {
		if cm := d.s.certManager(); cm != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			c, err := cm.MapCertificate(ctx, fe.certMap, strings.ToLower(hello.ServerName))
			cancel()
			if err != nil {
				return nil, err
			}
			cert = c
		}
	}
	if cert == nil {
		cert = selectCert(fe.certs, hello.ServerName)
	}
	if cert == nil {
		return nil, errors.New("no certificate available")
	}
	cfg.Certificates = []tls.Certificate{*cert}
	switch fe.mtlsMode {
	case "REJECT_INVALID":
		cfg.ClientAuth, cfg.ClientCAs = tls.RequireAndVerifyClientCert, fe.mtlsRoots
		if fe.mtlsRoots == nil {
			cfg.ClientCAs = x509.NewCertPool()
		}
	case "ALLOW_INVALID_OR_MISSING_CLIENT_CERT":
		cfg.ClientAuth = tls.RequestClientCert
	}
	return cfg, nil
}

// selectCert picks the certificate for sni: an exact SAN match, then a
// wildcard match, else the first (primary) certificate, as GCP does.
func selectCert(certs []*tls.Certificate, sni string) *tls.Certificate {
	if len(certs) == 0 {
		return nil
	}
	sni = strings.ToLower(strings.TrimSuffix(sni, "."))
	if sni != "" {
		for _, c := range certs {
			if c.Leaf == nil {
				continue
			}
			for _, n := range c.Leaf.DNSNames {
				if strings.EqualFold(n, sni) {
					return c
				}
			}
		}
		for _, c := range certs {
			if c.Leaf == nil {
				continue
			}
			for _, n := range c.Leaf.DNSNames {
				if strings.HasPrefix(n, "*.") {
					suffix := strings.ToLower(n[1:])
					if strings.HasSuffix(sni, suffix) && !strings.Contains(sni[:len(sni)-len(suffix)], ".") {
						return c
					}
				}
			}
		}
	}
	return certs[0]
}

// verifyClientChain validates a client certificate chain against roots
// (ALLOW_INVALID_OR_MISSING_CLIENT_CERT mode) and returns the
// {client_cert_error} value ("" when valid).
func verifyClientChain(st *tls.ConnectionState, roots *x509.CertPool) string {
	if len(st.PeerCertificates) == 0 {
		return "client_cert_not_provided"
	}
	if len(st.VerifiedChains) > 0 {
		return ""
	}
	if roots == nil {
		return "client_cert_validation_not_performed"
	}
	inter := x509.NewCertPool()
	for _, c := range st.PeerCertificates[1:] {
		inter.AddCert(c)
	}
	_, err := st.PeerCertificates[0].Verify(x509.VerifyOptions{Roots: roots, Intermediates: inter, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}})
	if err != nil {
		var ie x509.CertificateInvalidError
		if errors.As(err, &ie) && ie.Reason == x509.Expired {
			return "client_cert_validation_failed"
		}
		return "client_cert_chain_invalid"
	}
	return ""
}
