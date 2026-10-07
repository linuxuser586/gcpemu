package sql

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"strings"
	"time"

	sqladmin "google.golang.org/api/sqladmin/v1beta4"
)

// PKI (FR-SQL-005, FR-SQL-007). Each instance has a server CA that signs
// its server certificate (CN "project:instance", which the Go connector
// and the Auth Proxy verify) and a client CA that signs sslCerts client
// certificates and the connector's ephemeral certificates. Certificate
// validity always uses the real clock because TLS peers check it.

const (
	serverCACN = "Google Cloud SQL Server CA"
	clientCACN = "Google Cloud SQL Client CA"
	// ephemeralOU marks ephemeral (connector) certificates.
	ephemeralOU = "gcpemu-ephemeral"
	// principalScheme is the URI SAN scheme carrying the IAM principal of
	// an ephemeral certificate requested with an access token.
	principalScheme = "gcpemu-principal"
	caValidity      = 10 * 365 * 24 * time.Hour
	ephemeralTTL    = time.Hour
)

// keyPEM encodes a private key.
func keyPEM(k crypto.Signer) string {
	switch k := k.(type) {
	case *rsa.PrivateKey:
		return string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)}))
	default:
		der, _ := x509.MarshalPKCS8PrivateKey(k)
		return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	}
}

func certPEM(der []byte) string {
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func parseKey(p string) (crypto.Signer, error) {
	b, _ := pem.Decode([]byte(p))
	if b == nil {
		return nil, errors.New("no PEM key")
	}
	if k, err := x509.ParsePKCS1PrivateKey(b.Bytes); err == nil {
		return k, nil
	}
	k, err := x509.ParsePKCS8PrivateKey(b.Bytes)
	if err != nil {
		return nil, err
	}
	s, ok := k.(crypto.Signer)
	if !ok {
		return nil, errors.New("unsupported key")
	}
	return s, nil
}

func parseCert(p string) (*x509.Certificate, error) {
	b, _ := pem.Decode([]byte(p))
	if b == nil {
		return nil, errors.New("no PEM certificate")
	}
	return x509.ParseCertificate(b.Bytes)
}

func serial() *big.Int {
	n, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 62))
	return n
}

func newECKey() *ecdsa.PrivateKey {
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	return k
}

// newCA returns a self-signed CA certificate and key.
func newCA(cn string) (certP, keyP string, err error) {
	k := newECKey()
	now := time.Now().Add(-5 * time.Minute)
	tmpl := &x509.Certificate{
		SerialNumber:          serial(),
		Subject:               pkix.Name{Country: []string{"US"}, Organization: []string{"Google, Inc"}, CommonName: cn},
		NotBefore:             now,
		NotAfter:              now.Add(caValidity),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, k.Public(), k)
	if err != nil {
		return "", "", err
	}
	return certPEM(der), keyPEM(k), nil
}

// sign issues a certificate from tmpl for pub, signed by the CA.
func sign(caCertP, caKeyP string, tmpl *x509.Certificate, pub crypto.PublicKey) ([]byte, error) {
	ca, err := parseCert(caCertP)
	if err != nil {
		return nil, err
	}
	key, err := parseKey(caKeyP)
	if err != nil {
		return nil, err
	}
	if tmpl.SerialNumber == nil {
		tmpl.SerialNumber = serial()
	}
	return x509.CreateCertificate(rand.Reader, tmpl, ca, pub, key)
}

// initPKI generates the instance's CAs and server certificate.
func initPKI(rec *instanceRecord, project, name string) error {
	var err error
	if rec.ServerCAPEM, rec.ServerCAKeyPEM, err = newCA(serverCACN); err != nil {
		return err
	}
	if rec.ClientCAPEM, rec.ClientCAKeyPEM, err = newCA(clientCACN); err != nil {
		return err
	}
	k := newECKey()
	now := time.Now().Add(-5 * time.Minute)
	der, err := sign(rec.ServerCAPEM, rec.ServerCAKeyPEM, &x509.Certificate{
		Subject:     pkix.Name{Country: []string{"US"}, Organization: []string{"Google, Inc"}, CommonName: project + ":" + name},
		NotBefore:   now,
		NotAfter:    now.Add(caValidity),
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}, k.Public())
	if err != nil {
		return err
	}
	rec.ServerCertPEM, rec.ServerKeyPEM = certPEM(der), keyPEM(k)
	return nil
}

