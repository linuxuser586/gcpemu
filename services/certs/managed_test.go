package certs_test

import (
	"crypto/x509"
	"testing"
	"time"

	"cloud.google.com/go/certificatemanager/apiv1/certificatemanagerpb"
	dnsv1 "google.golang.org/api/dns/v1"
	"google.golang.org/api/option"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestManagedCertificateDNSAuthorization covers FR-LB-004: a managed
// certificate stays PROVISIONING until the DNS authorization CNAME exists
// in the emulated Cloud DNS, then is issued by the instance CA.
func TestManagedCertificateDNSAuthorization(t *testing.T) {
	e := start(t)
	op, err := e.cm.CreateDnsAuthorization(e.ctx, &certificatemanagerpb.CreateDnsAuthorizationRequest{
		Parent: globalLoc, DnsAuthorizationId: "auth",
		DnsAuthorization: &certificatemanagerpb.DnsAuthorization{Domain: "example.test"},
	})
	if err != nil {
		t.Fatal(err)
	}
	da, err := op.Wait(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	rr := da.GetDnsResourceRecord()
	if rr.GetName() != "_acme-challenge.example.test." || rr.GetType() != "CNAME" || da.GetType() != certificatemanagerpb.DnsAuthorization_FIXED_RECORD {
		t.Fatalf("dns authorization: %v", da)
	}

	cop, err := e.cm.CreateCertificate(e.ctx, &certificatemanagerpb.CreateCertificateRequest{
		Parent: globalLoc, CertificateId: "managed",
		Certificate: &certificatemanagerpb.Certificate{Type: &certificatemanagerpb.Certificate_Managed{Managed: &certificatemanagerpb.Certificate_ManagedCertificate{
			Domains: []string{"example.test", "*.example.test"}, DnsAuthorizations: []string{globalLoc + "/dnsAuthorizations/auth"},
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	c, err := cop.Wait(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if c.GetManaged().GetState() != certificatemanagerpb.Certificate_ManagedCertificate_PROVISIONING {
		t.Fatalf("state %v", c.GetManaged().GetState())
	}
	if _, err := e.mgr().Certificate(e.ctx, c.GetName()); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("provisioning certificate served: %v", err)
	}
	got, err := e.cm.GetCertificate(e.ctx, &certificatemanagerpb.GetCertificateRequest{Name: c.GetName()})
	if err != nil {
		t.Fatal(err)
	}
	m := got.GetManaged()
	if m.GetState() != certificatemanagerpb.Certificate_ManagedCertificate_PROVISIONING || m.GetProvisioningIssue().GetReason() != certificatemanagerpb.Certificate_ManagedCertificate_ProvisioningIssue_AUTHORIZATION_ISSUE ||
		len(m.GetAuthorizationAttemptInfo()) != 2 || m.GetAuthorizationAttemptInfo()[0].GetState() != certificatemanagerpb.Certificate_ManagedCertificate_AuthorizationAttemptInfo_AUTHORIZING {
		t.Fatalf("before CNAME: %v", got)
	}
	// A DNS authorization referenced by a certificate cannot be deleted.
	if _, err := e.cm.DeleteDnsAuthorization(e.ctx, &certificatemanagerpb.DeleteDnsAuthorizationRequest{Name: da.GetName()}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("delete used authorization: %v", err)
	}

	// Publish the CNAME in the emulated Cloud DNS.
	dsvc, err := dnsv1.NewService(e.ctx, option.WithEndpoint(e.inst.GatewayURL()+"/dns/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dsvc.ManagedZones.Create(testProject, &dnsv1.ManagedZone{Name: "zone", DnsName: "example.test.", Description: "x"}).Do(); err != nil {
		t.Fatal(err)
	}
	if _, err := dsvc.ResourceRecordSets.Create(testProject, "zone", &dnsv1.ResourceRecordSet{
		Name: rr.GetName(), Type: "CNAME", Ttl: 300, Rrdatas: []string{rr.GetData()},
	}).Do(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		got, err = e.cm.GetCertificate(e.ctx, &certificatemanagerpb.GetCertificateRequest{Name: c.GetName()})
		if err != nil {
			t.Fatal(err)
		}
		if got.GetManaged().GetState() == certificatemanagerpb.Certificate_ManagedCertificate_ACTIVE || time.Now().After(deadline) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if got.GetManaged().GetState() != certificatemanagerpb.Certificate_ManagedCertificate_ACTIVE || got.GetPemCertificate() == "" ||
		got.GetManaged().GetProvisioningIssue() != nil || got.GetExpireTime() == nil {
		t.Fatalf("after CNAME: %v", got)
	}
	tc, err := e.mgr().Certificate(e.ctx, c.GetName())
	if err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{"example.test", "www.example.test"} {
		if _, err := tc.Leaf.Verify(x509.VerifyOptions{Roots: e.inst.Env.CA.Pool(), DNSName: host}); err != nil {
			t.Fatalf("verify %s: %v", host, err)
		}
	}

	// Rotating the CA re-issues managed certificates from the new root.
	if code := e.rest("POST", "/_emu/v1/ca/rotate", nil, nil); code != 200 {
		t.Fatalf("rotate: %d", code)
	}
	tc2, err := e.mgr().Certificate(e.ctx, c.GetName())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tc2.Leaf.Verify(x509.VerifyOptions{Roots: e.inst.Env.CA.Pool(), DNSName: "example.test"}); err != nil {
		t.Fatalf("after rotate: %v", err)
	}
}
