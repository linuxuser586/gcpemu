package lb_test

import (
	"path"
	"testing"

	computev1 "google.golang.org/api/compute/v1"

	"github.com/linuxuser586/gcpemu/internal/ca"
)

// TestSecurityPolicies covers Cloud Armor policies and their rule methods,
// and their attachment to backend services (recorded, FR-LB-011).
func TestSecurityPolicies(t *testing.T) {
	e := start(t)
	c := e.c
	e.do(c.SecurityPolicies.Insert(proj, &computev1.SecurityPolicy{Name: "armor"}).Do())
	p, err := c.SecurityPolicies.Get(proj, "armor").Do()
	if err != nil || p.Type != "CLOUD_ARMOR" || len(p.Rules) != 1 || p.Rules[0].Priority != 2147483647 ||
		p.Rules[0].Action != "allow" || p.Rules[0].Match.Config.SrcIpRanges[0] != "*" || p.Fingerprint == "" {
		t.Fatalf("new policy = %+v, %v", p, err)
	}
	deny := &computev1.SecurityPolicyRule{Action: "deny(403)", Priority: 1000, Match: &computev1.SecurityPolicyRuleMatcher{
		VersionedExpr: "SRC_IPS_V1", Config: &computev1.SecurityPolicyRuleMatcherConfig{SrcIpRanges: []string{"192.0.2.0/24"}}}}
	e.do(c.SecurityPolicies.AddRule(proj, "armor", deny).Do())
	if _, err := c.SecurityPolicies.AddRule(proj, "armor", deny).Do(); errCode(err) != 400 {
		t.Errorf("duplicate priority: %v", err)
	}
	e.do(c.SecurityPolicies.PatchRule(proj, "armor", &computev1.SecurityPolicyRule{Action: "deny(404)"}).Priority(1000).Do())
	r, err := c.SecurityPolicies.GetRule(proj, "armor").Priority(1000).Do()
	if err != nil || r.Action != "deny(404)" || r.Match.Config.SrcIpRanges[0] != "192.0.2.0/24" {
		t.Fatalf("patched rule = %+v, %v", r, err)
	}
	if p, _ := c.SecurityPolicies.Get(proj, "armor").Do(); len(p.Rules) != 2 || p.Rules[0].Priority != 1000 {
		t.Fatalf("rules are not ordered by priority: %+v", p.Rules)
	}
	for name, call := range map[string]func() error{
		"remove default rule": func() error {
			_, err := c.SecurityPolicies.RemoveRule(proj, "armor").Priority(2147483647).Do()
			return err
		},
		"missing rule": func() error { _, err := c.SecurityPolicies.GetRule(proj, "armor").Priority(5).Do(); return err },
		"bad action": func() error {
			_, err := c.SecurityPolicies.AddRule(proj, "armor", &computev1.SecurityPolicyRule{Action: "drop", Priority: 1,
				Match: &computev1.SecurityPolicyRuleMatcher{Expr: &computev1.Expr{Expression: "true"}}}).Do()
			return err
		},
		"no default rule": func() error {
			_, err := c.SecurityPolicies.Insert(proj, &computev1.SecurityPolicy{Name: "nodefault", Rules: []*computev1.SecurityPolicyRule{deny}}).Do()
			return err
		},
	} {
		if code, _ := apiErr(call()); code != 400 {
			t.Errorf("%s: got %d", name, code)
		}
	}

	// Attachment: backend policies to backend services, edge policies to
	// services and buckets; attached policies cannot be deleted.
	e.do(c.SecurityPolicies.Insert(proj, &computev1.SecurityPolicy{Name: "edge", Type: "CLOUD_ARMOR_EDGE"}).Do())
	e.do(c.BackendServices.Insert(proj, &computev1.BackendService{Name: "api", LoadBalancingScheme: "EXTERNAL_MANAGED"}).Do())
	e.do(c.BackendServices.SetSecurityPolicy(proj, "api", &computev1.SecurityPolicyReference{SecurityPolicy: "global/securityPolicies/armor"}).Do())
	e.do(c.BackendServices.SetEdgeSecurityPolicy(proj, "api", &computev1.SecurityPolicyReference{SecurityPolicy: "edge"}).Do())
	bs, _ := c.BackendServices.Get(proj, "api").Do()
	if bs.SecurityPolicy != p.SelfLink || path.Base(bs.EdgeSecurityPolicy) != "edge" {
		t.Fatalf("backend service policies = %q, %q", bs.SecurityPolicy, bs.EdgeSecurityPolicy)
	}
	if _, err := c.BackendServices.SetSecurityPolicy(proj, "api", &computev1.SecurityPolicyReference{SecurityPolicy: "edge"}).Do(); errCode(err) != 400 {
		t.Errorf("edge policy as backend policy: %v", err)
	}
	e.do(c.BackendBuckets.Insert(proj, &computev1.BackendBucket{Name: "static", BucketName: "x", EdgeSecurityPolicy: "global/securityPolicies/edge"}).Do())
	if _, err := c.SecurityPolicies.Delete(proj, "armor").Do(); errReason(err) != "resourceInUseByAnotherResource" {
		t.Errorf("deleting an attached policy: %v", err)
	}
	e.do(c.BackendServices.SetSecurityPolicy(proj, "api", &computev1.SecurityPolicyReference{}).Do())
	e.do(c.SecurityPolicies.Delete(proj, "armor").Do())
}

