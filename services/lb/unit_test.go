package lb

import (
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	computev1 "google.golang.org/api/compute/v1"

	"github.com/linuxuser586/gcpemu/internal/ca"
	emuconfig "github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/loopback"
)

func TestSelectCert(t *testing.T) {
	authority, err := ca.Load(t.TempDir(), "unit")
	if err != nil {
		t.Fatal(err)
	}
	issue := func(names ...string) *tls.Certificate {
		c, err := authority.Issue(ca.Leaf{DNSNames: names})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	primary := issue("primary.test")
	exact := issue("www.example.test")
	wild := issue("*.example.test")
	certs := []*tls.Certificate{primary, wild, exact}
	cases := []struct {
		sni  string
		want *tls.Certificate
	}{
		{"www.example.test", exact},
		{"WWW.Example.Test.", exact},
		{"api.example.test", wild},
		{"a.b.example.test", primary}, // wildcard covers one label only
		{"example.test", primary},
		{"", primary},
		{"other.test", primary},
	}
	for _, c := range cases {
		if got := selectCert(certs, c.sni); got != c.want {
			t.Errorf("selectCert(%q) = %v", c.sni, got.Leaf.DNSNames)
		}
	}
	if selectCert(nil, "x") != nil {
		t.Error("no certificates should select nil")
	}
}

func TestHeaderVariables(t *testing.T) {
	authority, _ := ca.Load(t.TempDir(), "unit")
	client, _ := authority.Issue(ca.Leaf{DNSNames: []string{"client.test"}, ClientOnly: true})
	r := httptest.NewRequest("GET", "https://app.test/x", nil)
	r.Header.Set("User-Agent", "Mozilla/5.0 (iPhone; Mobile) Safari/604.1")
	r.Header.Set("Origin", "https://site.test")
	r.TLS = &tls.ConnectionState{Version: tls.VersionTLS13, CipherSuite: tls.TLS_AES_128_GCM_SHA256, ServerName: "app.test",
		PeerCertificates: []*x509.Certificate{client.Leaf}}
	rs := &reqState{fe: &frontend{ip: "34.120.0.1", port: 443, mtlsMode: "ALLOW_INVALID_OR_MISSING_CLIENT_CERT"}, req: r, clientIP: "127.0.0.1"}
	cases := map[string]string{
		"{client_region}":                     "ZZ",
		"{tls_version}":                       "TLSv1.3",
		"{tls_cipher_suite}":                  "TLS_AES_128_GCM_SHA256",
		"{tls_sni_hostname}":                  "app.test",
		"{client_encrypted}":                  "true",
		"{device_request_type}":               "MOBILE",
		"{user_agent_family}":                 "SAFARI",
		"{origin_request_header}":             "https://site.test",
		"{client_cert_present}":               "true",
		"{client_cert_chain_verified}":        "true",
		"{cdn_cache_status}":                  "disabled",
		"ip={server_ip}:{server_port} {nope}": "ip=34.120.0.1:443 {nope}",
	}
	for in, want := range cases {
		if got := expand(in, rs); got != want {
			t.Errorf("expand(%q) = %q, want %q", in, got, want)
		}
	}
	rs.certErr = "client_cert_chain_invalid"
	if got := expand("{client_cert_chain_verified}/{client_cert_error}", rs); got != "false/client_cert_chain_invalid" {
		t.Errorf("unverified = %q", got)
	}
	// Cache status inference.
	rs.cdnOn = true
	if got := rs.cacheStatusNow(); got != "hit" {
		t.Errorf("no origin = %q", got)
	}
	rs.originCalled.Store(true)
	if got := rs.cacheStatusNow(); got != "uncacheable" {
		t.Errorf("streaming origin = %q", got)
	}
	rs.originDone.Store(true)
	if got := rs.cacheStatusNow(); got != "miss" {
		t.Errorf("stored origin = %q", got)
	}
	rs.originStatus.Store(304)
	if got := rs.cacheStatusNow(); got != "revalidated" {
		t.Errorf("revalidated = %q", got)
	}
}

func TestHeaderActions(t *testing.T) {
	h := http.Header{"X-Keep": {"1"}, "X-Drop": {"1"}, "X-Multi": {"a"}}
	rs := &reqState{fe: &frontend{}, req: httptest.NewRequest("GET", "/", nil)}
	applyRequestActions(h, []*computev1.HttpHeaderAction{
		{RequestHeadersToRemove: []string{"x-drop"}, RequestHeadersToAdd: []*computev1.HttpHeaderOption{
			{HeaderName: "X-Multi", HeaderValue: "b"}, {HeaderName: "X-Keep", HeaderValue: "2", Replace: true}}},
		{RequestHeadersToAdd: []*computev1.HttpHeaderOption{{HeaderName: "X-Region", HeaderValue: "{client_region}"}}},
	}, rs)
	if h.Get("X-Drop") != "" || h.Get("X-Keep") != "2" || len(h.Values("X-Multi")) != 2 || h.Get("X-Region") != "ZZ" {
		t.Fatalf("headers = %v", h)
	}
	applyCustomHeaders(h, []string{"X-Custom: v {client_encrypted}"}, rs)
	if h.Get("X-Custom") != "v false" {
		t.Errorf("custom = %q", h.Get("X-Custom"))
	}
}

func TestOutlierAndPick(t *testing.T) {
	b := &backendSvc{}
	conf := &computev1.BackendService{LocalityLbPolicy: "ROUND_ROBIN",
		OutlierDetection: &computev1.OutlierDetection{ConsecutiveErrors: 2, MaxEjectionPercent: 50, BaseEjectionTime: &computev1.Duration{Seconds: 30}}}
	b.cfg.Store(conf)
	var eps []*endpoint
	for _, a := range []string{"10.0.0.1:80", "10.0.0.2:80", "10.0.0.3:80", "10.0.0.4:80"} {
		e := &endpoint{addr: a}
		e.health.Store(int32(stateHealthy))
		eps = append(eps, e)
	}
	b.eps.Store(&eps)
	r := httptest.NewRequest("GET", "/", nil)
	rs := &reqState{clientIP: "1.2.3.4"}
	seen := map[string]int{}
	for i := 0; i < 8; i++ {
		seen[b.pick(r, rs, nil).addr]++
	}
	if len(seen) != 4 {
		t.Errorf("round robin spread = %v", seen)
	}
	b.outlier(eps[0], 500, nil)
	b.outlier(eps[0], 503, nil)
	if eps[0].ejectedTo.Load() < time.Now().UnixNano() {
		t.Fatal("endpoint not ejected after consecutive errors")
	}
	for i := 0; i < 8; i++ {
		if b.pick(r, rs, nil) == eps[0] {
			t.Fatal("ejected endpoint picked")
		}
	}
	// Session affinity is stable.
	conf2 := *conf
	conf2.SessionAffinity = "CLIENT_IP"
	b.cfg.Store(&conf2)
	first := b.pick(r, rs, nil)
	for i := 0; i < 5; i++ {
		if b.pick(r, rs, nil) != first {
			t.Fatal("CLIENT_IP affinity not stable")
		}
	}
	// Draining endpoints receive no new requests.
	for _, e := range eps {
		e.draining.Store(true)
	}
	if b.pick(r, rs, nil) != nil {
		t.Error("draining endpoint picked")
	}
}

// TestVerifyBackend is FR-LB-006: the backend certificate must chain to
// the trust config and match subjectAltNames, or tlsSettings.sni when no
// subjectAltNames are configured.
func TestVerifyBackend(t *testing.T) {
	authority, _ := ca.Load(t.TempDir(), "unit")
	other, _ := ca.Load(t.TempDir(), "other")
	leaf, _ := authority.Issue(ca.Leaf{DNSNames: []string{"gateway.mesh.test"}})
	st := tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf.Leaf}}
	san := func(n string) []*computev1.BackendServiceTlsSettingsSubjectAltName {
		return []*computev1.BackendServiceTlsSettingsSubjectAltName{{DnsName: n}}
	}
	cases := []struct {
		name  string
		roots *ca.CA
		sni   string
		sans  []*computev1.BackendServiceTlsSettingsSubjectAltName
		ok    bool
	}{
		{"sni matches", authority, "gateway.mesh.test", nil, true},
		{"sni mismatch", authority, "other.mesh.test", nil, false},
		{"no sni or sans", authority, "", nil, true},
		{"sans override sni", authority, "other.mesh.test", san("gateway.mesh.test"), true},
		{"sans mismatch", authority, "gateway.mesh.test", san("other.mesh.test"), false},
		{"untrusted chain", other, "gateway.mesh.test", nil, false},
	}
	for _, c := range cases {
		if err := verifyBackend(st, c.roots.Pool(), c.sni, c.sans); (err == nil) != c.ok {
			t.Errorf("%s: err = %v", c.name, err)
		}
	}
}

