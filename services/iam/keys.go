package iam

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"net/url"
	"slices"
	"strings"
	"time"

	iamv1 "google.golang.org/api/iam/v1"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// Key types and origins (iam.v1 ServiceAccountKey).
const (
	keyUserManaged   = "USER_MANAGED"
	keySystemManaged = "SYSTEM_MANAGED"
	originGoogle     = "GOOGLE_PROVIDED"
	originUser       = "USER_PROVIDED"
	keyAlgRSA2048    = "KEY_ALG_RSA_2048"
)

// maxValidBefore is the validBeforeTime GCP reports on user-managed keys
// that never expire.
var maxValidBefore = time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC)

// keyRecord is a stored service-account key. Private material is kept only
// for system-managed keys (used by signBlob/signJwt); user-managed private
// keys are returned once at creation, as in GCP.
type keyRecord struct {
	Email       string    `json:"email"`
	ProjectID   string    `json:"projectId"`
	KeyID       string    `json:"keyId"`
	Type        string    `json:"keyType"`
	Origin      string    `json:"keyOrigin"`
	Algorithm   string    `json:"keyAlgorithm"`
	CertPEM     string    `json:"certPem"`
	PrivatePEM  string    `json:"privatePem,omitempty"`
	ValidAfter  time.Time `json:"validAfter"`
	ValidBefore time.Time `json:"validBefore"`
	Disabled    bool      `json:"disabled,omitempty"`
}

func keyKey(email, id string) string { return strings.ToLower(email) + "/" + id }

func (k *keyRecord) name() string {
	return "projects/" + k.ProjectID + "/serviceAccounts/" + k.Email + "/keys/" + k.KeyID
}

// publicKey parses the record's certificate.
func (k *keyRecord) publicKey() (*rsa.PublicKey, error) {
	cert, err := parseCertPEM([]byte(k.CertPEM))
	if err != nil {
		return nil, err
	}
	pub, ok := cert.PublicKey.(*rsa.PublicKey)
	if !ok {
		return nil, errors.New("not an RSA key")
	}
	return pub, nil
}

func (k *keyRecord) privateKey() (*rsa.PrivateKey, error) {
	blk, _ := pem.Decode([]byte(k.PrivatePEM))
	if blk == nil {
		return nil, errors.New("no private key")
	}
	key, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
	if err != nil {
		return nil, err
	}
	rk, ok := key.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("not an RSA key")
	}
	return rk, nil
}

// toAPI renders the key; publicKeyType selects publicKeyData ("" omits it).
func (k *keyRecord) toAPI(publicKeyType string) *iamv1.ServiceAccountKey {
	out := &iamv1.ServiceAccountKey{
		Name:            k.name(),
		KeyAlgorithm:    k.Algorithm,
		KeyOrigin:       k.Origin,
		KeyType:         k.Type,
		ValidAfterTime:  k.ValidAfter.UTC().Format(time.RFC3339),
		ValidBeforeTime: k.ValidBefore.UTC().Format(time.RFC3339),
		Disabled:        k.Disabled,
	}
	switch publicKeyType {
	case "TYPE_X509_PEM_FILE":
		out.PublicKeyData = base64.StdEncoding.EncodeToString([]byte(k.CertPEM))
	case "TYPE_RAW_PUBLIC_KEY":
		if pub, err := k.publicKey(); err == nil {
			der, _ := x509.MarshalPKIXPublicKey(pub)
			out.PublicKeyData = base64.StdEncoding.EncodeToString(der)
		}
	}
	return out
}

func parseCertPEM(b []byte) (*x509.Certificate, error) {
	blk, _ := pem.Decode(b)
	if blk == nil || blk.Type != "CERTIFICATE" {
		return nil, errors.New("no PEM certificate found")
	}
	return x509.ParseCertificate(blk.Bytes)
}

// newKeyPair generates an RSA-2048 key and a self-signed certificate for it.
func newKeyPair(subject string, notBefore, notAfter time.Time) (*rsa.PrivateKey, string, error) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, "", err
	}
	certPEM, err := selfSignedCert(priv, subject, notBefore, notAfter)
	return priv, certPEM, err
}

func selfSignedCert(priv *rsa.PrivateKey, subject string, notBefore, notAfter time.Time) (string, error) {
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 63))
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: subject},
		NotBefore:    notBefore.Add(-time.Minute),
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		return "", err
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), nil
}

