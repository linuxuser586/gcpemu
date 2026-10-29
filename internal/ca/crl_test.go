package ca

import (
	"crypto/x509"
	"reflect"
	"testing"
)

const testCRLURL = "http://127.0.0.1:4443/ca.crl"

func TestLeavesCarryCRLURL(t *testing.T) {
	c, err := Load(t.TempDir(), "test")
	if err != nil {
		t.Fatal(err)
	}
	c.SetCRLURL(testCRLURL)
	issued, err := c.Issue(Leaf{DNSNames: []string{"app.example.test"}})
	if err != nil {
		t.Fatal(err)
	}
	served, err := c.ServerCert("localhost")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{testCRLURL}
	for name, leaf := range map[string]*x509.Certificate{"Issue": issued.Leaf, "ServerCert": served.Leaf} {
		if !reflect.DeepEqual(leaf.CRLDistributionPoints, want) {
			t.Errorf("%s: CRL distribution points = %q, want %q", name, leaf.CRLDistributionPoints, want)
		}
	}
}

func TestSetCRLURLIssuesServerCertsAgain(t *testing.T) {
	c, err := Load(t.TempDir(), "test")
	if err != nil {
		t.Fatal(err)
	}
	before, err := c.ServerCert("localhost")
	if err != nil {
		t.Fatal(err)
	}
	c.SetCRLURL(testCRLURL)
	after, err := c.ServerCert("localhost")
	if err != nil {
		t.Fatal(err)
	}
	if after == before || !reflect.DeepEqual(after.Leaf.CRLDistributionPoints, []string{testCRLURL}) {
		t.Errorf("ServerCert after SetCRLURL kept the leaf issued without it")
	}
}

func TestCRL(t *testing.T) {
	c, err := Load(t.TempDir(), "test")
	if err != nil {
		t.Fatal(err)
	}
	first := parseCRL(t, c)
	if len(first.RevokedCertificateEntries) != 0 {
		t.Errorf("CRL revokes %d certificates, want none", len(first.RevokedCertificateEntries))
	}
	if err := first.CheckSignatureFrom(c.Certificate()); err != nil {
		t.Errorf("CRL signature: %v", err)
	}
	if d := first.NextUpdate.Sub(first.ThisUpdate); d != CRLValidity {
		t.Errorf("NextUpdate - ThisUpdate = %v, want %v", d, CRLValidity)
	}
	second := parseCRL(t, c)
	if second.Number.Cmp(first.Number) <= 0 {
		t.Errorf("second CRL number %v is not above the first's %v", second.Number, first.Number)
	}

	old := c.Certificate()
	if err := c.Rotate("test"); err != nil {
		t.Fatal(err)
	}
	rotated := parseCRL(t, c)
	if err := rotated.CheckSignatureFrom(c.Certificate()); err != nil {
		t.Errorf("CRL after rotation does not verify against the new CA: %v", err)
	}
	if err := rotated.CheckSignatureFrom(old); err == nil {
		t.Error("CRL after rotation verifies against the old CA")
	}
}

func TestCurrent(t *testing.T) {
	c, err := Load(t.TempDir(), "test")
	if err != nil {
		t.Fatal(err)
	}
	issue := func() *x509.Certificate {
		t.Helper()
		l, err := c.Issue(Leaf{DNSNames: []string{"app.example.test"}})
		if err != nil {
			t.Fatal(err)
		}
		return l.Leaf
	}
	noURL := issue()
	c.SetCRLURL(testCRLURL)
	if c.Current(noURL) {
		t.Error("a leaf with no CRL URL is current")
	}
	withURL := issue()
	if !c.Current(withURL) {
		t.Error("a leaf just issued is not current")
	}
	c.SetCRLURL("http://127.0.0.1:5555/ca.crl")
	if c.Current(withURL) {
		t.Error("a leaf with another CRL URL is current")
	}
	moved := issue()
	if err := c.Rotate("test"); err != nil {
		t.Fatal(err)
	}
	if c.Current(moved) {
		t.Error("a leaf from the old CA is current")
	}
}

func parseCRL(t *testing.T, c *CA) *x509.RevocationList {
	t.Helper()
	der, err := c.CRL()
	if err != nil {
		t.Fatal(err)
	}
	l, err := x509.ParseRevocationList(der)
	if err != nil {
		t.Fatal(err)
	}
	return l
}
