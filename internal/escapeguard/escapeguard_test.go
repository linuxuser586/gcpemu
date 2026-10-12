package escapeguard

import (
	"net/http"
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

func TestDescribe(t *testing.T) {
	if got := Describe("dns.gcpemu.invalid"); !strings.Contains(got, "dns.googleapis.com") || !strings.Contains(got, "*_custom_endpoint") {
		t.Errorf("Describe(dns.gcpemu.invalid) = %q", got)
	}
	if got, want := Describe("oauth2.googleapis.com"), "Escaped request to oauth2.googleapis.com"; got != want {
		t.Errorf("Describe = %q, want %q", got, want)
	}
}

func TestDescribeGcloud(t *testing.T) {
	if got := DescribeGcloud("pubsub.googleapis.com"); !strings.Contains(got, "CLOUDSDK_API_ENDPOINT_OVERRIDES_PUBSUB") {
		t.Errorf("DescribeGcloud(pubsub.googleapis.com) = %q", got)
	}
	for _, h := range []string{"secretmanager.us-central1.rep.googleapis.com", "example.com"} {
		if got, want := DescribeGcloud(h), "Escaped request to "+h; got != want {
			t.Errorf("DescribeGcloud = %q, want %q", got, want)
		}
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
