package certs_test

import (
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"strings"
	"testing"

	"cloud.google.com/go/networksecurity/apiv1/networksecuritypb"
	cmv1 "google.golang.org/api/certificatemanager/v1"
)

const (
	cmBase = "/certificatemanager/v1/" + globalLoc
	nsBase = "/networksecurity/v1/" + globalLoc
)

// restOp runs a REST mutation and checks the operation completed.
func (e *env) restOp(method, path string, body any) map[string]any {
	e.t.Helper()
	var op map[string]any
	if code := e.rest(method, path, body, &op); code != http.StatusOK {
		e.t.Fatalf("%s %s: %d %v", method, path, code, op)
	}
	if op["done"] != true || op["error"] != nil {
		e.t.Fatalf("%s %s: operation %v", method, path, op)
	}
	return op
}

// TestBackendAuthenticationREST covers FR-LB-006 over REST (the surface
// gcloud and OpenTofu use): a CLIENT_AUTH certificate, a trust config with
// an allowlisted certificate and a backend authentication config, resolved
// through emu.CertManager.
func TestBackendAuthenticationREST(t *testing.T) {
	e := start(t)
	lbCA := newTestCA(t, "lb client CA")
	backendCA := newTestCA(t, "backend CA")
	chain, key := lbCA.leaf(t, x509.ExtKeyUsageClientAuth, "lb.client.test")

	e.restOp("POST", cmBase+"/certificates?certificateId=client", map[string]any{
		"scope": "CLIENT_AUTH", "selfManaged": map[string]any{"pemCertificate": chain, "pemPrivateKey": key},
	})
	var cert map[string]any
	e.rest("GET", cmBase+"/certificates/client", nil, &cert)
	if cert["scope"] != "CLIENT_AUTH" || cert["selfManaged"] != nil || cert["pemCertificate"] == nil {
		t.Fatalf("certificate: %v", cert)
	}
	e.restOp("POST", cmBase+"/certificates?certificateId=server", map[string]any{
		"selfManaged": map[string]any{"pemCertificate": chain, "pemPrivateKey": key},
	})

	allow, _ := backendCA.leaf(t, x509.ExtKeyUsageServerAuth, "pinned.test")
	e.restOp("POST", cmBase+"/trustConfigs?trustConfigId=tc", map[string]any{
		"trustStores":             []any{map[string]any{"trustAnchors": []any{map[string]any{"pemCertificate": backendCA.pem}}}},
		"allowlistedCertificates": []any{map[string]any{"pemCertificate": allow}},
	})
	var tc cmv1.TrustConfig
	e.rest("GET", cmBase+"/trustConfigs/tc", nil, &tc)
	if len(tc.AllowlistedCertificates) != 1 || tc.Etag == "" || len(tc.TrustStores) != 1 {
		t.Fatalf("trust config: %+v", tc)
	}

	// Validation errors.
	var errResp map[string]any
	if code := e.rest("POST", nsBase+"/backendAuthenticationConfigs?backendAuthenticationConfigId=bad", map[string]any{
		"clientCertificate": globalLoc + "/certificates/server", "trustConfig": globalLoc + "/trustConfigs/tc",
	}, &errResp); code != http.StatusBadRequest {
		t.Fatalf("non-CLIENT_AUTH certificate: %d %v", code, errResp)
	}
	if code := e.rest("POST", cmBase+"/certificateMaps?certificateMapId=m", `{"bogus":1}`, &errResp); code != http.StatusBadRequest ||
		!strings.Contains(errResp["error"].(map[string]any)["message"].(string), "bogus") {
		t.Fatalf("unknown field: %d %v", code, errResp)
	}
	if code := e.rest("GET", cmBase+"/certificateMaps/nope", nil, &errResp); code != http.StatusNotFound {
		t.Fatalf("not found: %d", code)
	}

	e.restOp("POST", nsBase+"/backendAuthenticationConfigs?backendAuthenticationConfigId=bac", map[string]any{
		"clientCertificate": globalLoc + "/certificates/client", "trustConfig": globalLoc + "/trustConfigs/tc", "wellKnownRoots": "NONE",
	})
	client, roots, err := e.mgr().BackendAuthentication(e.ctx, "//networksecurity.googleapis.com/"+globalLoc+"/backendAuthenticationConfigs/bac")
	if err != nil || client == nil || roots == nil {
		t.Fatalf("BackendAuthentication: %v %v %v", client, roots, err)
	}
	// The client certificate verifies against a backend trust config of
	// the LB's CA (as an Istio gateway would check it).
	lbPool := x509.NewCertPool()
	lbPool.AddCert(lbCA.cert)
	if _, err := client.Leaf.Verify(x509.VerifyOptions{Roots: lbPool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		t.Fatalf("client cert: %v", err)
	}
	// Backend server certificates are verified against the trust config.
	srvChain, _ := backendCA.leaf(t, x509.ExtKeyUsageServerAuth, "backend.test")
	if _, err := parse(t, srvChain).Verify(x509.VerifyOptions{Roots: roots, DNSName: "backend.test"}); err != nil {
		t.Fatalf("backend cert: %v", err)
	}
	if _, err := parse(t, allow).Verify(x509.VerifyOptions{Roots: roots}); err != nil {
		t.Fatalf("allowlisted cert: %v", err)
	}
	if _, err := parse(t, chain).Verify(x509.VerifyOptions{Roots: roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}}); err == nil {
		t.Fatal("untrusted certificate verified")
	}

	// PUBLIC_ROOTS: the instance CA is trusted.
	e.restOp("PATCH", nsBase+"/backendAuthenticationConfigs/bac?updateMask=wellKnownRoots", map[string]any{"wellKnownRoots": "PUBLIC_ROOTS"})
	_, roots, _ = e.mgr().BackendAuthentication(e.ctx, globalLoc+"/backendAuthenticationConfigs/bac")
	issued, _ := e.inst.Env.CA.ServerCert("svc.test")
	if _, err := issued.Leaf.Verify(x509.VerifyOptions{Roots: roots, DNSName: "svc.test"}); err != nil {
		t.Fatalf("public roots: %v", err)
	}

	// Server TLS policy (frontend mTLS) via the gRPC client.
	op, err := e.ns.CreateServerTlsPolicy(e.ctx, &networksecuritypb.CreateServerTlsPolicyRequest{
		Parent: globalLoc, ServerTlsPolicyId: "stp",
		ServerTlsPolicy: &networksecuritypb.ServerTlsPolicy{MtlsPolicy: &networksecuritypb.ServerTlsPolicy_MTLSPolicy{
			ClientValidationMode:        networksecuritypb.ServerTlsPolicy_MTLSPolicy_REJECT_INVALID,
			ClientValidationTrustConfig: globalLoc + "/trustConfigs/tc",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := op.Wait(e.ctx); err != nil {
		t.Fatal(err)
	}
	pool, mode, err := e.mgr().ServerTLSPolicy(e.ctx, globalLoc+"/serverTlsPolicies/stp")
	if err != nil || mode != "REJECT_INVALID" || pool == nil {
		t.Fatalf("ServerTLSPolicy: %v %q %v", pool, mode, err)
	}

	// The trust config is in use; the BAC is listed over gRPC.
	if code := e.rest("DELETE", cmBase+"/trustConfigs/tc", nil, &errResp); code != http.StatusBadRequest {
		t.Fatalf("delete used trust config: %d %v", code, errResp)
	}
	it := e.ns.ListBackendAuthenticationConfigs(e.ctx, &networksecuritypb.ListBackendAuthenticationConfigsRequest{Parent: globalLoc})
	b, err := it.Next()
	if err != nil || b.GetWellKnownRoots() != networksecuritypb.BackendAuthenticationConfig_PUBLIC_ROOTS || b.GetEtag() == "" {
		t.Fatalf("list: %v %v", b, err)
	}

	// Locations and operations listing.
	var locs map[string]any
	if code := e.rest("GET", "/certificatemanager/v1/projects/"+testProject+"/locations", nil, &locs); code != 200 || len(locs["locations"].([]any)) < 2 {
		t.Fatalf("locations: %d %v", code, locs)
	}
	var ops map[string]any
	if code := e.rest("GET", nsBase+"/operations", nil, &ops); code != 200 || len(ops["operations"].([]any)) == 0 {
		t.Fatalf("operations: %d %v", code, ops)
	}
}

func parse(t *testing.T, p string) *x509.Certificate {
	t.Helper()
	b, _ := pem.Decode([]byte(p))
	c, err := x509.ParseCertificate(b.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return c
}
