package lb

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net"
	"net/http"
	"net/http/httputil"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	computev1 "google.golang.org/api/compute/v1"

	"github.com/linuxuser586/gcpemu/internal/emu"
)

// Backend services: endpoints from NEGs (refreshed continuously so GKE
// scaling shows within seconds, FR-INT-001), health (health.go), endpoint
// selection by locality LB policy and session affinity, timeouts, retries
// and outlier detection (FR-LB-005..008), and the backend transport:
// HTTP/1.1, HTTPS, HTTP/2 over TLS, H2C, gRPC passthrough and backend mTLS
// with Network Security backend authentication configs (FR-LB-006).

type healthState int32

const (
	stateUnknown healthState = iota
	stateHealthy
	stateUnhealthy
)

type endpoint struct {
	ip       string
	port     int
	addr     string
	instance string
	group    string // NEG path
	zone     string

	health    atomic.Int32
	succ      atomic.Int32
	fails     atomic.Int32
	draining  atomic.Bool
	drainAt   time.Time
	inflight  atomic.Int64
	consec    atomic.Int32 // consecutive errors (outlier detection)
	consecGW  atomic.Int32
	ejectedTo atomic.Int64 // unix nanos
	ejections atomic.Int32
}

func (e *endpoint) state() healthState { return healthState(e.health.Load()) }

type backendSvc struct {
	d    *dataplane
	path string

	cfg  atomic.Pointer[computev1.BackendService]
	eps  atomic.Pointer[[]*endpoint]
	hcfg atomic.Pointer[computev1.HealthCheck]
	rr   atomic.Uint64

	mu        sync.Mutex
	transport *http.Transport
	tkey      string
	proxy     *httputil.ReverseProxy
	hcStop    context.CancelFunc
	hcKick    chan struct{}
	stopped   bool

	refreshMu sync.Mutex

	authMu  sync.Mutex
	authAt  time.Time
	authCrt *tls.Certificate
	authCAs *x509.CertPool
	authErr error
}

func newBackendSvc(d *dataplane, path string) *backendSvc {
	b := &backendSvc{d: d, path: path, hcKick: make(chan struct{}, 1)}
	empty := []*endpoint{}
	b.eps.Store(&empty)
	b.proxy = &httputil.ReverseProxy{
		Rewrite:        b.rewrite,
		Transport:      b,
		ErrorHandler:   b.errorHandler,
		ModifyResponse: b.modifyResponse,
		ErrorLog:       log.New(io.Discard, "", 0),
	}
	ctx, cancel := context.WithCancel(d.ctx)
	b.hcStop = cancel
	go b.healthLoop(ctx)
	return b
}

func (b *backendSvc) conf() *computev1.BackendService { return b.cfg.Load() }

// configure installs a new resource version.
func (b *backendSvc) configure(c *computev1.BackendService) {
	b.cfg.Store(c)
	var hc *computev1.HealthCheck
	if len(c.HealthChecks) > 0 {
		if o, ok := b.d.s.load(kindHealthCheck, relPath(c.HealthChecks[0])); ok {
			hc = o.(*computev1.HealthCheck)
		}
	}
	b.hcfg.Store(hc)
	b.mu.Lock()
	key := c.Protocol + "|" + tlsKey(c.TlsSettings)
	if key != b.tkey {
		if b.transport != nil {
			b.transport.CloseIdleConnections()
		}
		b.transport = b.newTransport(c)
		b.tkey = key
		b.authAt = time.Time{}
	}
	b.mu.Unlock()
	b.refreshEndpoints(context.Background())
	b.kickHealth()
}

func tlsKey(t *computev1.BackendServiceTlsSettings) string {
	if t == nil {
		return ""
	}
	var sans []string
	for _, s := range t.SubjectAltNames {
		sans = append(sans, s.DnsName+"/"+s.UniformResourceIdentifier)
	}
	return t.AuthenticationConfig + "|" + t.Sni + "|" + strings.Join(sans, ",")
}

