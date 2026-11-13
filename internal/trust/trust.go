// Package trust builds CA bundles that combine the host's Public roots
// with the Instance CA (Section 7.4), for clients that read a single
// bundle file: SSL_CERT_FILE from `gcpemu env --trust` (FR-CORE-004), the
// bundle injected into GKE pods (FR-INT-007) and Cloud Build steps.
package trust

import (
	"bytes"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"sync"

	"github.com/linuxuser586/gcpemu/internal/ca"
)

// BundleFile is the combined bundle's name in the instance directory.
const BundleFile = "ca-bundle.pem"

// Source says where the Public roots were read from.
type Source struct {
	// Where names the store or file they came from; "" when none was found.
	Where string
	// FallbackErr is why the OS's own store could not be read, when the
	// Public roots came from a fallback file instead (macOS).
	FallbackErr error
}

var (
	rootsOnce sync.Once
	roots     []byte
	source    Source
)

func load() {
	rootsOnce.Do(func() {
		var b []byte
		b, source = hostRoots()
		roots = withoutInstanceCAs(b)
	})
}

// PublicRoots returns the Public roots (PEM): the root CAs the host
// trusts for public sites, without any Instance CA (Instances trust each
// other only by Pairing), or nil when none are found. They are read once
// per process: a bundle file on Linux, the keychain on macOS, the ROOT
// certificate store on Windows. SSL_CERT_FILE is ignored so that a bundle
// produced by `gcpemu env --trust` is never folded into itself.
func PublicRoots() []byte {
	load()
	return roots
}

// PublicRootsSource says where PublicRoots came from.
func PublicRootsSource() Source {
	load()
	return source
}

// PublicRootPool returns a new pool of the Public roots, which the caller
// may add to.
func PublicRootPool() *x509.CertPool {
	p := x509.NewCertPool()
	p.AppendCertsFromPEM(PublicRoots())
	return p
}

// withoutInstanceCAs returns the certificates of bundle (PEM) that are not
// an Instance CA, re-encoded without the text between them. A certificate
// Go cannot parse is kept: it is not one an Instance made.
func withoutInstanceCAs(bundle []byte) []byte {
	var out bytes.Buffer
	for rest := bundle; ; {
		var b *pem.Block
		if b, rest = pem.Decode(rest); b == nil {
			break
		}
		if b.Type != "CERTIFICATE" {
			continue
		}
		if c, err := x509.ParseCertificate(b.Bytes); err == nil && ca.IsInstanceCA(c) {
			continue
		}
		_ = pem.Encode(&out, &pem.Block{Type: "CERTIFICATE", Bytes: b.Bytes})
	}
	if out.Len() == 0 {
		return nil
	}
	return out.Bytes()
}

// marker separates the Instance CA from the Public roots in a bundle.
const marker = "# gcpemu local CA\n"

// Bundle returns the Public roots followed by caPEM.
func Bundle(caPEM []byte) []byte {
	return Append(PublicRoots(), caPEM)
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
