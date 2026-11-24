package gke

import (
	"context"
	"crypto/tls"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"cloud.google.com/go/container/apiv1/containerpb"
	mdns "github.com/miekg/dns"

	"github.com/linuxuser586/gcpemu/internal/ca"
	"github.com/linuxuser586/gcpemu/internal/clock"
	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// fakeAliases makes the host have Loopback aliases, or not, and serves DNS
// endpoints on a free port rather than 443.
func fakeAliases(t *testing.T, on bool) {
	aliases, port := hostAliases, dnsEndpointPort
	t.Cleanup(func() { hostAliases, dnsEndpointPort = aliases, port })
	hostAliases, dnsEndpointPort = func() bool { return on }, 0
}

// apiRequest is a request the fake API server got.
type apiRequest struct{ user, path string }

// connectAllowed grants container.clusters.connect to alice alone.
type connectAllowed struct{}

func (connectAllowed) Allowed(_ context.Context, p emu.Principal, perm, _ string) bool {
	return p == "user:alice@example.com" && perm == "container.clusters.connect"
}

// dnsDoor is a Service enforcing IAM, with the Instance CA, a cluster whose
// DNS endpoint allows external traffic and whose API server is fake.
type dnsDoor struct {
	s     *Service
	a     *api
	name  string // the cluster's DNS endpoint
	roots *ca.CA
	// seen are the requests the fake API server got.
	seen chan apiRequest
}

func newDNSDoor(t *testing.T, allowExternal bool) *dnsDoor {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := config.Defaults()
	cfg.Deterministic = true
	// The fixture is set up as the Instance's owner would; IAM is enforced
	// once it is.
	auth := emu.NewPolicyAuthorizer(config.IAMOff, log)
	auth.Install(tokenAuthn{"alice": "user:alice@example.com", "bob": "user:bob@example.com"}, connectAllowed{})
	roots, err := ca.Load(t.TempDir(), "test")
	if err != nil {
		t.Fatal(err)
	}
	s := New(&emu.Env{Config: &cfg, Store: store.NewMemory(), Clock: clock.Real{}, IDs: emu.NewIDs(true), Log: log,
		Auth: auth, Endpoints: emu.NewEndpoints(), CA: roots}).(*Service)
	t.Cleanup(func() { _ = s.Stop(context.Background()) })
	a := &api{s: s}
	d := &dnsDoor{s: s, a: a, roots: roots, seen: make(chan apiRequest, 4)}
	createRunning(t, a, apParent, &containerpb.Cluster{Name: "c", InitialNodeCount: 1,
		ControlPlaneEndpointsConfig: &containerpb.ControlPlaneEndpointsConfig{
			DnsEndpointConfig: &containerpb.ControlPlaneEndpointsConfig_DNSEndpointConfig{AllowExternalTraffic: &allowExternal},
		}})
	d.name = serveAt(t, a, "c", "10.0.0.2:443", "").GetControlPlaneEndpointsConfig().GetDnsEndpointConfig().GetEndpoint()
	kube := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d.seen <- apiRequest{r.Header.Get("Impersonate-User"), r.URL.Path}
		_, _ = io.WriteString(w, `{"kind":"NamespaceList"}`)
	}))
	t.Cleanup(kube.Close)
	r, _ := parseRef(apParent, "", "", "", "", 0)
	r.Cluster = "c"
	s.rtFor(r.key()).kube = &kubeClient{base: kube.URL, hc: kube.Client()}
	auth.SetMode(config.IAMEnforce)
	return d
}

// resolve asks the Service's host DNS answer for name's A record.
func (d *dnsDoor) resolve(t *testing.T, name string) (string, int) {
	t.Helper()
	rrs, rcode := d.s.answerHost(context.Background(), mdns.Fqdn(name), mdns.TypeA)
	for _, rr := range rrs {
		if a, ok := rr.(*mdns.A); ok {
			return a.A.String(), rcode
		}
	}
	return "", rcode
}

// get requests path from the DNS endpoint at addr, by its name, with
// token (none when ""), trusting only the Instance CA as gcloud's
// DNS-endpoint kubeconfig does through Host trust.
func (d *dnsDoor) get(t *testing.T, addr, token, path string) int {
	t.Helper()
	hc := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: d.roots.Pool(), ServerName: d.name},
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		},
	}}
	req, _ := http.NewRequest("GET", "https://"+d.name+path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

// TestDNSEndpointFrontDoor is #1229: a cluster's DNS endpoint resolves
// for host clients to a Loopback alias where the emulator serves it over
// TLS from the Instance CA, and passes to the API server, as the caller,
// only requests from callers with container.clusters.connect.
func TestDNSEndpointFrontDoor(t *testing.T) {
	fakeAliases(t, true)
	d := newDNSDoor(t, true)

	ip, rcode := d.resolve(t, d.name)
	if rcode != mdns.RcodeSuccess || ip == "" {
		t.Fatalf("A %s = %q (rcode %d), want a Loopback alias", d.name, ip, rcode)
	}
	addr := d.s.hostServices().DNSEndpoint.Address
	if h, p, _ := net.SplitHostPort(addr); h != ip || p == "" {
		t.Fatalf("DNS endpoint served at %q, want port on %s", addr, ip)
	}
	if _, rcode := d.resolve(t, "gke-00000000000000000000000000000000-1.us-central1.gke.goog"); rcode != mdns.RcodeNameError {
		t.Errorf("a DNS endpoint no cluster has: rcode %d, want NXDOMAIN", rcode)
	}

	if code := d.get(t, addr, "alice", "/api/v1/namespaces"); code != http.StatusOK {
		t.Fatalf("alice: status %d, want 200", code)
	}
	if got := <-d.seen; got != (apiRequest{"alice@example.com", "/api/v1/namespaces"}) {
		t.Errorf("API server got %+v, want alice's request as alice", got)
	}
	if code := d.get(t, addr, "bob", "/api/v1/namespaces"); code != http.StatusForbidden {
		t.Errorf("bob, without container.clusters.connect: status %d, want 403", code)
	}
	if code := d.get(t, addr, "", "/api/v1/namespaces"); code != http.StatusUnauthorized {
		t.Errorf("no credentials: status %d, want 401", code)
	}
	if len(d.seen) != 0 {
		t.Errorf("a refused request reached the API server: %v", <-d.seen)
	}
}

// TestDNSEndpointInternalOnly is #1229: a DNS endpoint that does not allow
// external traffic refuses host clients, which are outside Google Cloud.
func TestDNSEndpointInternalOnly(t *testing.T) {
	fakeAliases(t, true)
	d := newDNSDoor(t, false)
	ip, _ := d.resolve(t, d.name)
	addr := d.s.hostServices().DNSEndpoint.Address
	if ip == "" || addr == "" {
		t.Fatalf("DNS endpoint not served (A %q, at %q)", ip, addr)
	}
	if code := d.get(t, addr, "alice", "/api/v1/namespaces"); code != http.StatusForbidden {
		t.Errorf("status %d, want 403", code)
	}
}

// TestDNSEndpointUnserved is #1229: when the host has no Loopback aliases
// the name gets SERVFAIL and the Admin API says why.
func TestDNSEndpointUnserved(t *testing.T) {
	fakeAliases(t, false)
	d := newDNSDoor(t, true)
	if _, rcode := d.resolve(t, d.name); rcode != mdns.RcodeServerFailure {
		t.Errorf("rcode %d, want SERVFAIL", rcode)
	}
	if j := d.s.hostServices().DNSEndpoint; j.Reason == "" || j.Address != "" {
		t.Errorf("Admin API DNS endpoint = %+v, want a reason and no address", j)
	}
}