func (b *backendSvc) stop() {
	b.mu.Lock()
	b.stopped = true
	if b.hcStop != nil {
		b.hcStop()
	}
	if b.transport != nil {
		b.transport.CloseIdleConnections()
	}
	b.mu.Unlock()
}

func (b *backendSvc) kickHealth() {
	select {
	case b.hcKick <- struct{}{}:
	default:
	}
}

// refreshEndpoints re-reads the endpoints of every backend group: NEG
// endpoints, or an instance group's instances at the named port portName.
// Removed endpoints drain for connectionDraining.drainingTimeoutSec.
func (b *backendSvc) refreshEndpoints(ctx context.Context) {
	b.refreshMu.Lock()
	defer b.refreshMu.Unlock()
	c := b.conf()
	if c == nil {
		return
	}
	cur := *b.eps.Load()
	byAddr := map[string]*endpoint{}
	for _, e := range cur {
		byAddr[e.group+"|"+e.addr] = e
	}
	var next []*endpoint
	changed := false
	seen := map[string]bool{}
	for _, be := range c.Backends {
		g := relPath(be.Group)
		var eps []emu.NEGEndpoint
		var err error
		switch {
		case strings.Contains(g, "/networkEndpointGroups/"):
			eps, err = b.d.s.cmp.NEGEndpoints(ctx, g)
		case strings.Contains(g, "/instanceGroups/"):
			eps, err = b.d.s.cmp.InstanceGroupEndpoints(ctx, g, c.PortName)
		default:
			continue
		}
		if err != nil {
			continue
		}
		zone := ""
		if segs := strings.Split(g, "/"); len(segs) > 3 {
			zone = segs[3]
		}
		for _, ne := range eps {
			port := ne.Port
			if port == 0 {
				port = int(c.Port)
			}
			addr := net.JoinHostPort(ne.IP, strconv.Itoa(port))
			k := g + "|" + addr
			if seen[k] {
				continue
			}
			seen[k] = true
			e := byAddr[k]
			if e == nil {
				e = &endpoint{ip: ne.IP, port: port, addr: addr, instance: ne.Instance, group: g, zone: zone}
				if b.hcfg.Load() == nil {
					e.health.Store(int32(stateHealthy))
				}
				changed = true
			} else if e.draining.Load() {
				e.draining.Store(false)
				changed = true
			}
			next = append(next, e)
		}
	}
	drain := time.Duration(0)
	if c.ConnectionDraining != nil {
		drain = time.Duration(c.ConnectionDraining.DrainingTimeoutSec) * time.Second
	}
	now := time.Now()
	for _, e := range cur {
		if seen[e.group+"|"+e.addr] {
			continue
		}
		if !e.draining.Load() {
			e.draining.Store(true)
			e.drainAt = now
			changed = true
		}
		if now.Sub(e.drainAt) < drain {
			next = append(next, e)
		} else {
			changed = true
		}
	}
	if b.hcfg.Load() == nil {
		for _, e := range next {
			e.health.Store(int32(stateHealthy))
		}
	}
	if changed || len(next) != len(cur) {
		b.eps.Store(&next)
		b.kickHealth()
	}
}

// endpointLoop refreshes NEG endpoints of every backend service.
func (d *dataplane) endpointLoop(ctx context.Context) {
	t := time.NewTicker(300 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		cfg := d.cfg.Load()
		for _, b := range cfg.svcs {
			b.refreshEndpoints(ctx)
		}
	}
}

// --- request path ---

type rsKey struct{}
type actionKey struct{}