// TestLegacyHealthChecks covers httpHealthChecks and httpsHealthChecks.
func TestLegacyHealthChecks(t *testing.T) {
	e := start(t)
	c := e.c
	e.do(c.HttpHealthChecks.Insert(proj, &computev1.HttpHealthCheck{Name: "http"}).Do())
	h, err := c.HttpHealthChecks.Get(proj, "http").Do()
	if err != nil || h.Port != 80 || h.RequestPath != "/" || h.CheckIntervalSec != 5 || h.TimeoutSec != 5 ||
		h.HealthyThreshold != 2 || h.UnhealthyThreshold != 2 || h.Kind != "compute#httpHealthCheck" {
		t.Fatalf("http health check = %+v, %v", h, err)
	}
	e.do(c.HttpHealthChecks.Patch(proj, "http", &computev1.HttpHealthCheck{RequestPath: "/healthz"}).Do())
	if h, _ := c.HttpHealthChecks.Get(proj, "http").Do(); h.RequestPath != "/healthz" || h.Port != 80 {
		t.Fatalf("patched = %+v", h)
	}
	e.do(c.HttpsHealthChecks.Insert(proj, &computev1.HttpsHealthCheck{Name: "https"}).Do())
	if hs, _ := c.HttpsHealthChecks.Get(proj, "https").Do(); hs.Port != 443 {
		t.Fatalf("https health check = %+v", hs)
	}
	_, err = c.HttpsHealthChecks.Insert(proj, &computev1.HttpsHealthCheck{Name: "bad", CheckIntervalSec: 2, TimeoutSec: 10}).Do()
	if code, _ := apiErr(err); code != 400 {
		t.Errorf("timeout above interval: %v", err)
	}
	e.do(c.HttpHealthChecks.Delete(proj, "http").Do())
}

