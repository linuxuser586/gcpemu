// Package ca is the emulator's local certificate authority (Section 7.4).
// On first start a root CA is created in the instance directory (ca.pem,
// ca-key.pem, mode 0600); it signs Google-managed certificates for load
// balancers (FR-LB-004), host-mode serving certificates (FR-CORE-043) and
// anything else that must chain to a root the user trusts once.
package ca

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// File names inside the instance directory.
const (
	CertFile = "ca.pem"
	KeyFile  = "ca-key.pem"
)

// DefaultLeafValidity is the lifetime of issued leaf certificates (Google-managed
// certificates are valid for 90 days).
const DefaultLeafValidity = 90 * 24 * time.Hour

// CA is a root certificate authority backed by files in a directory.
type CA struct {
	dir string

	mu    sync.RWMutex
	cert  *x509.Certificate
	key   crypto.Signer
	pem   []byte
	cache map[string]*tls.Certificate
	now   func() time.Time
}

// Load opens the CA in dir, creating it if absent. instance names it.
func Load(dir, instance string) (*CA, error) {
	c := &CA{dir: dir, cache: map[string]*tls.Certificate{}, now: time.Now}
	certPEM, err1 := os.ReadFile(filepath.Join(dir, CertFile))
	keyPEM, err2 := os.ReadFile(filepath.Join(dir, KeyFile))
	if err1 == nil && err2 == nil {
		if err := c.parse(certPEM, keyPEM); err != nil {
			return nil, fmt.Errorf("load CA from %s: %w", dir, err)
		}
		return c, nil
	}
	if !errors.Is(err1, os.ErrNotExist) && err1 != nil {
		return nil, err1
	}
	if err := c.generate(instance); err != nil {
		return nil, err
	}
	return c, nil
}

// Rotate replaces the root CA with a new one (`gcpemu ca rotate`). Leaf
// certificates issued by the old root stop verifying once clients trust
// only the new one.
func (c *CA) Rotate(instance string) error {
	return c.generate(instance)
}

func (c *CA) generate(instance string) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial(),
		Subject: pkix.Name{
			Organization: []string{"gcpemu"},
			CommonName:   fmt.Sprintf("gcpemu local CA (%s)", instance),
		},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	if err := os.MkdirAll(c.dir, 0o700); err != nil {
		return err
	}
	if err := writeFile(filepath.Join(c.dir, KeyFile), keyPEM); err != nil {
		return err
	}
	if err := writeFile(filepath.Join(c.dir, CertFile), certPEM); err != nil {
		return err
	}
	return c.parse(certPEM, keyPEM)
}

func writeFile(path string, b []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (c *CA) parse(certPEM, keyPEM []byte) error {
	cb, _ := pem.Decode(certPEM)
	kb, _ := pem.Decode(keyPEM)
	if cb == nil || kb == nil {
		return errors.New("invalid PEM")
	}
	cert, err := x509.ParseCertificate(cb.Bytes)
	if err != nil {
		return err
	}
	k, err := x509.ParsePKCS8PrivateKey(kb.Bytes)
	if err != nil {
		return err
	}
	signer, ok := k.(crypto.Signer)
	if !ok {
		return errors.New("CA key is not a signer")
	}
	c.mu.Lock()
	c.cert, c.key, c.pem = cert, signer, certPEM
	c.cache = map[string]*tls.Certificate{}
	c.mu.Unlock()
	return nil
}

// Certificate returns the root certificate.
func (c *CA) Certificate() *x509.Certificate {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.cert
}

// PEM returns the root certificate in PEM form.
func (c *CA) PEM() []byte {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return append([]byte(nil), c.pem...)
}

// Path returns the path of ca.pem.
func (c *CA) Path() string { return filepath.Join(c.dir, CertFile) }

// Pool returns a cert pool containing only the root.
func (c *CA) Pool() *x509.CertPool {
	p := x509.NewCertPool()
	p.AddCert(c.Certificate())
	return p
}

// Leaf describes a certificate to issue.
type Leaf struct {
	// DNSNames and IPs become SANs; the first DNS name (or IP) is the CN.
	DNSNames []string
	IPs      []net.IP
	// Validity defaults to DefaultLeafValidity.
	Validity time.Duration
	// Client adds the clientAuth extended key usage (serverAuth is always set
	// unless ClientOnly).
	Client     bool
	ClientOnly bool
	// Organization for the subject (optional).
	Organization string
}

// Issue signs a new leaf certificate with a fresh P-256 key.
func (c *CA) Issue(l Leaf) (*tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	if l.Validity == 0 {
		l.Validity = DefaultLeafValidity
	}
	cn := ""
	if len(l.DNSNames) > 0 {
		cn = l.DNSNames[0]
	} else if len(l.IPs) > 0 {
		cn = l.IPs[0].String()
	}
	now := c.now()
	tmpl := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{CommonName: cn},
		DNSNames:     l.DNSNames,
		IPAddresses:  l.IPs,
		NotBefore:    now.Add(-5 * time.Minute),
		NotAfter:     now.Add(l.Validity),
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}
	if l.Organization != "" {
		tmpl.Subject.Organization = []string{l.Organization}
	}
	if !l.ClientOnly {
		tmpl.ExtKeyUsage = append(tmpl.ExtKeyUsage, x509.ExtKeyUsageServerAuth)
	}
	if l.Client || l.ClientOnly {
		tmpl.ExtKeyUsage = append(tmpl.ExtKeyUsage, x509.ExtKeyUsageClientAuth)
	}
	c.mu.RLock()
	parent, signer, rootDER := c.cert, c.key, c.cert.Raw
	c.mu.RUnlock()
	der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, &key.PublicKey, signer)
	if err != nil {
		return nil, err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return &tls.Certificate{Certificate: [][]byte{der, rootDER}, PrivateKey: key, Leaf: leaf}, nil
}

// ServerCert returns a cached serving certificate for the given names,
// issuing (or re-issuing near expiry) as needed.
func (c *CA) ServerCert(names ...string) (*tls.Certificate, error) {
	sorted := append([]string(nil), names...)
	sort.Strings(sorted)
	k := strings.Join(sorted, ",")
	c.mu.RLock()
	cert := c.cache[k]
	c.mu.RUnlock()
	if cert != nil && c.now().Before(cert.Leaf.NotAfter.Add(-24*time.Hour)) {
		return cert, nil
	}
	var l Leaf
	for _, n := range names {
		if ip := net.ParseIP(n); ip != nil {
			l.IPs = append(l.IPs, ip)
		} else {
			l.DNSNames = append(l.DNSNames, n)
		}
	}
	cert, err := c.Issue(l)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.cache[k] = cert
	c.mu.Unlock()
	return cert, nil
}

// EncodeCert returns the PEM chain and PEM private key of a tls.Certificate.
func EncodeCert(cert *tls.Certificate) (chainPEM, keyPEM []byte, err error) {
	for _, der := range cert.Certificate {
		chainPEM = append(chainPEM, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
	}
	kd, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	if err != nil {
		return nil, nil, err
	}
	return chainPEM, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kd}), nil
}

func serial() *big.Int {
	n, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 126))
	return n.Add(n, big.NewInt(1))
}
