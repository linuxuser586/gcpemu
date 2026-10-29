package ca

import (
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"math/big"
	"slices"
	"time"
)

// The empty CRL (ADR 0046). Windows' Schannel refuses a leaf whose
// revocation it can't check, so every leaf names a CRL on the loopback
// API gateway, which serves it signed by the current Instance CA on each
// request, revoking nothing.

// CRLPath is where the API gateway serves the CRL.
const CRLPath = "/ca.crl"

// CRLValidity is how long a CRL is valid (NextUpdate - ThisUpdate).
const CRLValidity = 7 * 24 * time.Hour

// CRLURL returns the CRL distribution point of an Instance whose API
// gateway listens on addr, a dialable host:port (emu.Dialable).
func CRLURL(addr string) string { return "http://" + addr + CRLPath }

// SetCRLURL sets the CRL distribution point of every leaf issued from now
// on, and drops the cached serving certificates so that ServerCert issues
// them again with it. The Instance calls it once the API gateway listens,
// before any Service starts.
func (c *CA) SetCRLURL(u string) {
	c.mu.Lock()
	c.crlURL = u
	c.cache = map[string]*tls.Certificate{}
	c.mu.Unlock()
}

// Current reports whether leaf was signed by the current Instance CA and
// names the current CRL URL as its only distribution point, so a stored
// leaf that isn't is issued again.
func (c *CA) Current(leaf *x509.Certificate) bool {
	c.mu.RLock()
	root, u := c.cert, c.crlURL
	c.mu.RUnlock()
	var want []string
	if u != "" {
		want = []string{u}
	}
	return slices.Equal(leaf.CRLDistributionPoints, want) && leaf.CheckSignatureFrom(root) == nil
}

// CurrentPEM is Current for the leaf at the head of a PEM chain; a chain
// that doesn't parse is not current.
func (c *CA) CurrentPEM(chainPEM []byte) bool {
	b, _ := pem.Decode(chainPEM)
	if b == nil {
		return false
	}
	leaf, err := x509.ParseCertificate(b.Bytes)
	return err == nil && c.Current(leaf)
}

// CRL returns a DER CRL revoking nothing, signed now by the current
// Instance CA. Its number is taken from the clock and only increases.
func (c *CA) CRL() ([]byte, error) {
	now := c.now()
	c.mu.Lock()
	n := big.NewInt(now.UnixNano())
	if c.crlNumber != nil && n.Cmp(c.crlNumber) <= 0 {
		n.Add(c.crlNumber, big.NewInt(1))
	}
	c.crlNumber = n
	root, signer := c.cert, c.key
	c.mu.Unlock()
	return x509.CreateRevocationList(rand.Reader, &x509.RevocationList{
		Number:     n,
		ThisUpdate: now,
		NextUpdate: now.Add(CRLValidity),
	}, root, signer)
}