// handler returns the origin handler for one request.
func (b *backendSvc) handler(rs *reqState, action *computev1.HttpRouteAction) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := b.conf()
		timeout := time.Duration(c.TimeoutSec) * time.Second
		if action != nil && action.Timeout != nil {
			timeout = durationOf(action.Timeout)
		}
		ctx := context.WithValue(r.Context(), rsKey{}, rs)
		ctx = context.WithValue(ctx, actionKey{}, action)
		var cancel context.CancelFunc
		if timeout > 0 && r.Header.Get("Upgrade") == "" {
			ctx, cancel = context.WithTimeout(ctx, timeout)
			defer cancel()
		}
		if cookie := b.affinityCookie(r, rs); cookie != nil {
			if rw, ok := w.(*respWriter); ok {
				prev := rw.onHeader
				rw.onHeader = func(h http.Header) {
					h.Add("Set-Cookie", cookie.String())
					if prev != nil {
						prev(h)
					}
				}
			} else {
				http.SetCookie(w, cookie)
			}
		}
		b.proxy.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (b *backendSvc) rewrite(pr *httputil.ProxyRequest) {
	c := b.conf()
	pr.Out.URL.Scheme = "http"
	if c.Protocol == "HTTPS" || c.Protocol == "HTTP2" {
		pr.Out.URL.Scheme = "https"
	}
	pr.Out.URL.Host = "backend"
	for _, h := range []string{"X-Forwarded-For", "X-Forwarded-Proto"} {
		if v := pr.In.Header.Values(h); len(v) > 0 {
			pr.Out.Header[h] = v
		}
	}
	pr.Out.Host = pr.In.Host
}

func (b *backendSvc) modifyResponse(resp *http.Response) error {
	if rs, _ := resp.Request.Context().Value(rsKey{}).(*reqState); rs != nil {
		rs.details = "response_sent_by_backend"
	}
	return nil
}

var (
	errNoBackend = errors.New("no healthy upstream")
	errUpstream  = errors.New("upstream error")
)

func (b *backendSvc) errorHandler(w http.ResponseWriter, r *http.Request, err error) {
	rs, _ := r.Context().Value(rsKey{}).(*reqState)
	code, details, msg := http.StatusBadGateway, "failed_to_connect_to_backend", "upstream connect error or disconnect/reset before headers"
	var ne net.Error
	var oe *net.OpError
	switch {
	case errors.Is(err, errNoBackend):
		code, details, msg = http.StatusServiceUnavailable, "failed_to_pick_backend", "no healthy upstream"
	case errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()):
		code, details, msg = http.StatusGatewayTimeout, "backend_timeout", "upstream request timeout"
	case errors.Is(err, context.Canceled):
		code, details = 499, "client_disconnected_before_any_response"
	case errors.As(err, &oe) && oe.Op == "dial":
	default:
		details = "backend_connection_closed_before_data_sent_to_client"
	}
	if rs != nil {
		rs.details = details
	}
	if code == 499 {
		return
	}
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(code)
	_, _ = io.WriteString(w, msg)
}

