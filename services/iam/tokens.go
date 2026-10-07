package iam

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/golang-jwt/jwt/v5"
	iamv1 "google.golang.org/api/iam/v1"

	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// Token issuance (FR-IAM-006, FR-IAM-007, FR-CORE-051).

const (
	// tokenLifetime is the default access/ID token lifetime.
	tokenLifetime = time.Hour
	// oidcIssuer is the iss claim of ID tokens; clients that hard-code
	// Google's issuer accept it, and the emulator's JWKS verifies it.
	oidcIssuer = "https://accounts.google.com"
	// tokenPrefix marks emulator access tokens.
	tokenPrefix = "ya29.gcpemu."
)

// tokenRecord is a stored opaque access token.
type tokenRecord struct {
	Principal string    `json:"principal"`
	Scopes    []string  `json:"scopes,omitempty"`
	ClientID  string    `json:"clientId,omitempty"`
	Issued    time.Time `json:"issued"`
	Expiry    time.Time `json:"expiry"`
}

// signingKey is the emulator's OIDC signing key, persisted 0600 under the
// service data dir so ID tokens stay verifiable across restarts.
type signingKey struct {
	priv    *rsa.PrivateKey
	kid     string
	certPEM string
}

// loadSigningKey reads or creates dir/signing-key.pem (NFR-SEC-003).
func loadSigningKey(dir string, now time.Time) (*signingKey, error) {
	path := filepath.Join(dir, "signing-key.pem")
	var priv *rsa.PrivateKey
	if b, err := os.ReadFile(path); err == nil {
		blk, _ := pem.Decode(b)
		if blk == nil {
			return nil, fmt.Errorf("%s: no PEM data", path)
		}
		k, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		var ok bool
		if priv, ok = k.(*rsa.PrivateKey); !ok {
			return nil, fmt.Errorf("%s: not an RSA key", path)
		}
	} else if errors.Is(err, os.ErrNotExist) {
		if priv, err = rsa.GenerateKey(rand.Reader, 2048); err != nil {
			return nil, err
		}
		tmp := path + ".tmp"
		if err := os.WriteFile(tmp, []byte(privatePEM(priv)), 0o600); err != nil {
			return nil, err
		}
		if err := os.Rename(tmp, path); err != nil {
			return nil, err
		}
	} else {
		return nil, err
	}
	der, _ := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	sum := sha256.Sum256(der)
	cert, err := selfSignedCert(priv, "gcpemu-oidc", now, now.AddDate(10, 0, 0))
	if err != nil {
		return nil, err
	}
	return &signingKey{priv: priv, kid: hex.EncodeToString(sum[:20]), certPEM: cert}, nil
}

// jwks renders the public key as a JSON Web Key Set.
func (k *signingKey) jwks() map[string]any {
	return map[string]any{"keys": []map[string]string{{
		"kty": "RSA", "alg": "RS256", "use": "sig", "kid": k.kid,
		"n": b64url(k.priv.N.Bytes()),
		"e": b64url(bigEndian(k.priv.E)),
	}}}
}