// sslCertOf describes a PEM certificate as an sql#sslCert.
func sslCertOf(p, project, instance string) *sqladmin.SslCert {
	c, err := parseCert(p)
	if err != nil {
		return nil
	}
	fp := sha1.Sum(c.Raw)
	sha := hex.EncodeToString(fp[:])
	return &sqladmin.SslCert{
		Kind:             "sql#sslCert",
		Cert:             p,
		CertSerialNumber: c.SerialNumber.String(),
		CommonName:       dnString(c.Subject),
		CreateTime:       c.NotBefore.UTC().Format(time.RFC3339Nano),
		ExpirationTime:   c.NotAfter.UTC().Format(time.RFC3339Nano),
		Instance:         instance,
		Sha1Fingerprint:  sha,
		SelfLink:         instanceLink(project, instance) + "/sslCerts/" + sha,
	}
}

// dnString renders a subject the way Cloud SQL reports commonName for CA
// certificates ("C=US,O=Google\, Inc,CN=...") and the plain CN otherwise.
func dnString(n pkix.Name) string {
	if len(n.Country) == 0 {
		return n.CommonName
	}
	return "C=" + strings.Join(n.Country, ",") + ",O=" + strings.ReplaceAll(strings.Join(n.Organization, ","), ",", `\,`) + ",CN=" + n.CommonName
}

// newClientCert issues an sslCerts client certificate (RSA key, as GCP).
func newClientCert(rec *instanceRecord, commonName string) (certP, keyP string, err error) {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return "", "", err
	}
	now := time.Now().Add(-5 * time.Minute)
	der, err := sign(rec.ClientCAPEM, rec.ClientCAKeyPEM, &x509.Certificate{
		Subject:     pkix.Name{CommonName: commonName},
		NotBefore:   now,
		NotAfter:    now.Add(caValidity),
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}, k.Public())
	if err != nil {
		return "", "", err
	}
	return certPEM(der), keyPEM(k), nil
}

// parsePublicKey accepts the connector's "RSA PUBLIC KEY" PEM (which in
// practice holds PKIX bytes) and PKCS#1.
func parsePublicKey(p string) (crypto.PublicKey, error) {
	b, _ := pem.Decode([]byte(p))
	if b == nil {
		return nil, errors.New("public key is not PEM encoded")
	}
	if k, err := x509.ParsePKIXPublicKey(b.Bytes); err == nil {
		return k, nil
	}
	return x509.ParsePKCS1PublicKey(b.Bytes)
}

// newEphemeralCert issues a short-lived client certificate for pub. A
// non-empty principal (from the request's access token) is embedded for
// IAM database authentication on port 3307 (FR-SQL-006).
func newEphemeralCert(rec *instanceRecord, pub crypto.PublicKey, principal string, ttl time.Duration) (string, error) {
	if ttl <= 0 || ttl > ephemeralTTL {
		ttl = ephemeralTTL
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		Subject:     pkix.Name{CommonName: "Google Cloud SQL Client", OrganizationalUnit: []string{ephemeralOU}},
		NotBefore:   now.Add(-5 * time.Minute),
		NotAfter:    now.Add(ttl),
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	if principal != "" {
		tmpl.URIs = []*url.URL{{Scheme: principalScheme, Opaque: principal}}
	}
	der, err := sign(rec.ClientCAPEM, rec.ClientCAKeyPEM, tmpl, pub)
	if err != nil {
		return "", err
	}
	return certPEM(der), nil
}

// clientCertInfo is what the emulator learns from a client certificate
// presented to the agent.
type clientCertInfo struct {
	Ephemeral bool
	Principal string
	SHA1      string
}

// verifyClientCert checks der against the instance's client CA.
func verifyClientCert(rec *instanceRecord, der []byte) (*clientCertInfo, error) {
	c, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	ca, err := parseCert(rec.ClientCAPEM)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	if _, err := c.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		return nil, fmt.Errorf("client certificate: %w", err)
	}
	fp := sha1.Sum(c.Raw)
	info := &clientCertInfo{SHA1: hex.EncodeToString(fp[:])}
	for _, ou := range c.Subject.OrganizationalUnit {
		if ou == ephemeralOU {
			info.Ephemeral = true
		}
	}
	for _, u := range c.URIs {
		if u.Scheme == principalScheme {
			info.Principal = u.Opaque
		}
	}
	return info, nil
}