// RoundTrip picks an endpoint per attempt and applies the retry policy and
// outlier detection.
func (b *backendSvc) RoundTrip(req *http.Request) (*http.Response, error) {
	rs, _ := req.Context().Value(rsKey{}).(*reqState)
	action, _ := req.Context().Value(actionKey{}).(*computev1.HttpRouteAction)
	conds := map[string]bool{"connect-failure": true}
	retries := 1
	var perTry time.Duration
	if action != nil && action.RetryPolicy != nil {
		rp := action.RetryPolicy
		conds = map[string]bool{}
		for _, c := range rp.RetryConditions {
			conds[c] = true
		}
		retries = int(rp.NumRetries)
		if retries == 0 {
			retries = 1
		}
		perTry = durationOf(rp.PerTryTimeout)
	}
	replayable := req.Body == nil || req.Body == http.NoBody || req.GetBody != nil
	tried := map[*endpoint]bool{}
	var lastErr error
	for attempt := 0; ; attempt++ {
		ep := b.pick(req, rs, tried)
		if ep == nil && attempt > 0 && len(tried) > 0 {
			// No other endpoint: retry the same set.
			ep = b.pick(req, rs, nil)
		}
		if ep == nil {
			if lastErr != nil {
				return nil, lastErr
			}
			return nil, errNoBackend
		}
		tried[ep] = true
		out := req
		if attempt > 0 {
			out = req.Clone(req.Context())
			if req.GetBody != nil {
				out.Body, _ = req.GetBody()
			}
		}
		u := *out.URL
		u.Host = ep.addr
		out.URL = &u
		var cancel context.CancelFunc
		if perTry > 0 {
			var ctx context.Context
			ctx, cancel = context.WithTimeout(out.Context(), perTry)
			out = out.WithContext(ctx)
		}
		ep.inflight.Add(1)
		t0 := time.Now()
		b.mu.Lock()
		t := b.transport
		b.mu.Unlock()
		resp, err := t.RoundTrip(out)
		if rs != nil {
			rs.backend, rs.bLat = ep.addr, time.Since(t0)
		}
		retry := false
		if err != nil {
			ep.inflight.Add(-1)
			if cancel != nil {
				cancel()
			}
			b.outlier(ep, 0, err)
			var oe *net.OpError
			isDial := errors.As(err, &oe) && oe.Op == "dial"
			retry = (isDial && conds["connect-failure"]) || conds["reset"] || conds["refused-stream"] && isDial ||
				(errors.Is(err, context.DeadlineExceeded) && perTry > 0 && req.Context().Err() == nil && (conds["deadline-exceeded"] || conds["5xx"] || conds["gateway-error"]))
			if !retry || attempt >= retries || !replayable {
				return nil, err
			}
			lastErr = err
			continue
		}
		b.outlier(ep, resp.StatusCode, nil)
		retry = retryStatus(conds, resp)
		if retry && attempt < retries && replayable {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
			resp.Body.Close()
			ep.inflight.Add(-1)
			if cancel != nil {
				cancel()
			}
			continue
		}
		resp.Body = &trackBody{ReadCloser: resp.Body, ep: ep, cancel: cancel}
		return resp, nil
	}
}

func retryStatus(conds map[string]bool, resp *http.Response) bool {
	c := resp.StatusCode
	switch {
	case conds["5xx"] && c >= 500:
		return true
	case conds["gateway-error"] && (c == 502 || c == 503 || c == 504):
		return true
	case conds["retriable-4xx"] && c == 409:
		return true
	}
	if g := resp.Header.Get("Grpc-Status"); g != "" {
		switch g {
		case "1":
			return conds["cancelled"]
		case "4":
			return conds["deadline-exceeded"]
		case "8":
			return conds["resource-exhausted"]
		case "13":
			return conds["internal"]
		case "14":
			return conds["unavailable"]
		}
	}
	return false
}

// trackBody decrements the endpoint's in-flight count when the response
// body is closed (LEAST_REQUEST).
type trackBody struct {
	io.ReadCloser
	ep     *endpoint
	cancel context.CancelFunc
	once   sync.Once
}

func (t *trackBody) Close() error {
	err := t.ReadCloser.Close()
	t.once.Do(func() {
		t.ep.inflight.Add(-1)
		if t.cancel != nil {
			t.cancel()
		}
	})
	return err
}

// Write supports upgraded (WebSocket) bodies.
func (t *trackBody) Write(p []byte) (int, error) {
	if w, ok := t.ReadCloser.(io.Writer); ok {
		return w.Write(p)
	}
	return 0, errors.New("body is not writable")
}

