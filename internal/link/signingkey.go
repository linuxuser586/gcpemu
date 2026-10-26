package link

import (
	"context"
	"errors"
	"net/http"
	"slices"
)

// ID token signing keys (ADR 0027): a service account in a Delegated
// Project gets its ID tokens from the Linked Instance, which signs them
// with its own key, but clients verify them against the front Instance's
// certificate endpoints. The front Instance therefore fetches the signing
// key of each link it delegates a Project to whenever it connects to
// watch that link's buckets, keeps the last one it heard while the
// Linked Instance cannot be reached, and forgets it once nothing is
// delegated to that link. The keys are held in memory only.

// SigningKey is an Instance's ID token signing key, as its certificate
// endpoints list it.
type SigningKey struct {
	KeyID string `json:"keyId"`
	// JWK is the public key as a JSON Web Key.
	JWK     map[string]string `json:"jwk"`
	CertPEM string            `json:"certPem"`
}

// serveSigningKey answers a paired Instance with this Instance's signing
// key. One without a key answers 404, as an Instance that predates this
// route does.
func (m *Manager) serveSigningKey(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := m.admit(w, r); !ok {
		return
	}
	var k SigningKey
	ok := false
	if m.cfg.SigningKey != nil {
		k, ok = m.cfg.SigningKey()
	}
	if !ok {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, http.StatusOK, k)
}

// fetchSigningKey asks the link name for its signing key.
func (m *Manager) fetchSigningKey(ctx context.Context, name string) (SigningKey, error) {
	var k SigningKey
	l, err := m.usable(name)
	if err != nil {
		return k, err
	}
	_, err = m.callPaired(ctx, l, http.MethodGet, "/link/v1/signing-key", &k)
	return k, err
}

// refreshSigningKey asks the link name for its signing key now, for no
// longer than bucketRetry, so that a Linked Instance that hangs does not
// hold up its buckets. While it cannot be reached, the key it last sent
// is kept; one that has none (404) has it forgotten.
func (m *Manager) refreshSigningKey(ctx context.Context, name string) {
	ctx, cancel := context.WithTimeout(ctx, bucketRetry)
	defer cancel()
	k, err := m.fetchSigningKey(ctx, name)
	var se *statusError
	switch {
	case errors.As(err, &se) && se.code == http.StatusNotFound, err == nil && k.KeyID == "":
		m.setSigningKey(name, nil)
	case err != nil:
		m.cfg.Log.Debug("could not ask a Linked Instance for its signing key", "link", name, "err", err)
	default:
		m.setSigningKey(name, &k)
	}
}

// setSigningKey keeps the signing key of the link name, while it is
// watched; nil forgets it.
func (m *Manager) setSigningKey(name string, k *SigningKey) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, watched := m.watchers[name]; !watched {
		return
	}
	if k == nil {
		delete(m.signingKeys, name)
		return
	}
	m.signingKeys[name] = *k
}

// LinkedSigningKeys returns the signing keys of the links this Instance
// delegates a Project to, as last heard, by link. It never waits on a
// Linked Instance.
func (m *Manager) LinkedSigningKeys() []SigningKey {
	m.mu.Lock()
	defer m.mu.Unlock()
	names := make([]string, 0, len(m.signingKeys))
	for name := range m.signingKeys {
		names = append(names, name)
	}
	slices.Sort(names)
	out := make([]SigningKey, len(names))
	for i, name := range names {
		out[i] = m.signingKeys[name]
	}
	return out
}
