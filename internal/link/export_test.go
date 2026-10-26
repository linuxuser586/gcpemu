package link

import (
	"context"
	"testing"
)

// FetchSigningKey asks the link name for its signing key, as a front
// Instance does.
func (m *Manager) FetchSigningKey(ctx context.Context, name string) (SigningKey, error) {
	return m.fetchSigningKey(ctx, name)
}

// AskIDToken asks the link name for the Principal of the ID token token,
// as LinkedIDToken does.
func (m *Manager) AskIDToken(ctx context.Context, name, token string) (string, error) {
	return m.askIDToken(ctx, name, token)
}

// AskLBFrontends asks the link name for its Load balancing frontends, as
// LinkedLBFrontends does, unfiltered.
func (m *Manager) AskLBFrontends(ctx context.Context, name string) ([]LBFrontend, error) {
	reply, err := m.askLBFrontends(ctx, name)
	return reply.Frontends, err
}

// AskIDTokensAt has Instances ask about ID tokens at path, which one that
// predates the route does not serve, until the test ends. Set it once the
// listeners serve.
func AskIDTokensAt(t *testing.T, path string) {
	old := idTokensRoute
	idTokensRoute = path
	t.Cleanup(func() { idTokensRoute = old })
}
