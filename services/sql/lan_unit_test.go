package sql

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	sqladmin "google.golang.org/api/sqladmin/v1beta4"

	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/emu"
)

// tlsBackend serves TLS on a loopback port and writes reply to every
// client that completes the handshake.
func tlsBackend(t *testing.T, reply string) string {
	t.Helper()
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: reply},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, k.Public(), k)
	if err != nil {
		t.Fatal(err)
	}
	l, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: k}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				if err := c.(*tls.Conn).Handshake(); err == nil {
					_, _ = io.WriteString(c, reply)
				}
			}()
		}
	}()
	return l.Addr().String()
}

// dialSNI connects to addr over TLS with server name sni and returns what
// the server writes.
func dialSNI(addr, sni string) (string, error) {
	c, err := tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", addr, &tls.Config{ServerName: sni, InsecureSkipVerify: true})
	if err != nil {
		return "", err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	b, err := io.ReadAll(c)
	return string(b), err
}

// A --sql-bind address's port 3307 is shared by every instance: the
// router reads the server name the connector sends and relays the
// connection, its ClientHello untouched, to that instance's agent.
func TestConnectRouterRoutesBySNI(t *testing.T) {
	targets := map[string]string{
		"a.us-central1.sql.goog": tlsBackend(t, "A"),
		"b.us-central1.sql.goog": tlsBackend(t, "B"),
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	r := serveConnect(l, func(sni string) (string, bool) {
		a, ok := targets[sni]
		return a, ok
	}, (&net.Dialer{}).DialContext)
	defer r.Close()

	for sni, want := range map[string]string{"a.us-central1.sql.goog": "A", "b.us-central1.sql.goog": "B"} {
		got, err := dialSNI(r.Addr(), sni)
		if err != nil || got != want {
			t.Errorf("SNI %s: got %q, %v; want %q", sni, got, err, want)
		}
	}
	if got, err := dialSNI(r.Addr(), "c.us-central1.sql.goog"); err == nil {
		t.Errorf("unknown SNI reached %q", got)
	}
}

// Connectors that send no server name reach the address's only instance;
// with two or more, nothing tells them apart, so they are refused.
func TestConnectRoute(t *testing.T) {
	one := map[string]string{"a.r.sql.goog.": "10.0.0.1:3307"}
	two := map[string]string{"a.r.sql.goog.": "10.0.0.1:3307", "b.r.sql.goog.": "10.0.0.2:3307"}
	for _, tc := range []struct {
		name    string
		targets map[string]string
		sni     string
		want    string
	}{
		{"by name", two, "b.r.sql.goog", "10.0.0.2:3307"},
		{"trailing dot and case", two, "A.R.SQL.GOOG.", "10.0.0.1:3307"},
		{"no name, one instance", one, "", "10.0.0.1:3307"},
		{"no name, two instances", two, "", ""},
		{"unknown name, one instance", one, "x.r.sql.goog", ""},
	} {
		got, ok := connectRoute(tc.targets, tc.sni)
		if got != tc.want || ok != (tc.want != "") {
			t.Errorf("%s: connectRoute(%q) = %q, %v; want %q", tc.name, tc.sni, got, ok, tc.want)
		}
	}
}

// newRecord is an instance record with its PKI, as insert makes it.
func newRecord(t *testing.T, project, region, name string) *instanceRecord {
	t.Helper()
	rec := &instanceRecord{Instance: &sqladmin.DatabaseInstance{Project: project, Name: name, Region: region,
		ConnectionName: project + ":" + region + ":" + name}}
	if err := initPKI(rec, project, name); err != nil {
		t.Fatal(err)
	}
	return rec
}

// Every instance has a dnsName, which connectors send as the server name
// that routes them on a --sql-bind address, and which its server
// certificate names. It is the same on every start, and an older
// certificate without it is re-issued from the same server CA.
func TestInstanceDNSName(t *testing.T) {
	a := newRecord(t, "p", "us-central1", "a")
	if !ensureDNSName(a) {
		t.Fatal("a new instance had no dnsName to set")
	}
	name := a.Instance.DnsName
	if !strings.HasSuffix(name, ".us-central1.sql.goog.") || strings.Count(name, ".") != 4 {
		t.Fatalf("dnsName = %q, want <id>.us-central1.sql.goog.", name)
	}
	cert, err := parseCert(a.ServerCertPEM)
	if err != nil {
		t.Fatal(err)
	}
	if err := cert.VerifyHostname(strings.TrimSuffix(name, ".")); err != nil {
		t.Errorf("server certificate: %v", err)
	}
	if cert.Subject.CommonName != "p:a" {
		t.Errorf("server certificate CN = %q, want p:a", cert.Subject.CommonName)
	}
	pool := x509.NewCertPool()
	ca, _ := parseCert(a.ServerCAPEM)
	pool.AddCert(ca)
	if _, err := cert.Verify(x509.VerifyOptions{Roots: pool}); err != nil {
		t.Errorf("server certificate not from the server CA: %v", err)
	}

	pem := a.ServerCertPEM
	if ensureDNSName(a) || a.Instance.DnsName != name || a.ServerCertPEM != pem {
		t.Errorf("second start changed the instance: %q, cert changed %v", a.Instance.DnsName, a.ServerCertPEM != pem)
	}
	again := newRecord(t, "p", "us-central1", "a")
	ensureDNSName(again)
	if again.Instance.DnsName != name {
		t.Errorf("dnsName of p:us-central1:a = %q then %q", name, again.Instance.DnsName)
	}
	b := newRecord(t, "p", "us-central1", "b")
	ensureDNSName(b)
	if b.Instance.DnsName == name {
		t.Errorf("a and b share dnsName %q", name)
	}
}

// A LAN caller sees the --sql-bind address as the instance's only
// address, PRIMARY as on GCP's public path, and no address at all for an
// instance the LAN can't reach.
func TestLANIPAddresses(t *testing.T) {
	want := []*sqladmin.IpMapping{{Type: "PRIMARY", IpAddress: "192.168.1.5"}}
	if got := lanIPAddresses("192.168.1.5"); !reflect.DeepEqual(got, want) {
		t.Errorf("LAN caller: %v, want %v", got, want)
	}
	if got := lanIPAddresses(""); len(got) != 0 {
		t.Errorf("LAN caller of an instance not on the LAN: %v", got)
	}
}

// Without --sql-bind, a LAN caller, such as one over a link, is told no
// address of an instance, never its loopback ones, and connectSettings
// tells the connectors why, naming --sql-bind. A caller on loopback is
// told the loopback addresses as before.
func TestNoSQLBindLANCallerNoAddress(t *testing.T) {
	s := testService(t, config.IAMOff)
	putInstance(t, s, "p", "i", false)
	if err := s.updateRecord("p", "i", func(r *instanceRecord) error {
		r.Instance.IpAddresses = []*sqladmin.IpMapping{{Type: "PRIMARY", IpAddress: "127.0.0.5"}}
		r.Instance.Settings.IpConfiguration = &sqladmin.IpConfiguration{Ipv4Enabled: true}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	rec, err := s.loadRecord("p", "i")
	if err != nil {
		t.Fatal(err)
	}
	link := emu.WithLANCaller(context.Background(), "192.168.1.9")
	if got := s.forCaller(link, rec.Instance).IpAddresses; len(got) != 0 {
		t.Errorf("addresses told a LAN caller = %v, want none", got)
	}
	if got := s.forCaller(context.Background(), rec.Instance).IpAddresses; len(got) != 1 || got[0].IpAddress != "127.0.0.5" {
		t.Errorf("addresses told a loopback caller = %v, want 127.0.0.5", got)
	}
	settings := func(ctx context.Context) *httptest.ResponseRecorder {
		r := httptest.NewRequestWithContext(ctx, http.MethodGet, "/v1/projects/p/instances/i/connectSettings", nil)
		r.SetPathValue("project", "p")
		r.SetPathValue("instance", "i")
		w := httptest.NewRecorder()
		s.connectSettings(w, r)
		return w
	}
	if w := settings(link); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "--sql-bind") {
		t.Errorf("connectSettings for a LAN caller = %d %s, want FAILED_PRECONDITION naming --sql-bind", w.Code, w.Body)
	}
	if w := settings(context.Background()); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "127.0.0.5") {
		t.Errorf("connectSettings for a loopback caller = %d %s", w.Code, w.Body)
	}
}
