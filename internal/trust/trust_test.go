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
	goruntime "runtime"
	"testing"

	"github.com/linuxuser586/gcpemu/internal/ca"
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
	if !bytes.HasPrefix(b, PublicRoots()) || !bytes.Contains(b, bytes.TrimSpace(ca)) {
		t.Errorf("bundle = %d bytes", len(b))
	}
}

// TestPublicRoots checks that the host's roots are found, are all
// certificates (#219: Windows has no bundle file, only its ROOT store) and
// hold no Instance CA (#1138). On macOS they come from the keychain.
func TestPublicRoots(t *testing.T) {
	src := PublicRootsSource()
	if goruntime.GOOS == "darwin" && (src.Where != "the macOS keychain" || src.FallbackErr != nil) {
		t.Errorf("source = %+v, want the macOS keychain", src)
	}
	rest := PublicRoots()
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
		c, err := x509.ParseCertificate(b.Bytes)
		if err != nil {
			t.Errorf("root %d: %v", n, err)
		} else if ca.IsInstanceCA(c) {
			t.Errorf("root %d is an Instance CA: %s", n, c.Subject)
		}
		n++
	}
	if n == 0 {
		t.Error("system roots hold no certificates")
	}
}

// TestWithoutInstanceCAs: every Instance CA is dropped, whichever Instance
// it is, and the other certificates are kept, without the text between
// them (#1138).
func TestWithoutInstanceCAs(t *testing.T) {
	root := testCert(t, "Public Root")
	var in bytes.Buffer
	in.WriteString("# a comment\n")
	in.Write(pemOf(root))
	for _, name := range []string{"a", "b"} {
		c, err := ca.Load(t.TempDir(), name)
		if err != nil {
			t.Fatal(err)
		}
		in.Write(c.PEM())
	}
	if got := withoutInstanceCAs(in.Bytes()); !bytes.Equal(got, pemOf(root)) {
		t.Errorf("withoutInstanceCAs =\n%s", got)
	}
	if got := withoutInstanceCAs(nil); got != nil {
		t.Errorf("withoutInstanceCAs(nil) = %q", got)
	}
}