// TestL4AndGRPCProxies covers target TCP, SSL and gRPC proxies and their
// forwarding rules (recorded: no listener).
func TestL4AndGRPCProxies(t *testing.T) {
	e := start(t)
	c := e.c
	e.do(c.HealthChecks.Insert(proj, &computev1.HealthCheck{Name: "tcp", TcpHealthCheck: &computev1.TCPHealthCheck{Port: 5432}}).Do())
	e.do(c.BackendServices.Insert(proj, &computev1.BackendService{Name: "http", LoadBalancingScheme: "EXTERNAL_MANAGED"}).Do())
	e.do(c.BackendServices.Insert(proj, &computev1.BackendService{Name: "tcp", LoadBalancingScheme: "EXTERNAL_MANAGED", Protocol: "TCP",
		HealthChecks: []string{"global/healthChecks/tcp"}}).Do())
	_, err := c.TargetTcpProxies.Insert(proj, &computev1.TargetTcpProxy{Name: "bad", Service: "global/backendServices/http"}).Do()
	if code, _ := apiErr(err); code != 400 {
		t.Errorf("TCP proxy to an HTTP backend service: %v", err)
	}
	e.do(c.TargetTcpProxies.Insert(proj, &computev1.TargetTcpProxy{Name: "tcp", Service: "global/backendServices/tcp"}).Do())
	e.do(c.TargetTcpProxies.SetProxyHeader(proj, "tcp", &computev1.TargetTcpProxiesSetProxyHeaderRequest{ProxyHeader: "PROXY_V1"}).Do())
	tp, err := c.TargetTcpProxies.Get(proj, "tcp").Do()
	if err != nil || tp.ProxyHeader != "PROXY_V1" || path.Base(tp.Service) != "tcp" {
		t.Fatalf("TCP proxy = %+v, %v", tp, err)
	}
	e.do(c.GlobalForwardingRules.Insert(proj, &computev1.ForwardingRule{Name: "tcp", LoadBalancingScheme: "EXTERNAL_MANAGED",
		PortRange: "5432", Target: "global/targetTcpProxies/tcp"}).Do())
	if fr, err := c.GlobalForwardingRules.Get(proj, "tcp").Do(); err != nil || fr.PortRange != "5432-5432" || fr.IPAddress == "" {
		t.Fatalf("TCP forwarding rule = %+v, %v", fr, err)
	}
	if _, err := c.BackendServices.Delete(proj, "tcp").Do(); errReason(err) != "resourceInUseByAnotherResource" {
		t.Errorf("deleting a backend service used by a TCP proxy: %v", err)
	}

	leaf, err := e.inst.Env.CA.Issue(ca.Leaf{DNSNames: []string{"db.example.test"}})
	if err != nil {
		t.Fatal(err)
	}
	chain, key, _ := ca.EncodeCert(leaf)
	e.do(c.SslCertificates.Insert(proj, &computev1.SslCertificate{Name: "cert", Certificate: string(chain), PrivateKey: string(key)}).Do())
	_, err = c.TargetSslProxies.Insert(proj, &computev1.TargetSslProxy{Name: "ssl", Service: "global/backendServices/tcp"}).Do()
	if code, _ := apiErr(err); code != 400 {
		t.Errorf("SSL proxy without certificates: %v", err)
	}
	e.do(c.TargetSslProxies.Insert(proj, &computev1.TargetSslProxy{Name: "ssl", Service: "global/backendServices/tcp",
		SslCertificates: []string{"cert"}}).Do())
	e.do(c.SslPolicies.Insert(proj, &computev1.SslPolicy{Name: "modern", Profile: "MODERN"}).Do())
	e.do(c.TargetSslProxies.SetSslPolicy(proj, "ssl", &computev1.SslPolicyReference{SslPolicy: "global/sslPolicies/modern"}).Do())
	if sp, err := c.TargetSslProxies.Get(proj, "ssl").Do(); err != nil || sp.ProxyHeader != "NONE" || path.Base(sp.SslPolicy) != "modern" || len(sp.SslCertificates) != 1 {
		t.Fatalf("SSL proxy = %+v, %v", sp, err)
	}

	// Proxyless gRPC.
	e.do(c.BackendServices.Insert(proj, &computev1.BackendService{Name: "grpc", LoadBalancingScheme: "INTERNAL_SELF_MANAGED", Protocol: "GRPC"}).Do())
	e.do(c.UrlMaps.Insert(proj, &computev1.UrlMap{Name: "grpc", DefaultService: "global/backendServices/grpc"}).Do())
	e.do(c.TargetGrpcProxies.Insert(proj, &computev1.TargetGrpcProxy{Name: "grpc", UrlMap: "global/urlMaps/grpc", ValidateForProxyless: true}).Do())
	e.do(c.TargetGrpcProxies.Patch(proj, "grpc", &computev1.TargetGrpcProxy{Description: "mesh"}).Do())
	if gp, err := c.TargetGrpcProxies.Get(proj, "grpc").Do(); err != nil || gp.Description != "mesh" || !gp.ValidateForProxyless || gp.Fingerprint == "" {
		t.Fatalf("gRPC proxy = %+v, %v", gp, err)
	}
	e.do(c.Networks.Insert(proj, &computev1.Network{Name: "mesh"}).Do())
	_, err = c.GlobalForwardingRules.Insert(proj, &computev1.ForwardingRule{Name: "grpc", LoadBalancingScheme: "EXTERNAL_MANAGED",
		PortRange: "80", Target: "global/targetGrpcProxies/grpc"}).Do()
	if code, _ := apiErr(err); code != 400 {
		t.Errorf("gRPC proxy with an external scheme: %v", err)
	}
	e.do(c.GlobalForwardingRules.Insert(proj, &computev1.ForwardingRule{Name: "grpc", LoadBalancingScheme: "INTERNAL_SELF_MANAGED",
		IPAddress: "0.0.0.0", PortRange: "8443", Network: "global/networks/mesh", Target: "global/targetGrpcProxies/grpc"}).Do())

	// Regional TCP proxy.
	e.do(c.RegionBackendServices.Insert(proj, "us-central1", &computev1.BackendService{Name: "rtcp", LoadBalancingScheme: "INTERNAL_MANAGED", Protocol: "TCP"}).Do())
	e.do(c.RegionTargetTcpProxies.Insert(proj, "us-central1", &computev1.TargetTcpProxy{Name: "rtcp", Service: "regions/us-central1/backendServices/rtcp"}).Do())
	agg, err := c.TargetTcpProxies.AggregatedList(proj).Do()
	if err != nil || len(agg.Items["global"].TargetTcpProxies) != 1 || len(agg.Items["regions/us-central1"].TargetTcpProxies) != 1 {
		t.Fatalf("aggregated TCP proxies = %+v, %v", agg, err)
	}

	e.do(c.GlobalForwardingRules.Delete(proj, "tcp").Do())
	e.do(c.TargetTcpProxies.Delete(proj, "tcp").Do())
}

