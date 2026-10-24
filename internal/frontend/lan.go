package frontend

import (
	"bytes"
	"context"
	"crypto/tls"
	stdjson "encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/emu"
)

// --gfe-bind (Config.GFEBind): the Google front end also serves on port
// 443 (--port gfe-lan=N) of each configured address, so that a machine on
// the LAN whose DNS sends storage.googleapis.com, oauth2.googleapis.com,
// LOCATION-docker.pkg.dev, ... there and which trusts the instance CA
// reaches the Instance unmodified (ADR 0021). Their Service endpoint is
// recorded as "gfe-lan".
//
// Unlike the loopback listener, which pipes bytes to the gateway, these
// listeners terminate HTTP so that every request can be checked, whatever
// the IAM enforcement level:
//
//   - LOCATION-docker.pkg.dev goes to Artifact Registry's --ar-bind
//     handler, which requires credentials itself (ADR 0011).
//   - oauth2.googleapis.com/token takes only the JWT-bearer grant, whose
//     assertion a service-account key signs, in a form or JSON body and
//     with no query. The refresh-token grant names its principal
//     unverified, so it is refused.
//   - A read of a bucket the Instance doesn't have that carries none of
//     the Instance's credentials, such as the Go module proxy's
//     downloads, is read through from real GCS. Their signed URLs are
//     Google's own and go with them.
//   - Everything else needs a bearer token the Instance issued, or for
//     the XML API a signature Cloud Storage verifies.
//
// The Admin API and the Web console are never served there.
//
// On port 443, which an unprivileged emulator on Linux may not bind, the
// LAN front-end relay can listen instead and relay to the same handler
// (lanrelay.go).

// LANEndpoint is the --gfe-bind listeners' Service endpoint name in
// emu.Endpoints.
const LANEndpoint = "gfe-lan"

const (
	storageHost = "storage.googleapis.com"
	oauth2Host  = "oauth2.googleapis.com"
	jwtGrant    = "urn:ietf:params:oauth:grant-type:jwt-bearer"
)

// LAN is where the --gfe-bind listeners send the requests they admit.
type LAN struct {
	// Gateway serves every real hostname but the registry's.
	Gateway http.Handler
	// Registry returns Artifact Registry's authenticating handler, or nil
	// while it isn't running.
	Registry func() http.Handler
	// Storage classifies storage.googleapis.com requests; nil without
	// Cloud Storage.
	Storage Storage
	// ReadThrough serves reads of buckets the Instance doesn't have
	// (NewReadThrough); nil sends them through the credentials check like
	// any other.
	ReadThrough http.Handler
}

// Storage is what the LAN listeners ask Cloud Storage.
type Storage interface {
	// ForeignBucket reports whether r reads a bucket the Instance doesn't have.
	ForeignBucket(r *http.Request) bool
	// Signed reports whether r carries a signature Cloud Storage verifies.
	Signed(r *http.Request) bool
}

// StartLAN serves lan on every --gfe-bind address. An address whose port
// 443 the emulator may not bind is relayed by the LAN front-end relay when
// it can be (lanrelay.go); any other failed address is logged and skipped.
func (f *Frontend) StartLAN(lan LAN) {
	binds := f.env.Config.GFEBind
	if len(binds) == 0 {
		return
	}
	port, set := f.env.Config.PortSet(LANEndpoint)
	if !set {
		port = 443
	}
	srv := f.lanServer(&lanHandler{f: f, lan: lan})
	var relayed []string
	containerized := sync.OnceValue(f.relayContainerized)
	for _, ip := range binds {
		addr := net.JoinHostPort(ip, strconv.Itoa(port))
		l, err := f.listen("tcp", addr)
		if err != nil {
			if !set && f.relayable(err, containerized) {
				relayed = append(relayed, addr)
				continue
			}
			f.lanSkipped(addr, err)
			continue
		}
		f.serveLAN(srv(), l, "direct", l.Addr().String())
	}
	if len(relayed) > 0 {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		f.mu.Lock()
		closed := f.closed
		if !closed {
			f.relayStop, f.relayDone = cancel, done
		}
		f.mu.Unlock()
		if closed {
			cancel()
			return
		}
		go func() {
			defer close(done)
			defer cancel()
			f.startRelay(ctx, srv, relayed)
		}()
	}
}

