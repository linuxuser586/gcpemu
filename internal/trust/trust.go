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

var (
	rootsOnce sync.Once
	roots     []byte
)

// SystemRoots returns the host's system root bundle (PEM), or nil when
// none is found: a bundle file on Linux and macOS, the ROOT certificate
// store on Windows. SSL_CERT_FILE is ignored so that a bundle produced by
// `gcpemu env --trust` is never folded into itself.
func SystemRoots() []byte {
	rootsOnce.Do(func() { roots = systemRoots() })
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
