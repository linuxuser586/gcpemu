package link

import "context"

// FetchSigningKey asks the link name for its signing key, as a front
// Instance does.
func (m *Manager) FetchSigningKey(ctx context.Context, name string) (SigningKey, error) {
	return m.fetchSigningKey(ctx, name)
}
