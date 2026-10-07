package certs_test

import (
	"crypto/x509"
	"os"
	"path/filepath"
	"testing"
)

func TestSeed(t *testing.T) {
	e := start(t)
	ca := newTestCA(t, "seed CA")
	dir := t.TempDir()
	web, webKey := ca.leaf(t, x509.ExtKeyUsageServerAuth, "app.example.test")
	cli, cliKey := ca.leaf(t, x509.ExtKeyUsageClientAuth, "lb.example.test")
	for f, c := range map[string]string{"web.pem": web, "web-key.pem": webKey, "cli.pem": cli, "cli-key.pem": cliKey, "ca.pem": ca.pem} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte(c), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	seed := `
certs:
  trustConfigs:
    - {project: test-proj, id: backends, trustStores: [{trustAnchors: [{pemCertificateFile: ca.pem}]}]}
  dnsAuthorizations:
    - {project: test-proj, id: example, domain: example.test}
  certificates:
    - project: test-proj
      id: web
      selfManaged: {pemCertificateFile: web.pem, pemPrivateKeyFile: web-key.pem}
    - {project: test-proj, id: lb-client, scope: CLIENT_AUTH, selfManaged: {pemCertificateFile: cli.pem, pemPrivateKeyFile: cli-key.pem}}
    - {project: test-proj, id: managed, managed: {domains: [example.test, "*.example.test"], dnsAuthorizations: [example]}}
  certificateMaps:
    - {project: test-proj, id: web, labels: {env: test}}
  certificateMapEntries:
    - {project: test-proj, certificateMap: web, id: app, hostname: app.example.test, certificates: [web]}
  backendAuthenticationConfigs:
    - {project: test-proj, id: mtls, clientCertificate: lb-client, trustConfig: backends}
  serverTlsPolicies:
    - {project: test-proj, id: frontend, mtlsPolicy: {clientValidationMode: REJECT_INVALID, clientValidationTrustConfig: backends}}
`
	path := filepath.Join(dir, "seed.yaml")
	if err := os.WriteFile(path, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ { // idempotent
		if err := e.inst.ApplySeed(e.ctx, path); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}
	c, err := e.mgr().MapCertificate(e.ctx, globalLoc+"/certificateMaps/web", "app.example.test")
	if err != nil || c == nil || c.Leaf.DNSNames[0] != "app.example.test" {
		t.Fatalf("map: %v %v", c, err)
	}
	client, roots, err := e.mgr().BackendAuthentication(e.ctx, globalLoc+"/backendAuthenticationConfigs/mtls")
	if err != nil || client == nil || client.Leaf.DNSNames[0] != "lb.example.test" {
		t.Fatalf("bac: %v %v", client, err)
	}
	if _, err := client.Leaf.Verify(x509.VerifyOptions{Roots: roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		t.Fatal(err)
	}
	if _, mode, err := e.mgr().ServerTLSPolicy(e.ctx, globalLoc+"/serverTlsPolicies/frontend"); err != nil || mode != "REJECT_INVALID" {
		t.Fatalf("stp: %q %v", mode, err)
	}
	var m map[string]any
	if e.rest("GET", cmBase+"/certificates/managed", nil, &m); m["managed"].(map[string]any)["state"] != "PROVISIONING" {
		t.Fatalf("managed: %v", m)
	}
}
