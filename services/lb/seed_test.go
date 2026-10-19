package lb_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/linuxuser586/gcpemu/internal/ca"
)

// TestSeedExport exports a seeded Instance, applies the export to the
// reset Instance and expects the same export back (FR-CORE-013).
func TestSeedExport(t *testing.T) {
	e := start(t)
	ctx := context.Background()
	dir := t.TempDir()
	leaf, _ := e.inst.Env.CA.Issue(ca.Leaf{DNSNames: []string{"seed.example.test"}})
	chain, key, _ := ca.EncodeCert(leaf)
	_ = os.WriteFile(filepath.Join(dir, "c.pem"), chain, 0o600)
	_ = os.WriteFile(filepath.Join(dir, "k.pem"), key, 0o600)
	seed := `
compute:
  project: ` + proj + `
  networks: [{name: vpc}]
  subnetworks:
    - {name: nodes, region: us-central1, network: vpc, ipCidrRange: 10.0.0.0/24}
    - {name: proxy, region: us-central1, network: vpc, ipCidrRange: 10.1.0.0/24, purpose: REGIONAL_MANAGED_PROXY, role: ACTIVE}
  addresses:
    - {name: web-ip}
lb:
  project: ` + proj + `
  healthChecks:
    - {name: hc, type: HTTP, httpHealthCheck: {portSpecification: USE_SERVING_PORT, requestPath: /healthz}}
    - {name: rhc, region: us-central1, type: TCP, tcpHealthCheck: {port: 8080}}
  sslCertificates:
    - {name: app, certificateFile: c.pem, privateKeyFile: k.pem}
    - {name: managed, managed: {domains: [app.example.test]}}
  sslPolicies:
    - {name: modern, profile: MODERN, minTlsVersion: TLS_1_2}
  securityPolicies:
    - name: armor
      rules:
        - {priority: 1000, action: deny(403), match: {versionedExpr: SRC_IPS_V1, config: {srcIpRanges: [192.0.2.0/24]}}}
        - {priority: 2147483647, action: allow, match: {versionedExpr: SRC_IPS_V1, config: {srcIpRanges: ["*"]}}}
  backendBuckets:
    - {name: static, bucketName: assets, enableCdn: true}
  backendServices:
    - {name: api, loadBalancingScheme: EXTERNAL_MANAGED, healthChecks: [global/healthChecks/hc], securityPolicy: global/securityPolicies/armor, timeoutSec: 45}
    - {name: internal, region: us-central1, loadBalancingScheme: INTERNAL_MANAGED, protocol: HTTP, healthChecks: [regions/us-central1/healthChecks/rhc]}
  urlMaps:
    - name: web
      defaultService: global/backendBuckets/static
      hostRules: [{hosts: ["*"], pathMatcher: m}]
      pathMatchers:
        - {name: m, defaultService: global/backendBuckets/static, pathRules: [{paths: ["/api/*"], service: global/backendServices/api}]}
    - {name: internal, region: us-central1, defaultService: regions/us-central1/backendServices/internal}
  targetHttpProxies:
    - {name: internal, region: us-central1, urlMap: regions/us-central1/urlMaps/internal}
  targetHttpsProxies:
    - {name: web, urlMap: global/urlMaps/web, sslCertificates: [global/sslCertificates/app, global/sslCertificates/managed], sslPolicy: global/sslPolicies/modern}
  forwardingRules:
    - {name: web, loadBalancingScheme: EXTERNAL_MANAGED, portRange: "8080", target: global/targetHttpsProxies/web, IPAddress: web-ip, labels: {env: test}}
    - {name: web-ephemeral, loadBalancingScheme: EXTERNAL_MANAGED, portRange: "443", target: global/targetHttpsProxies/web}
    - {name: internal, region: us-central1, loadBalancingScheme: INTERNAL_MANAGED, portRange: "80", target: regions/us-central1/targetHttpProxies/internal, network: vpc, subnetwork: nodes}
`
	if err := e.inst.ApplySeedData(ctx, []byte(seed), dir); err != nil {
		t.Fatal(err)
	}
	first, err := e.inst.ExportSeed(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, unwanted := range []string{"selfLink", "creationTimestamp", "fingerprint", "https://", "PROVISIONING", "expireTime", "subjectAlternativeNames"} {
		if bytes.Contains(first, []byte(unwanted)) {
			t.Errorf("export contains %q:\n%s", unwanted, first)
		}
	}
	if err := e.inst.Reset(ctx); err != nil {
		t.Fatal(err)
	}
	if err := e.inst.ApplySeedData(ctx, first, t.TempDir()); err != nil {
		t.Fatalf("apply export: %v\n%s", err, first)
	}
	second, err := e.inst.ExportSeed(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatalf("export changed after a round trip:\n%s\n---\n%s", first, second)
	}
	fr, err := e.c.GlobalForwardingRules.Get(proj, "web").Do()
	if err != nil || fr.Labels["env"] != "test" {
		t.Fatalf("forwarding rule after round trip = %+v, %v", fr, err)
	}
	if a, err := e.c.GlobalAddresses.Get(proj, "web-ip").Do(); err != nil || a.Address != fr.IPAddress || a.Status != "IN_USE" {
		t.Fatalf("address after round trip = %+v, %v", a, err)
	}
	if e.inst.Endpoint(ep("web")) == "" {
		t.Error("forwarding rule after round trip has no listener")
	}
}