// fakeListener hands out connections with the given local addresses.
type fakeListener struct {
	conns chan net.Conn
}

func (f *fakeListener) Accept() (net.Conn, error) {
	c, ok := <-f.conns
	if !ok {
		return nil, net.ErrClosed
	}
	return c, nil
}
func (f *fakeListener) Close() error   { return nil }
func (f *fakeListener) Addr() net.Addr { return &net.TCPAddr{IP: net.IPv4zero, Port: 443} }

// localConn is a connection whose local address is injected.
type localConn struct {
	net.Conn
	local  net.Addr
	closed bool
}

func (c *localConn) LocalAddr() net.Addr { return c.local }
func (c *localConn) Close() error        { c.closed = true; return nil }

// TestWildcardLocalFilter: the wildcard listener of a privileged port on
// macOS serves connections to loopback and --lb-bind addresses only, and
// closes the others unserved (#172).
func TestWildcardLocalFilter(t *testing.T) {
	d := &dataplane{s: &Service{env: &emu.Env{Config: &emuconfig.Config{LBBind: []string{"192.168.1.20", "fd00::1"}}}}}
	conn := func(ip string) *localConn {
		return &localConn{local: &net.TCPAddr{IP: net.ParseIP(ip), Port: 443}}
	}
	cases := []struct {
		c     *localConn
		serve bool
	}{
		{conn("127.0.0.1"), true},
		{conn("127.0.0.9"), true},
		{conn("::1"), true},
		{conn("::ffff:127.0.0.1"), true},
		{conn("192.168.1.20"), true},
		{conn("::ffff:192.168.1.20"), true},
		{conn("fd00::1"), true},
		{conn("192.168.1.21"), false},
		{conn("10.0.0.5"), false},
		{conn("fd00::2"), false},
	}
	fl := &fakeListener{conns: make(chan net.Conn, len(cases))}
	for _, tc := range cases {
		fl.conns <- tc.c
	}
	close(fl.conns)
	ln := &loopback.LocalFilter{Listener: fl, Allow: d.lbLocal}
	var served []net.Conn
	for {
		c, err := ln.Accept()
		if err != nil {
			break
		}
		served = append(served, c)
	}
	for _, tc := range cases {
		got := false
		for _, c := range served {
			got = got || c == net.Conn(tc.c)
		}
		if got != tc.serve || tc.c.closed == tc.serve {
			t.Errorf("connection to %s: served %v, closed %v; want served %v", tc.c.local, got, tc.c.closed, tc.serve)
		}
	}
}
