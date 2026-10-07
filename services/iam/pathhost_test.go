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
