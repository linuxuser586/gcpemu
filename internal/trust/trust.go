// Package trust builds CA bundles that combine the host's system roots
// with the emulator's local CA (Section 7.4), for clients that read a
// single bundle file: SSL_CERT_FILE from `gcpemu env --trust`
// (FR-CORE-004) and the bundle injected into GKE pods (FR-INT-007).
package trust

import (
	"bytes"
	"os"
	"path/filepath"
	"sync"
)

// BundleFile is the combined bundle's name in the instance directory.
const BundleFile = "ca-bundle.pem"

// systemFiles are the well-known Linux bundle locations, in the order Go's
// crypto/x509 probes them.
var systemFiles = []string{
	"/etc/ssl/certs/ca-certificates.crt",                // Debian/Ubuntu/Gentoo, Alpine
	"/etc/pki/tls/certs/ca-bundle.crt",                  // Fedora/RHEL 6
	"/etc/ssl/ca-bundle.pem",                            // OpenSUSE
	"/etc/pki/tls/cacert.pem",                           // OpenELEC
	"/etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem", // CentOS/RHEL 7
	"/etc/ssl/cert.pem",                                 // Alpine, macOS
}

var (
	rootsOnce sync.Once
	roots     []byte
)

// SystemRoots returns the host's system root bundle (PEM), or nil when
// none is found. SSL_CERT_FILE is ignored so that a bundle produced by
// `gcpemu env --trust` is never folded into itself.
func SystemRoots() []byte {
	rootsOnce.Do(func() {
		for _, f := range systemFiles {
			if b, err := os.ReadFile(f); err == nil && len(b) > 0 {
				roots = b
				return
			}
		}
	})
	return roots
}

// marker separates the emulator CA from the system roots in a bundle.
const marker = "# gcpemu local CA\n"

// Bundle returns the system roots followed by caPEM.
func Bundle(caPEM []byte) []byte {
	return Append(SystemRoots(), caPEM)
}

// Append returns bundle with caPEM appended, unless it already contains it.
func Append(bundle, caPEM []byte) []byte {
	ca := bytes.TrimSpace(caPEM)
	if len(ca) == 0 || bytes.Contains(bundle, ca) {
		return bundle
	}
	var b bytes.Buffer
	b.Write(bundle)
	if len(bundle) > 0 && !bytes.HasSuffix(bundle, []byte("\n")) {
		b.WriteByte('\n')
	}
	b.WriteString(marker)
	b.Write(ca)
	b.WriteByte('\n')
	return b.Bytes()
}

// WriteBundle writes Bundle(caPEM) to path atomically (mode 0644: it holds
// only public certificates) and returns path.
func WriteBundle(path string, caPEM []byte) (string, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, Bundle(caPEM), 0o644); err != nil {
		return "", err
	}
	return path, os.Rename(tmp, path)
}