func b64url(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// bigEndian encodes an RSA public exponent.
func bigEndian(e int) []byte { return big.NewInt(int64(e)).Bytes() }

var mintCount atomic.Uint64

// mintAccessToken issues an opaque access token for p and persists it.
func (s *Service) mintAccessToken(p emu.Principal, scopes []string, clientID string, lifetime time.Duration) (string, time.Time, error) {
	if lifetime <= 0 {
		lifetime = tokenLifetime
	}
	var b [32]byte
	_, _ = rand.Read(b[:])
	tok := tokenPrefix + base64.RawURLEncoding.EncodeToString(b[:])
	now := s.env.Clock.Now().UTC()
	rec := tokenRecord{Principal: string(p), Scopes: scopes, ClientID: clientID, Issued: now, Expiry: now.Add(lifetime)}
	prune := mintCount.Add(1)%256 == 0
	err := s.env.Store.Update(func(tx store.Tx) error {
		if prune {
			var expired []string
			tx.Scan(nsTokens, "", func(k string, v []byte) bool {
				var r tokenRecord
				if json.Unmarshal(v, &r) == nil && now.After(r.Expiry) {
					expired = append(expired, k)
				}
				return true
			})
			for _, k := range expired {
				_ = tx.Delete(nsTokens, k)
			}
		}
		return store.PutJSON(tx, nsTokens, tok, rec)
	})
	return tok, rec.Expiry, err
}

// lookupToken returns an unexpired token record.
func (s *Service) lookupToken(tok string) (*tokenRecord, bool) {
	var rec tokenRecord
	var found bool
	_ = s.env.Store.View(func(tx store.Tx) error {
		found = store.GetJSON(tx, nsTokens, tok, &rec) == nil
		return nil
	})
	if !found || s.env.Clock.Now().After(rec.Expiry) {
		return nil, false
	}
	return &rec, true
}

// Authenticate implements emu.Authenticator (FR-CORE-051): emulator access
// tokens and self-signed service-account JWTs (as sent by client libraries
// using a JSON key without a token exchange) identify the caller.
func (s *Service) Authenticate(ctx context.Context, tok string) (emu.Principal, bool) {
	if rec, ok := s.lookupToken(tok); ok {
		if email, isSA := strings.CutPrefix(rec.Principal, "serviceAccount:"); isSA && s.accountDisabled(email) {
			return "", false
		}
		return emu.Principal(rec.Principal), true
	}
	if strings.Count(tok, ".") == 2 {
		if email, err := s.verifyAccountJWT(tok, ""); err == nil {
			return emu.Principal("serviceAccount:" + email), true
		}
	}
	return "", false
}

func (s *Service) accountDisabled(email string) bool {
	var disabled bool
	_ = s.env.Store.View(func(tx store.Tx) error {
		if sa, ok := s.getAccount(tx, email); ok {
			disabled = sa.Disabled
		}
		return nil
	})
	return disabled
}

// verifyAccountJWT verifies a JWT signed with one of a service account's
// keys (iss = account email, kid = key ID) and returns the account email.
// aud, when non-empty, must be among the token's audiences.
func (s *Service) verifyAccountJWT(tok, aud string) (string, error) {
	var email string
	opts := []jwt.ParserOption{
		jwt.WithValidMethods([]string{"RS256"}),
		jwt.WithTimeFunc(s.env.Clock.Now),
		jwt.WithLeeway(5 * time.Minute),
		jwt.WithExpirationRequired(),
	}
	if aud != "" {
		opts = append(opts, jwt.WithAudience(aud))
	}
	_, err := jwt.Parse(tok, func(t *jwt.Token) (any, error) {
		claims, _ := t.Claims.(jwt.MapClaims)
		iss, _ := claims["iss"].(string)
		kid, _ := t.Header["kid"].(string)
		if iss == "" {
			return nil, errors.New("missing iss")
		}
		var key *rsa.PublicKey
		err := s.env.Store.View(func(tx store.Tx) error {
			sa, ok := s.getAccount(tx, iss)
			if !ok {
				return fmt.Errorf("unknown service account %s", iss)
			}
			if sa.Disabled {
				return fmt.Errorf("service account %s is disabled", iss)
			}
			email = sa.Email
			for _, k := range listKeys(tx, sa.Email) {
				if k.Disabled || (kid != "" && k.KeyID != kid) {
					continue
				}
				if pub, err := k.publicKey(); err == nil {
					key = pub
					if kid != "" {
						break
					}
				}
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		if key == nil {
			return nil, errors.New("no matching key")
		}
		return key, nil
	}, opts...)
	return email, err
}

// mintIDToken signs an OIDC ID token with the emulator key.
func (s *Service) mintIDToken(email, audience string, includeEmail bool, extra map[string]any) (string, error) {
	if audience == "" {
		return "", errors.New("audience required")
	}
	now := s.env.Clock.Now().UTC()
	sub := uniqueIDFor(email)
	_ = s.env.Store.View(func(tx store.Tx) error {
		if sa, ok := s.getAccount(tx, email); ok {
			sub = sa.UniqueId
		}
		return nil
	})
	claims := jwt.MapClaims{
		"iss": oidcIssuer,
		"aud": audience,
		"azp": sub,
		"sub": sub,
		"iat": now.Unix(),
		"exp": now.Add(tokenLifetime).Unix(),
	}
	if includeEmail {
		claims["email"] = email
		claims["email_verified"] = true
	}
	for k, v := range extra {
		claims[k] = v
	}
	t := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	t.Header["kid"] = s.signer.kid
	return t.SignedString(s.signer.priv)
}

// IDToken implements emu.ServiceAccountKeys (FR-IAM-007, FR-PS-006).
func (s *Service) IDToken(ctx context.Context, email, audience string) (string, error) {
	return s.mintIDToken(email, audience, true, nil)
}

// AccessToken implements emu.ServiceAccountKeys.
func (s *Service) AccessToken(ctx context.Context, p emu.Principal) (string, int, error) {
	tok, exp, err := s.mintAccessToken(p, []string{cloudPlatformScope}, "", tokenLifetime)
	if err != nil {
		return "", 0, err
	}
	return tok, int(exp.Sub(s.env.Clock.Now()).Seconds()), nil
}

const cloudPlatformScope = "https://www.googleapis.com/auth/cloud-platform"

// signJWTWithSystemKey signs a JWT payload (JSON claims) with the
// account's system-managed key.
func (s *Service) signJWTWithSystemKey(sa *iamv1.ServiceAccount, payload string) (string, string, error) {
	var claims jwt.MapClaims
	if err := json.Unmarshal([]byte(payload), &claims); err != nil {
		return "", "", fmt.Errorf("payload is not a JSON object: %w", err)
	}
	rec, priv, err := s.systemKey(sa)
	if err != nil {
		return "", "", err
	}
	t := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	t.Header["kid"] = rec.KeyID
	signed, err := t.SignedString(priv)
	return rec.KeyID, signed, err
}
