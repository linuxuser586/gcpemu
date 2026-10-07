package ca

import (
	"crypto/x509"
	"testing"
)

func TestCA(t *testing.T) {
	dir := t.TempDir()
	c, err := Load(dir, "test")
	if err != nil {
		t.Fatal(err)
	}
	cert, err := c.ServerCert("app.example.test", "127.0.0.9")
	if err != nil {
		t.Fatal(err)
	}
	opts := x509.VerifyOptions{Roots: c.Pool(), DNSName: "app.example.test"}
	if _, err := cert.Leaf.Verify(opts); err != nil {
		t.Fatalf("verify: %v", err)
	}
	again, _ := c.ServerCert("127.0.0.9", "app.example.test")
	if again != cert {
		t.Error("server cert not cached")
	}

	// Reload keeps the same root.
	c2, err := Load(dir, "test")
	if err != nil {
		t.Fatal(err)
	}
	if !c2.Certificate().Equal(c.Certificate()) {
		t.Error("reload produced a different root")
	}
	if _, err := cert.Leaf.Verify(x509.VerifyOptions{Roots: c2.Pool(), DNSName: "app.example.test"}); err != nil {
		t.Errorf("verify after reload: %v", err)
	}

	// Rotation invalidates old leaves.
	if err := c2.Rotate("test"); err != nil {
		t.Fatal(err)
	}
	if _, err := cert.Leaf.Verify(x509.VerifyOptions{Roots: c2.Pool(), DNSName: "app.example.test"}); err == nil {
		t.Error("old leaf verified against rotated root")
	}

	client, err := c2.Issue(Leaf{DNSNames: []string{"lb-client"}, ClientOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Leaf.Verify(x509.VerifyOptions{Roots: c2.Pool(), KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		t.Errorf("client cert: %v", err)
	}
}
