package certs_test

import (
	"bytes"
	"crypto/x509"
	"os"
	"path/filepath"
	"strings"
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
    - {project: test-proj, location: us-central1, id: regional, domain: regional.example.test, type: PER_PROJECT_RECORD}
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

// A seeded Certificate map entry naming a regional certificate fails the same
// way create does.
func TestSeedRegionalMapEntry(t *testing.T) {
	e := start(t)
	ca := newTestCA(t, "seed CA")
	dir := t.TempDir()
	web, webKey := ca.leaf(t, x509.ExtKeyUsageServerAuth, "app.example.test")
	for f, c := range map[string]string{"web.pem": web, "web-key.pem": webKey} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte(c), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	seed := `
certs:
  certificates:
    - {project: test-proj, location: us-central1, id: web, selfManaged: {pemCertificateFile: web.pem, pemPrivateKeyFile: web-key.pem}}
  certificateMaps:
    - {project: test-proj, id: web}
  certificateMapEntries:
    - {project: test-proj, certificateMap: web, id: app, hostname: app.example.test, certificates: [projects/test-proj/locations/us-central1/certificates/web]}
`
	path := filepath.Join(dir, "seed.yaml")
	if err := os.WriteFile(path, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}
	want := `Certificate projects/test-proj/locations/us-central1/certificates/web must be in location "global" to be used in a certificate map entry.`
	if err := e.inst.ApplySeed(e.ctx, path); err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("seed: %v", err)
	}
}

// TestSeedExport exports a seeded Instance, applies the export to the
// reset Instance and expects the same export back (FR-CORE-013).
func TestSeedExport(t *testing.T) {
	e := start(t)
	ca := newTestCA(t, "export CA")
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
    - {project: test-proj, id: backends, description: backend CAs, trustStores: [{trustAnchors: [{pemCertificateFile: ca.pem}]}]}
  dnsAuthorizations:
    - {project: test-proj, id: example, domain: example.test}
    - {project: test-proj, location: us-central1, id: regional, domain: regional.example.test, type: PER_PROJECT_RECORD}
  certificates:
    - {project: test-proj, id: web, labels: {env: test}, selfManaged: {pemCertificateFile: web.pem, pemPrivateKeyFile: web-key.pem}}
    - {project: test-proj, id: lb-client, scope: CLIENT_AUTH, selfManaged: {pemCertificateFile: cli.pem, pemPrivateKeyFile: cli-key.pem}}
    - {project: test-proj, id: managed, managed: {domains: [example.test, "*.example.test"], dnsAuthorizations: [example]}}
  certificateMaps:
    - {project: test-proj, id: web, labels: {env: test}}
  certificateMapEntries:
    - {project: test-proj, certificateMap: web, id: app, hostname: app.example.test, certificates: [web]}
    - {project: test-proj, certificateMap: web, id: primary, matcher: PRIMARY, certificates: [managed]}
  backendAuthenticationConfigs:
    - {project: test-proj, id: mtls, clientCertificate: lb-client, trustConfig: backends}
  serverTlsPolicies:
    - {project: test-proj, id: frontend, mtlsPolicy: {clientValidationMode: REJECT_INVALID, clientValidationTrustConfig: backends}}
`
	if err := e.inst.ApplySeedData(e.ctx, []byte(seed), dir); err != nil {
		t.Fatal(err)
	}
	first, err := e.inst.ExportSeed(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, unwanted := range []string{"createTime", "etag", "state:", "sanDnsnames", "dnsResourceRecord", "projects/test-proj/locations"} {
		if bytes.Contains(first, []byte(unwanted)) {
			t.Errorf("export contains %q:\n%s", unwanted, first)
		}
	}
	if err := e.inst.Reset(e.ctx); err != nil {
		t.Fatal(err)
	}
	if err := e.inst.ApplySeedData(e.ctx, first, t.TempDir()); err != nil {
		t.Fatalf("apply export: %v\n%s", err, first)
	}
	second, err := e.inst.ExportSeed(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatalf("export changed after a round trip:\n%s\n---\n%s", first, second)
	}
	c, err := e.mgr().MapCertificate(e.ctx, globalLoc+"/certificateMaps/web", "app.example.test")
	if err != nil || c == nil || c.Leaf.DNSNames[0] != "app.example.test" {
		t.Fatalf("map after round trip: %v %v", c, err)
	}
}
