// macOS's /etc/ssl/cert.pem ships EC-ACC, whose serial is negative; Go
// refuses to parse it unless x509negativeserial is set (#186).
//
//go:debug x509negativeserial=1
package trust

import (
	"bytes"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
)

func TestAppendAndWrite(t *testing.T) {
	ca := []byte("-----BEGIN CERTIFICATE-----\nQUJD\n-----END CERTIFICATE-----\n")
	got := Append([]byte("ROOTS"), ca)
	if !bytes.HasPrefix(got, []byte("ROOTS\n# gcpemu local CA\n-----BEGIN")) {
		t.Errorf("Append = %q", got)
	}
	if again := Append(got, ca); !bytes.Equal(again, got) {
		t.Errorf("Append is not idempotent: %q", again)
	}
	p, err := WriteBundle(filepath.Join(t.TempDir(), BundleFile), ca)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	if !bytes.HasPrefix(b, SystemRoots()) || !bytes.Contains(b, bytes.TrimSpace(ca)) {
		t.Errorf("bundle = %d bytes", len(b))
	}
}

// TestSystemRoots checks that the host's roots are found and are all
// certificates (#219: Windows has no bundle file, only its ROOT store).
func TestSystemRoots(t *testing.T) {
	rest := SystemRoots()
	if len(rest) == 0 {
		t.Skip("no system roots on this host")
	}
	n := 0
	for {
		var b *pem.Block
		if b, rest = pem.Decode(rest); b == nil {
			break
		}
		if b.Type != "CERTIFICATE" {
			continue
		}
		if _, err := x509.ParseCertificate(b.Bytes); err != nil {
			t.Errorf("root %d: %v", n, err)
		}
		n++
	}
	if n == 0 {
		t.Error("system roots hold no certificates")
	}
}
