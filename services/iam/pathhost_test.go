package iam_test

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"net"
	"net/http"
	"slices"
	"testing"
	"time"
)

// TestLegacyHostCerts: www.googleapis.com is served by the Google frontend
// with path routing, so google.golang.org/api/idtoken (which fetches
// https://www.googleapis.com/oauth2/v3/certs) verifies emulator-signed ID
// tokens in pods (FR-PS-006, FR-INT-007).
func TestLegacyHostCerts(t *testing.T) {
	inst := startIAM(t)
	if !slices.Contains(inst.FrontendHosts(), "www.googleapis.com") {
		t.Fatalf("frontend hosts %v lack www.googleapis.com", inst.FrontendHosts())
	}
	fe := inst.Endpoint("frontend")
	cl := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: inst.Env.CA.Pool()},
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, fe)
		},
	}}
	resp, err := cl.Get("https://www.googleapis.com/oauth2/v3/certs")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var jwks struct {
		Keys []struct{ Kid, Alg string }
	}
	if err := json.NewDecoder(resp.Body).Decode(&jwks); err != nil || resp.StatusCode != 200 || len(jwks.Keys) == 0 || jwks.Keys[0].Alg != "RS256" {
		t.Fatalf("certs: %d %v %+v", resp.StatusCode, err, jwks)
	}
}

// TestOpenIDConnectHostUserinfo is #155: the OpenTofu google provider looks
// up the caller's email at a hard-coded openidconnect.googleapis.com URL,
// so the Google frontend (and with it host mode) answers there.
func TestOpenIDConnectHostUserinfo(t *testing.T) {
	inst := startIAM(t)
	if !slices.Contains(inst.FrontendHosts(), "openidconnect.googleapis.com") {
		t.Fatalf("frontend hosts %v lack openidconnect.googleapis.com", inst.FrontendHosts())
	}
	fe := inst.Endpoint("frontend")
	cl := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: inst.Env.CA.Pool()},
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, fe)
		},
	}}
	req, _ := http.NewRequest("GET", "https://openidconnect.googleapis.com/v1/userinfo?alt=json", nil)
	req.Header.Set("Authorization", "Bearer owner")
	resp, err := cl.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var info struct{ Email string }
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil || resp.StatusCode != 200 || info.Email == "" {
		t.Fatalf("userinfo: %d %v %+v", resp.StatusCode, err, info)
	}
}