// lanServer returns a function making the http.Server of one --gfe-bind
// listener, serving h with the TLS configuration they share.
func (f *Frontend) lanServer(h http.Handler) func() *http.Server {
	cfg := &tls.Config{
		MinVersion: tls.VersionTLS12,
		NextProtos: []string{"h2", "http/1.1"},
		GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
			if hello.ServerName == "" {
				return nil, errors.New("Google front end: a LAN client must name the host it wants")
			}
			return f.certificate(hello)
		},
	}
	return func() *http.Server {
		return &http.Server{
			Handler:           h,
			TLSConfig:         cfg,
			ReadHeaderTimeout: 30 * time.Second,
			ErrorLog:          slog.NewLogLogger(f.env.Log.Handler(), slog.LevelDebug),
		}
	}
}

// serveLAN serves srv on l, which serves the --gfe-bind addresses addrs
// (mode "direct" or "relay"), and records them as the gfe-lan endpoint.
// It reports false, having served nothing, once the frontend is closed.
func (f *Frontend) serveLAN(srv *http.Server, l net.Listener, mode string, addrs ...string) bool {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		l.Close()
		return false
	}
	f.lan = append(f.lan, srv)
	f.lanAddrs = append(f.lanAddrs, addrs...)
	sort.Strings(f.lanAddrs)
	f.env.Endpoints.Set(LANEndpoint, strings.Join(f.lanAddrs, ","))
	f.mu.Unlock()
	go func() {
		if err := srv.ServeTLS(l, "", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
			f.env.Log.Error("Google front end stopped on --gfe-bind address", "address", l.Addr().String(), "err", err)
		}
	}()
	for _, a := range addrs {
		f.env.Log.Info("Google front end exposed", "address", a, "mode", mode)
	}
	return true
}

// lanSkipped logs a --gfe-bind address (host:port) the Google front end
// does not serve.
func (f *Frontend) lanSkipped(addr string, err error) {
	ip, port, _ := net.SplitHostPort(addr)
	if why := f.env.ExplainBind(context.Background(), err); why != err {
		f.env.Log.Warn("Google front end: cannot serve on --gfe-bind address", "address", ip, "port", port, "err", why)
		return
	}
	f.env.Log.Warn("Google front end: cannot serve on --gfe-bind address; clients there expect port 443, which an unprivileged process binds only after `sudo sysctl net.ipv4.ip_unprivileged_port_start=443`",
		"address", ip, "port", port, "err", err)
}

// StopLAN stops the --gfe-bind listeners and removes the LAN front-end
// relay, one still starting included. The Instance calls it before it
// removes its containers, so that no relay starts after.
func (f *Frontend) StopLAN() {
	f.stopRelay()
	f.mu.Lock()
	for _, s := range f.lan {
		_ = s.Close()
	}
	if f.lanAddrs != nil {
		f.env.Endpoints.Delete(LANEndpoint)
	}
	relay := f.relay
	f.lan, f.lanAddrs, f.relay = nil, nil, nil
	f.mu.Unlock()
	if relay != nil {
		closeRelay(relay)
	}
}

type lanHandler struct {
	f   *Frontend
	lan LAN
}

