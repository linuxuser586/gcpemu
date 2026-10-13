//go:build !windows

package trust

import "os"

// systemFiles are the well-known bundle locations, in the order Go's
// crypto/x509 probes them.
var systemFiles = []string{
	"/etc/ssl/certs/ca-certificates.crt",                // Debian/Ubuntu/Gentoo, Alpine
	"/etc/pki/tls/certs/ca-bundle.crt",                  // Fedora/RHEL 6
	"/etc/ssl/ca-bundle.pem",                            // OpenSUSE
	"/etc/pki/tls/cacert.pem",                           // OpenELEC
	"/etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem", // CentOS/RHEL 7
	"/etc/ssl/cert.pem",                                 // Alpine, macOS
}

// systemRoots returns the first system bundle file that exists.
func systemRoots() []byte {
	for _, f := range systemFiles {
		if b, err := os.ReadFile(f); err == nil && len(b) > 0 {
			return b
		}
	}
	return nil
}
