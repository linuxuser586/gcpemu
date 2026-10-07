package trust

import (
	"bytes"
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
