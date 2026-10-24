package link

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/linuxuser586/gcpemu/internal/project"
)

// Tokens (ADR 0027): a service account in a Delegated Project gets its
// access tokens from the Linked Instance, through the front Instance's
// token endpoint or generateAccessToken. When a caller presents one, the
// front Instance, which did not issue it, asks the Linked Instances it
// delegates to which Principal it is, and asserts that Principal.

// tokenTTL is how long the front Instance keeps what a Linked Instance
// said of a token, and maxTokens how many it keeps.
const (
	tokenTTL  = 30 * time.Second
	maxTokens = 1024
)

// tokenAnswer is what Linked Instances said of a token, until expires.
type tokenAnswer struct {
	principal string
	expires   time.Time
}

// serveToken answers a paired Instance with the Principal of a token this
// Instance issued, for a service account in a Project shared with it
// only, and "" for any other.
func (m *Manager) serveToken(w http.ResponseWriter, r *http.Request) {
	p, _, ok := m.admit(w, r)
	if !ok {
		return
	}
	var body struct{ Token string }
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&body); err != nil {
		m.refuse(w, http.StatusBadRequest, "", "invalid token request: "+err.Error())
		return
	}
	principal := ""
	if m.cfg.Authenticate != nil && body.Token != "" {
		if got, ok := m.cfg.Authenticate(r.Context(), body.Token); ok {
			email, isAccount := strings.CutPrefix(got, "serviceAccount:")
			if id := project.OfServiceAccount(email); isAccount && id != "" && m.Shared(p.Name, id) {
				principal = got
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"principal": principal})
}

// Authenticate returns the Principal of token, which this Instance did not
// issue, as a Linked Instance it delegates to issued it.
func (m *Manager) Authenticate(ctx context.Context, token string) (string, bool) {
	now := time.Now()
	m.mu.Lock()
	a, ok := m.tokens[token]
	m.mu.Unlock()
	if ok && now.Before(a.expires) {
		return a.principal, a.principal != ""
	}
	principal, reached := "", true
	for _, name := range m.DelegatedLinks() {
		l, err := m.usable(name)
		if err != nil {
			continue
		}
		var reply struct{ Principal string }
		if _, err := m.call(ctx, l.Address, http.MethodPost, "/link/v1/tokens", l.pinned(), map[string]string{"token": token}, &reply); err != nil {
			m.cfg.Log.Debug("could not ask a Linked Instance about a token", "link", name, "err", err)
			reached = false
			continue
		}
		// A Linked Instance speaks only for the service accounts of the
		// Projects delegated to it, never for this Instance's own.
		email, _ := strings.CutPrefix(reply.Principal, "serviceAccount:")
		if to, ok := m.DelegatedTo(project.OfServiceAccount(email)); ok && to == name && strings.HasPrefix(reply.Principal, "serviceAccount:") {
			principal = reply.Principal
			break
		}
	}
	// An answer that missed an unreachable Linked Instance is not kept, so
	// that its token works once it is back.
	if principal != "" || reached {
		m.mu.Lock()
		if len(m.tokens) >= maxTokens {
			clear(m.tokens)
		}
		m.tokens[token] = tokenAnswer{principal, now.Add(tokenTTL)}
		m.mu.Unlock()
	}
	return principal, principal != ""
}