func privatePEM(priv *rsa.PrivateKey) string {
	der, _ := x509.MarshalPKCS8PrivateKey(priv)
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}

// keyFile is the JSON key file format (TYPE_GOOGLE_CREDENTIALS_FILE).
type keyFile struct {
	Type                    string `json:"type"`
	ProjectID               string `json:"project_id"`
	PrivateKeyID            string `json:"private_key_id"`
	PrivateKey              string `json:"private_key"`
	ClientEmail             string `json:"client_email"`
	ClientID                string `json:"client_id"`
	AuthURI                 string `json:"auth_uri"`
	TokenURI                string `json:"token_uri"`
	AuthProviderX509CertURL string `json:"auth_provider_x509_cert_url"`
	ClientX509CertURL       string `json:"client_x509_cert_url"`
	UniverseDomain          string `json:"universe_domain"`
}

// gatewayURL is the emulator's base URL for links embedded in credentials.
func (s *Service) gatewayURL() string {
	return "http://" + s.env.Endpoints.Get("gateway")
}

// tokenURI is the OAuth2 token endpoint advertised in key files (FR-IAM-006).
func (s *Service) tokenURI() string { return s.gatewayURL() + "/oauth2/token" }

// createKey creates a user-managed key and returns it with the JSON key
// file (FR-IAM-001).
func (s *Service) createKey(sa *iamv1.ServiceAccount) (*keyRecord, []byte, error) {
	now := s.env.Clock.Now().UTC().Truncate(time.Second)
	priv, certPEM, err := newKeyPair(sa.Email, now, maxValidBefore)
	if err != nil {
		return nil, nil, err
	}
	rec := &keyRecord{
		Email: sa.Email, ProjectID: sa.ProjectId, KeyID: s.env.IDs.Hex(20),
		Type: keyUserManaged, Origin: originGoogle, Algorithm: keyAlgRSA2048,
		CertPEM: certPEM, ValidAfter: now, ValidBefore: maxValidBefore,
	}
	kf, _ := json.MarshalIndent(keyFile{
		Type: "service_account", ProjectID: sa.ProjectId, PrivateKeyID: rec.KeyID,
		PrivateKey: privatePEM(priv), ClientEmail: sa.Email, ClientID: sa.UniqueId,
		AuthURI:                 "https://accounts.google.com/o/oauth2/auth",
		TokenURI:                s.tokenURI(),
		AuthProviderX509CertURL: s.gatewayURL() + "/oauth2/v1/certs",
		ClientX509CertURL:       s.gatewayURL() + "/service_accounts/v1/metadata/x509/" + url.PathEscape(sa.Email),
		UniverseDomain:          "googleapis.com",
	}, "", "  ")
	err = s.env.Store.Update(func(tx store.Tx) error {
		return store.PutJSON(tx, nsKeys, keyKey(sa.Email, rec.KeyID), rec)
	})
	if err != nil {
		return nil, nil, err
	}
	return rec, kf, nil
}

// uploadKey registers a user-provided X.509 certificate.
func (s *Service) uploadKey(sa *iamv1.ServiceAccount, certPEM []byte) (*keyRecord, error) {
	cert, err := parseCertPEM(certPEM)
	if err != nil {
		return nil, apierr.InvalidArgument("Invalid public key data: %v", err).WithReason(iamDomain, "INVALID_PUBLIC_KEY")
	}
	pub, ok := cert.PublicKey.(*rsa.PublicKey)
	if !ok {
		return nil, apierr.InvalidArgument("Only RSA public keys are supported.").WithReason(iamDomain, "INVALID_PUBLIC_KEY")
	}
	alg := keyAlgRSA2048
	if pub.N.BitLen() > 2048 {
		alg = "KEY_ALG_RSA_4096"
	}
	sum := sha256.Sum256(cert.Raw)
	rec := &keyRecord{
		Email: sa.Email, ProjectID: sa.ProjectId, KeyID: hex.EncodeToString(sum[:20]),
		Type: keyUserManaged, Origin: originUser, Algorithm: alg,
		CertPEM:    string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})),
		ValidAfter: cert.NotBefore.UTC(), ValidBefore: cert.NotAfter.UTC(),
	}
	err = s.env.Store.Update(func(tx store.Tx) error {
		if store.Exists(tx, nsKeys, keyKey(sa.Email, rec.KeyID)) {
			return apierr.AlreadyExists("The given public key already exists for this service account.").WithReason(iamDomain, "KEY_ALREADY_EXISTS")
		}
		return store.PutJSON(tx, nsKeys, keyKey(sa.Email, rec.KeyID), rec)
	})
	return rec, err
}