// outlier applies outlier detection (consecutive errors / gateway
// failures eject an endpoint for baseEjectionTime × ejections).
func (b *backendSvc) outlier(ep *endpoint, status int, err error) {
	od := b.conf().OutlierDetection
	if od == nil {
		return
	}
	isErr := err != nil || status >= 500
	isGW := err != nil || status == 502 || status == 503 || status == 504
	if !isErr {
		ep.consec.Store(0)
		ep.consecGW.Store(0)
		return
	}
	n := ep.consec.Add(1)
	g := int32(0)
	if isGW {
		g = ep.consecGW.Add(1)
	}
	limit, glimit := od.ConsecutiveErrors, od.ConsecutiveGatewayFailure
	if limit == 0 {
		limit = 5
	}
	if glimit == 0 {
		glimit = 3
	}
	enforce := od.EnforcingConsecutiveErrors
	if enforce == 0 {
		enforce = 100
	}
	if (int64(n) >= limit && rand.Int64N(100) < enforce) || (od.EnforcingConsecutiveGatewayFailure > 0 && int64(g) >= glimit) {
		eps := *b.eps.Load()
		max := od.MaxEjectionPercent
		if max == 0 {
			max = 50
		}
		ejected := 0
		now := time.Now().UnixNano()
		for _, e := range eps {
			if e.ejectedTo.Load() > now {
				ejected++
			}
		}
		if len(eps) > 0 && int64(ejected+1)*100 > max*int64(len(eps)) {
			return
		}
		base := durationOf(od.BaseEjectionTime)
		if base == 0 {
			base = 30 * time.Second
		}
		k := ep.ejections.Add(1)
		ep.ejectedTo.Store(time.Now().Add(base * time.Duration(k)).UnixNano())
		ep.consec.Store(0)
		ep.consecGW.Store(0)
	}
}

// usable reports whether an endpoint may receive new requests.
func (b *backendSvc) usable(e *endpoint, now time.Time) bool {
	if e.draining.Load() || e.state() != stateHealthy {
		return false
	}
	if e.ejectedTo.Load() > now.UnixNano() {
		return false
	}
	c := b.conf()
	for _, be := range c.Backends {
		if relPath(be.Group) == e.group {
			return be.CapacityScaler > 0
		}
	}
	return true
}

// pick selects an endpoint (FR-LB-008): session affinity hashes when set,
// otherwise the locality LB policy.
func (b *backendSvc) pick(r *http.Request, rs *reqState, tried map[*endpoint]bool) *endpoint {
	now := time.Now()
	all := *b.eps.Load()
	cands := make([]*endpoint, 0, len(all))
	for _, e := range all {
		if !tried[e] && b.usable(e, now) {
			cands = append(cands, e)
		}
	}
	if len(cands) == 0 {
		return nil
	}
	if len(cands) == 1 {
		return cands[0]
	}
	c := b.conf()
	if key := b.affinityKey(r, rs); key != "" {
		return rendezvous(cands, key)
	}
	switch c.LocalityLbPolicy {
	case "LEAST_REQUEST":
		a, z := cands[rand.IntN(len(cands))], cands[rand.IntN(len(cands))]
		if z.inflight.Load() < a.inflight.Load() {
			return z
		}
		return a
	case "RANDOM":
		return cands[rand.IntN(len(cands))]
	case "RING_HASH", "MAGLEV", "WEIGHTED_MAGLEV":
		return rendezvous(cands, rs.clientIP+r.URL.Path)
	}
	return cands[int(b.rr.Add(1)-1)%len(cands)]
}

// rendezvous is highest-random-weight hashing: stable as endpoints come
// and go, like a consistent hash ring.
func rendezvous(cands []*endpoint, key string) *endpoint {
	var best *endpoint
	var bestW uint64
	for _, e := range cands {
		h := sha256.Sum256([]byte(key + "|" + e.addr))
		w := binary.BigEndian.Uint64(h[:8])
		if best == nil || w > bestW {
			best, bestW = e, w
		}
	}
	return best
}

const gclbCookie = "GCLB"

