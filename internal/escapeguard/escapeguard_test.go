package escapeguard

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
)

func TestRefusesAndRecords(t *testing.T) {
	g := New(t)
	var proxy string
	for _, kv := range g.Env() {
		if v, ok := strings.CutPrefix(kv, "HTTPS_PROXY="); ok {
			proxy = v
		}
	}
	u, err := url.Parse(proxy)
	if err != nil {
		t.Fatal(err)
	}
	cl := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(u)}}
	// HTTPS goes through CONNECT, plain HTTP as an absolute URL.
	for _, target := range []string{
		"https://dns.gcpemu.invalid/dns/v1/projects/p",
		"http://example.com/",
		"https://dns.gcpemu.invalid/again",
	} {
		resp, err := cl.Get(target)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode != http.StatusForbidden {
				t.Errorf("GET %s through the guard: %s", target, resp.Status)
			}
		}
	}
	if got, want := g.Take(), []string{"dns.gcpemu.invalid", "example.com"}; !slices.Equal(got, want) {
		t.Errorf("Take() = %v, want %v", got, want)
	}
	if got := g.Take(); len(got) != 0 {
		t.Errorf("second Take() = %v, want none", got)
	}
}

// proxyClient returns a client that sends every request through g.
func proxyClient(t *testing.T, g *Guard, tr *http.Transport) *http.Client {
	t.Helper()
	for _, kv := range g.Env() {
		if v, ok := strings.CutPrefix(kv, "HTTPS_PROXY="); ok {
			u, err := url.Parse(v)
			if err != nil {
				t.Fatal(err)
			}
			tr.Proxy = http.ProxyURL(u)
		}
	}
	return &http.Client{Transport: tr}
}

func TestTunnelsServedHosts(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "served "+r.Host)
	}))
	defer srv.Close()
	ip, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	g := New(t)
	g.Tunnel(ip, []string{"storage.googleapis.com"})
	tr := srv.Client().Transport.(*http.Transport).Clone()
	tr.TLSClientConfig.ServerName = "example.com" // in httptest's certificate
	cl := proxyClient(t, g, tr)
	// The tunnel keeps the port the client asked for.
	resp, err := cl.Get("https://storage.googleapis.com:" + port + "/b/o")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if got, want := string(b), "served storage.googleapis.com:"+port; got != want {
		t.Errorf("tunnelled GET = %q, want %q", got, want)
	}
	if resp, err := cl.Get("https://pubsub.googleapis.com:" + port + "/"); err == nil {
		resp.Body.Close()
		t.Errorf("GET of a host not served: %s", resp.Status)
	}
	if got, want := g.Take(), []string{"pubsub.googleapis.com"}; !slices.Equal(got, want) {
		t.Errorf("Take() = %v, want %v", got, want)
	}
}

func TestDescribe(t *testing.T) {
	if got := Describe("dns.gcpemu.invalid"); !strings.Contains(got, "dns.googleapis.com") || !strings.Contains(got, "*_custom_endpoint") {
		t.Errorf("Describe(dns.gcpemu.invalid) = %q", got)
	}
	if got, want := Describe("oauth2.googleapis.com"), "Escaped request to oauth2.googleapis.com"; got != want {
		t.Errorf("Describe = %q, want %q", got, want)
	}
}

func TestDescribeGcloud(t *testing.T) {
	for _, h := range []string{"pubsub.gcpemu.invalid", "pubsub.googleapis.com"} {
		if got := DescribeGcloud(h); !strings.Contains(got, "CLOUDSDK_API_ENDPOINT_OVERRIDES_PUBSUB") {
			t.Errorf("DescribeGcloud(%s) = %q", h, got)
		}
	}
	for _, h := range []string{"secretmanager.us-central1.rep.googleapis.com", "example.com"} {
		if got, want := DescribeGcloud(h), "Escaped request to "+h; got != want {
			t.Errorf("DescribeGcloud = %q, want %q", got, want)
		}
	}
}

func TestGcloudDropsDescriptorFetch(t *testing.T) {
	got := Gcloud([]string{"storage.gcpemu.invalid", "pubsub.gcpemu.invalid", "storage.googleapis.com"})
	if want := []string{"pubsub.gcpemu.invalid", "storage.googleapis.com"}; !slices.Equal(got, want) {
		t.Errorf("Gcloud() = %v, want %v", got, want)
	}
}

func TestEnvExemptsOnlyLoopback(t *testing.T) {
	env := New(t).Env()
	for _, want := range []string{"NO_PROXY=127.0.0.1,localhost,::1", "no_proxy=127.0.0.1,localhost,::1"} {
		if !slices.Contains(env, want) {
			t.Errorf("Env() = %v, lacks %s", env, want)
		}
	}
}

func TestJavaEnvSetsSystemProperties(t *testing.T) {
	g := New(t)
	env := g.JavaEnv()
	_, port, _ := net.SplitHostPort(g.addr)
	want := "JAVA_TOOL_OPTIONS=-Dhttp.proxyHost=127.0.0.1 -Dhttp.proxyPort=" + port +
		" -Dhttps.proxyHost=127.0.0.1 -Dhttps.proxyPort=" + port + " -Dhttp.nonProxyHosts=localhost|127.*|[::1]"
	if !slices.Contains(env, want) {
		t.Errorf("JavaEnv() = %v, lacks %s", env, want)
	}
	for _, kv := range g.Env() {
		if !slices.Contains(env, kv) {
			t.Errorf("JavaEnv() = %v, lacks %s from Env()", env, kv)
		}
	}
}
