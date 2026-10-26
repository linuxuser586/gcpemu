package link

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/linuxuser586/gcpemu/internal/project"
)

// Tokens (ADR 0027): a service account in a Delegated Project gets its
// access tokens from the Linked Instance, through the front Instance's
// token endpoint or generateAccessToken, and a Federated principal of a
// pool in one through the Linked Instance's STS. When a caller presents one, the front
// Instance, which did not issue it, asks the Linked Instances it
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
// Instance issued, for a service account or a Federated principal of a
// Project shared with it only, and "" for any other.
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
			if id := principalProject(got); id != "" && m.Shared(p.Name, id) {
				principal = got
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"principal": principal})
}

// Authenticate returns the Principal of token, which this Instance did not
// issue, as a Linked Instance it delegates to issued it, and the links it
// could not ask, with why: broken, with no --link-bind address, or not
// answering.
func (m *Manager) Authenticate(ctx context.Context, token string) (principal string, unreachable map[string]error) {
	now := time.Now()
	m.mu.Lock()
	a, ok := m.tokens[token]
	m.mu.Unlock()
	if ok && now.Before(a.expires) {
		return a.principal, nil
	}
	for _, name := range m.DelegatedLinks() {
		p, err := m.askToken(ctx, name, token)
		if err != nil {
			m.cfg.Log.Debug("could not ask a Linked Instance about a token", "link", name, "err", err)
			if unreachable == nil {
				unreachable = map[string]error{}
			}
			unreachable[name] = err
			continue
		}
		// A Linked Instance speaks only for the service accounts and
		// Federated principals of the Projects delegated to it, never for
		// this Instance's own.
		if to, ok := m.DelegatedTo(principalProject(p)); ok && to == name {
			principal, unreachable = p, nil
			break
		}
	}
	// An answer that missed an unreachable Linked Instance is not kept, so
	// that its token works once it is back.
	if principal != "" || len(unreachable) == 0 {
		m.mu.Lock()
		if len(m.tokens) >= maxTokens {
			clear(m.tokens)
		}
		m.tokens[token] = tokenAnswer{principal, now.Add(tokenTTL)}
		m.mu.Unlock()
	}
	return principal, unreachable
}

// askToken asks the link name for the Principal of the access token token.
func (m *Manager) askToken(ctx context.Context, name, token string) (string, error) {
	l, err := m.usable(name)
	if err != nil {
		return "", err
	}
	var reply struct{ Principal string }
	_, err = m.call(ctx, l.Address, http.MethodPost, "/link/v1/tokens", l.pinned(), map[string]string{"token": token}, &reply)
	return reply.Principal, err
}

// federatedPrincipal matches the Project of a Federated principal: a
// subject of a workload identity pool.
var federatedPrincipal = regexp.MustCompile(`^principal://iam\.googleapis\.com/projects/([^/]+)/locations/global/workloadIdentityPools/[^/]+/subject/.+$`)

// principalProject returns the Project, an ID or number, of a service
// account or a Federated principal, or "" for any other Principal.
func principalProject(p string) string {
	if email, ok := strings.CutPrefix(p, "serviceAccount:"); ok {
		return project.OfServiceAccount(email)
	}
	if m := federatedPrincipal.FindStringSubmatch(p); m != nil {
		return m[1]
	}
	return ""
}