// affinityKey derives the session affinity key of a request.
func (b *backendSvc) affinityKey(r *http.Request, rs *reqState) string {
	c := b.conf()
	switch c.SessionAffinity {
	case "CLIENT_IP", "CLIENT_IP_PROTO", "CLIENT_IP_PORT_PROTO", "CLIENT_IP_NO_DESTINATION":
		if rs != nil {
			return rs.clientIP
		}
	case "GENERATED_COOKIE", "STRONG_COOKIE_AFFINITY":
		name := gclbCookie
		if c.SessionAffinity == "STRONG_COOKIE_AFFINITY" && c.StrongSessionAffinityCookie != nil && c.StrongSessionAffinityCookie.Name != "" {
			name = c.StrongSessionAffinityCookie.Name
		}
		if ck, err := r.Cookie(name); err == nil {
			return ck.Value
		}
		return r.Header.Get("X-Gcpemu-Affinity")
	case "HEADER_FIELD":
		if c.ConsistentHash != nil {
			return r.Header.Get(c.ConsistentHash.HttpHeaderName)
		}
	case "HTTP_COOKIE":
		if c.ConsistentHash != nil && c.ConsistentHash.HttpCookie != nil {
			if ck, err := r.Cookie(c.ConsistentHash.HttpCookie.Name); err == nil {
				return ck.Value
			}
			return r.Header.Get("X-Gcpemu-Affinity")
		}
	}
	return ""
}

// affinityCookie generates the affinity cookie for a request that has none
// (GENERATED_COOKIE "GCLB", HTTP_COOKIE), stashing its value for pick.
func (b *backendSvc) affinityCookie(r *http.Request, rs *reqState) *http.Cookie {
	c := b.conf()
	var name, path string
	var ttl int64
	switch c.SessionAffinity {
	case "GENERATED_COOKIE":
		name, ttl = gclbCookie, c.AffinityCookieTtlSec
	case "STRONG_COOKIE_AFFINITY":
		name = gclbCookie
		if sc := c.StrongSessionAffinityCookie; sc != nil {
			if sc.Name != "" {
				name = sc.Name
			}
			path, ttl = sc.Path, int64(durationOf(sc.Ttl)/time.Second)
		}
	case "HTTP_COOKIE":
		if c.ConsistentHash == nil || c.ConsistentHash.HttpCookie == nil {
			return nil
		}
		hc := c.ConsistentHash.HttpCookie
		name, path, ttl = hc.Name, hc.Path, int64(durationOf(hc.Ttl)/time.Second)
	default:
		return nil
	}
	if _, err := r.Cookie(name); err == nil {
		return nil
	}
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], rand.Uint64())
	v := hex.EncodeToString(buf[:])
	r.Header.Set("X-Gcpemu-Affinity", v)
	ck := &http.Cookie{Name: name, Value: v, Path: path, HttpOnly: true}
	if ck.Path == "" {
		ck.Path = "/"
	}
	if ttl > 0 {
		ck.MaxAge = int(ttl)
	}
	return ck
}

// --- transport ---

func (b *backendSvc) newTransport(c *computev1.BackendService) *http.Transport {
	t := &http.Transport{
		DialContext:           b.d.dialContext,
		MaxIdleConns:          1024,
		MaxIdleConnsPerHost:   256,
		IdleConnTimeout:       600 * time.Second,
		ResponseHeaderTimeout: 0,
		DisableCompression:    true,
		ExpectContinueTimeout: time.Second,
	}
	switch c.Protocol {
	case "HTTPS", "HTTP2":
		t.DialTLSContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			return b.dialTLS(ctx, network, addr, c.Protocol == "HTTP2")
		}
		if c.Protocol == "HTTP2" {
			t.ForceAttemptHTTP2 = true
		} else {
			t.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
		}
	case "H2C", "GRPC":
		p := new(http.Protocols)
		p.SetUnencryptedHTTP2(true)
		t.Protocols = p
	}
	return t
}

