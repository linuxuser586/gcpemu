package trust

import (
	"bytes"
	"encoding/pem"
	"unsafe"

	"golang.org/x/sys/windows"
)

// systemRoots exports the current user's ROOT certificate store (which
// includes the machine's) as PEM: Windows keeps its roots in the store, not
// in a bundle file, and SSL_CERT_FILE replaces it for OpenSSL and Python
// clients, so the bundle must carry them (#219). Roots that Windows fetches
// on demand through automatic root update are absent until first used.
func systemRoots() []byte {
	name, err := windows.UTF16PtrFromString("ROOT")
	if err != nil {
		return nil
	}
	store, err := windows.CertOpenSystemStore(0, name)
	if err != nil {
		return nil
	}
	defer windows.CertCloseStore(store, 0)
	var b bytes.Buffer
	var cert *windows.CertContext
	for {
		// Passing the previous context frees it; the loop ends on
		// CRYPT_E_NOT_FOUND with a nil context.
		if cert, err = windows.CertEnumCertificatesInStore(store, cert); err != nil || cert == nil {
			break
		}
		if cert.EncodedCert == nil || cert.Length == 0 {
			continue
		}
		_ = pem.Encode(&b, &pem.Block{Type: "CERTIFICATE", Bytes: unsafe.Slice(cert.EncodedCert, cert.Length)})
	}
	return b.Bytes()
}