// listKeys returns the keys of an account (all types when types is empty).
func listKeys(tx store.Tx, email string, types ...string) []*keyRecord {
	var out []*keyRecord
	tx.Scan(nsKeys, strings.ToLower(email)+"/", func(_ string, b []byte) bool {
		var k keyRecord
		if json.Unmarshal(b, &k) == nil && (len(types) == 0 || slices.Contains(types, k.Type)) {
			out = append(out, &k)
		}
		return true
	})
	return out
}

// systemKey returns (creating on first use) the account's Google-managed
// signing key used by signBlob, signJwt and V4 signing (FR-IAM-007).
func (s *Service) systemKey(sa *iamv1.ServiceAccount) (*keyRecord, *rsa.PrivateKey, error) {
	var rec *keyRecord
	_ = s.env.Store.View(func(tx store.Tx) error {
		for _, k := range listKeys(tx, sa.Email, keySystemManaged) {
			if !k.Disabled && k.PrivatePEM != "" {
				rec = k
				break
			}
		}
		return nil
	})
	if rec == nil {
		now := s.env.Clock.Now().UTC().Truncate(time.Second)
		valid := now.AddDate(10, 0, 0)
		priv, certPEM, err := newKeyPair(sa.Email, now, valid)
		if err != nil {
			return nil, nil, err
		}
		nk := &keyRecord{
			Email: sa.Email, ProjectID: sa.ProjectId, KeyID: s.env.IDs.Hex(20),
			Type: keySystemManaged, Origin: originGoogle, Algorithm: keyAlgRSA2048,
			CertPEM: certPEM, PrivatePEM: privatePEM(priv), ValidAfter: now, ValidBefore: valid,
		}
		err = s.env.Store.Update(func(tx store.Tx) error {
			for _, k := range listKeys(tx, sa.Email, keySystemManaged) {
				if !k.Disabled && k.PrivatePEM != "" { // lost a race
					rec = k
					return nil
				}
			}
			rec = nk
			return store.PutJSON(tx, nsKeys, keyKey(sa.Email, nk.KeyID), nk)
		})
		if err != nil {
			return nil, nil, err
		}
	}
	priv, err := rec.privateKey()
	return rec, priv, err
}

// lookupActive resolves an account that must exist and be enabled.
func (s *Service) lookupActive(email string) (*iamv1.ServiceAccount, error) {
	var sa *iamv1.ServiceAccount
	var ok bool
	_ = s.env.Store.View(func(tx store.Tx) error { sa, ok = s.getAccount(tx, email); return nil })
	if !ok {
		return nil, notFoundSA()
	}
	if sa.Disabled {
		return nil, apierr.FailedPrecondition("Service account %s is disabled.", sa.Email).WithReason(iamDomain, "SERVICE_ACCOUNT_DISABLED")
	}
	return sa, nil
}

// PublicKeys implements emu.ServiceAccountKeys: every enabled key of the
// account, user- and system-managed.
func (s *Service) PublicKeys(ctx context.Context, email string) ([]*rsa.PublicKey, error) {
	var out []*rsa.PublicKey
	err := s.env.Store.View(func(tx store.Tx) error {
		if _, ok := s.getAccount(tx, email); !ok {
			return notFoundSA()
		}
		for _, k := range listKeys(tx, email) {
			if k.Disabled {
				continue
			}
			if pub, err := k.publicKey(); err == nil {
				out = append(out, pub)
			}
		}
		return nil
	})
	return out, err
}

// SignBlob implements emu.ServiceAccountKeys with the system-managed key.
func (s *Service) SignBlob(ctx context.Context, email string, data []byte) (string, []byte, error) {
	sa, err := s.lookupActive(email)
	if err != nil {
		return "", nil, err
	}
	return s.signWithSystemKey(sa, data)
}

func (s *Service) signWithSystemKey(sa *iamv1.ServiceAccount, data []byte) (string, []byte, error) {
	rec, priv, err := s.systemKey(sa)
	if err != nil {
		return "", nil, err
	}
	sum := sha256.Sum256(data)
	sig, err := rsa.SignPKCS1v15(rand.Reader, priv, crypto.SHA256, sum[:])
	return rec.KeyID, sig, err
}