// dialTLS connects to a TLS backend, presenting the backend
// authentication client certificate and verifying the server against its
// trust config (FR-LB-006); without an authentication config the
// connection is encrypted but not verified, as on GCP.
func (b *backendSvc) dialTLS(ctx context.Context, network, addr string, h2 bool) (net.Conn, error) {
	c := b.conf()
	raw, err := b.d.dialContext(ctx, network, addr)
	if err != nil {
		return nil, err
	}
	cfg := &tls.Config{InsecureSkipVerify: true, NextProtos: []string{"http/1.1"}}
	if h2 {
		cfg.NextProtos = []string{"h2", "http/1.1"}
	}
	ts := c.TlsSettings
	if ts != nil && ts.Sni != "" {
		cfg.ServerName = ts.Sni
	} else if rs, _ := ctx.Value(rsKey{}).(*reqState); rs != nil {
		cfg.ServerName = stripPortHost(rs.req.Host)
	}
	if ts != nil && ts.AuthenticationConfig != "" {
		crt, roots, err := b.backendAuth(ctx, ts.AuthenticationConfig)
		if err != nil {
			raw.Close()
			return nil, err
		}
		if crt != nil {
			cfg.Certificates = []tls.Certificate{*crt}
		}
		if roots == nil || !emptyPool(roots) {
			sans, sni := ts.SubjectAltNames, ts.Sni
			cfg.VerifyConnection = func(st tls.ConnectionState) error {
				return verifyBackend(st, roots, sni, sans)
			}
		}
	}
	tc := tls.Client(raw, cfg)
	if err := tc.HandshakeContext(ctx); err != nil {
		raw.Close()
		return nil, err
	}
	return tc, nil
}

func stripPortHost(h string) string {
	if host, _, err := net.SplitHostPort(h); err == nil {
		return host
	}
	return h
}

func emptyPool(p *x509.CertPool) bool { return p.Equal(x509.NewCertPool()) }

// verifyBackend validates the backend's chain against roots (nil = system
// roots) and, when subjectAltNames are configured, that one of them is in
// the certificate; otherwise a configured tlsSettings.sni must match the
// certificate. An SNI taken from the request Host is not checked.
func verifyBackend(st tls.ConnectionState, roots *x509.CertPool, sni string, sans []*computev1.BackendServiceTlsSettingsSubjectAltName) error {
	if len(st.PeerCertificates) == 0 {
		return errors.New("backend presented no certificate")
	}
	leaf := st.PeerCertificates[0]
	inter := x509.NewCertPool()
	for _, c := range st.PeerCertificates[1:] {
		inter.AddCert(c)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots, Intermediates: inter}); err != nil {
		return err
	}
	if len(sans) > 0 {
		for _, s := range sans {
			if s.DnsName != "" && leaf.VerifyHostname(s.DnsName) == nil {
				return nil
			}
			for _, u := range leaf.URIs {
				if s.UniformResourceIdentifier != "" && u.String() == s.UniformResourceIdentifier {
					return nil
				}
			}
		}
		return errors.New("backend certificate matches none of tlsSettings.subjectAltNames")
	}
	if sni != "" {
		if err := leaf.VerifyHostname(sni); err != nil {
			return fmt.Errorf("backend certificate does not match tlsSettings.sni: %w", err)
		}
	}
	return nil
}

// backendAuth resolves (and caches briefly) the backend authentication config.
func (b *backendSvc) backendAuth(ctx context.Context, name string) (*tls.Certificate, *x509.CertPool, error) {
	b.authMu.Lock()
	defer b.authMu.Unlock()
	if time.Since(b.authAt) < 5*time.Second {
		return b.authCrt, b.authCAs, b.authErr
	}
	cm := b.d.s.certManager()
	if cm == nil {
		b.authErr = errors.New("backend authentication requires the certs service")
	} else {
		b.authCrt, b.authCAs, b.authErr = cm.BackendAuthentication(ctx, strings.TrimPrefix(name, netSecPrefix))
	}
	b.authAt = time.Now()
	return b.authCrt, b.authCAs, b.authErr
}