// TestInternetNEGBackend: backend services accept global internet NEGs.
func TestInternetNEGBackend(t *testing.T) {
	e := start(t)
	c := e.c
	e.do(c.GlobalNetworkEndpointGroups.Insert(proj, &computev1.NetworkEndpointGroup{Name: "ext", NetworkEndpointType: "INTERNET_FQDN_PORT"}).Do())
	e.do(c.BackendServices.Insert(proj, &computev1.BackendService{Name: "ext", LoadBalancingScheme: "EXTERNAL_MANAGED", Protocol: "HTTPS",
		Backends: []*computev1.Backend{{Group: "global/networkEndpointGroups/ext"}}}).Do())
	bs, err := c.BackendServices.Get(proj, "ext").Do()
	if err != nil || len(bs.Backends) != 1 || bs.Backends[0].BalancingMode != "RATE" {
		t.Fatalf("backend service = %+v, %v", bs, err)
	}
	_, err = c.BackendServices.Insert(proj, &computev1.BackendService{Name: "bad",
		Backends: []*computev1.Backend{{Group: "global/networkEndpointGroups/missing"}}}).Do()
	if code, _ := apiErr(err); code != 404 {
		t.Errorf("missing global NEG: %v", err)
	}
	if _, err := c.GlobalNetworkEndpointGroups.Delete(proj, "ext").Do(); errReason(err) != "resourceInUseByAnotherResource" {
		t.Errorf("deleting a NEG in use: %v", err)
	}
}

func errCode(err error) int {
	c, _ := apiErr(err)
	return c
}

func errReason(err error) string {
	_, r := apiErr(err)
	return r
}