func (h *lanHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Services report the Data plane addresses this caller's machine can
	// reach, such as Cloud SQL's --sql-bind address.
	var local string
	if a, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr); ok {
		local, _, _ = net.SplitHostPort(a.String())
	}
	r = r.WithContext(emu.WithLANCaller(r.Context(), local))
	host := r.Host
	if hh, _, err := net.SplitHostPort(host); err == nil {
		host = hh
	}
	host = Normalize(host)
	if !h.f.Serves(host) {
		http.Error(w, "Google front end: "+host+" is not served by this emulator", http.StatusMisdirectedRequest)
		return
	}
	if IsRegistryHost(host) {
		var reg http.Handler
		if h.lan.Registry != nil {
			reg = h.lan.Registry()
		}
		if reg == nil {
			http.Error(w, "Artifact Registry is not running", http.StatusServiceUnavailable)
			return
		}
		reg.ServeHTTP(w, r)
		return
	}
	if p := r.URL.Path; p == "/_emu" || strings.HasPrefix(p, "/_emu/") || p == "/console" || strings.HasPrefix(p, "/console/") {
		apierr.Write(w, apierr.NotFound("Not Found"))
		return
	}
	read := r.Method == http.MethodGet || r.Method == http.MethodHead
	if host == storageHost && read && !carriesCredentials(r) && h.lan.ReadThrough != nil && h.lan.Storage != nil && h.lan.Storage.ForeignBucket(r) {
		h.lan.ReadThrough.ServeHTTP(w, r)
		return
	}
	authz := r.Header.Get("Authorization")
	if host == oauth2Host && strings.TrimSuffix(r.URL.Path, "/") == "/token" {
		if !keyGrant(r) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":"unsupported_grant_type","error_description":"Only the JWT-bearer grant of a service account key is accepted beyond loopback."}`)
			return
		}
		h.lan.Gateway.ServeHTTP(w, r)
		return
	}
	if tok, ok := strings.CutPrefix(authz, "Bearer "); ok {
		if _, ok := h.f.env.Auth.Authenticate(r.Context(), strings.TrimSpace(tok)); ok {
			h.lan.Gateway.ServeHTTP(w, r)
			return
		}
	}
	if host == storageHost && h.lan.Storage != nil && h.lan.Storage.Signed(r) {
		h.lan.Gateway.ServeHTTP(w, r)
		return
	}
	w.Header().Set("WWW-Authenticate", `Bearer realm="https://accounts.google.com/"`)
	apierr.Write(w, apierr.Unauthenticated("Request is missing required authentication credential. Expected OAuth 2 access token, login cookie or other valid authentication credential."))
}

// maxTokenForm bounds the token request body read to find its grant.
const maxTokenForm = 1 << 20

// carriesCredentials reports whether r carries a credential the Instance
// may have issued, which must never reach Google: an Authorization or API
// key header, or an access token or API key in the query.
func carriesCredentials(r *http.Request) bool {
	for _, h := range []string{"Authorization", "Proxy-Authorization", "X-Goog-Api-Key"} {
		if r.Header.Get(h) != "" {
			return true
		}
	}
	q := r.URL.Query()
	return q.Has("access_token") || q.Has("key")
}

// keyGrant reports whether r is a JWT-bearer token request. It reads the
// body and puts it back. The token endpoint reads a form body together
// with the query, and only the query when the body is neither a form nor
// JSON, so a request with a query or another body is refused.
func keyGrant(r *http.Request) bool {
	if r.Method != http.MethodPost || r.Body == nil || r.URL.RawQuery != "" {
		return false
	}
	ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	json := strings.HasPrefix(r.Header.Get("Content-Type"), "application/json")
	if !json && ct != "application/x-www-form-urlencoded" {
		return false
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, maxTokenForm))
	r.Body.Close()
	r.Body = io.NopCloser(bytes.NewReader(b))
	if err != nil {
		return false
	}
	if json {
		var v struct {
			GrantType string `json:"grant_type"`
		}
		return stdjson.Unmarshal(b, &v) == nil && v.GrantType == jwtGrant
	}
	v, err := url.ParseQuery(string(b))
	return err == nil && v.Get("grant_type") == jwtGrant
}

// NewReadThrough returns the handler that sends requests on to real GCS,
// resolving storage.googleapis.com with resolvers (host:port), the host's
// real DNS servers, since the host's own resolver may send it back here in
// Host mode. It honours HTTPS_PROXY. With no resolvers it returns nil.
func NewReadThrough(resolvers []string) http.Handler {
	if len(resolvers) == 0 {
		return nil
	}
	res := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
		var d net.Dialer
		var err error
		for _, s := range resolvers {
			var c net.Conn
			if c, err = d.DialContext(ctx, network, s); err == nil {
				return c, nil
			}
		}
		return nil, err
	}}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.DialContext = (&net.Dialer{Timeout: 10 * time.Second, Resolver: res}).DialContext
	target := &url.URL{Scheme: "https", Host: storageHost}
	return &httputil.ReverseProxy{
		Transport: tr,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.Host = ""
			pr.Out.Header.Del("Cookie")
		},
	}
}
