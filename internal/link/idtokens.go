package link

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/linuxuser586/gcpemu/internal/project"
)

// ID tokens (ADR 0027): a service account in a Delegated Project gets its
// ID tokens from the Linked Instance, signed with its Signing key. When one is
// exchanged at the front Instance's STS, for a provider that trusts the
// Instance's own issuer, the front Instance, which did not sign it, asks
// the Linked Instances it delegates to whether one of them did. Unlike
// access tokens, answers are not kept.

// idTokensRoute is the path of serveIDToken.
var idTokensRoute = "/link/v1/id-tokens"

// serveIDToken answers a paired Instance with the service account of an
// ID token this Instance signed, for a Project shared with it only, and
// "" for any other. It does not check the token's audience.
func (m *Manager) serveIDToken(w http.ResponseWriter, r *http.Request) {
	p, _, ok := m.admit(w, r)
	if !ok {
		return
	}
	var body struct{ Token string }
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&body); err != nil {
		m.refuse(w, http.StatusBadRequest, "", "invalid ID token request: "+err.Error())
		return
	}
	principal := ""
	if m.cfg.VerifyIDToken != nil && body.Token != "" {
		if email, ok := m.cfg.VerifyIDToken(body.Token); ok && m.Shared(p.Name, project.OfServiceAccount(email)) {
			principal = "serviceAccount:" + email
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"principal": principal})
}

// askIDToken asks the link name for the Principal of the ID token token.
// A Linked Instance that predates the route (404) does not claim it.
func (m *Manager) askIDToken(ctx context.Context, name, token string) (string, error) {
	l, err := m.usable(name)
	if err != nil {
		return "", err
	}
	var reply struct{ Principal string }
	_, err = m.call(ctx, l.Address, http.MethodPost, idTokensRoute, l.pinned(), map[string]string{"token": token}, &reply)
	var se *statusError
	if errors.As(err, &se) && se.code == http.StatusNotFound {
		return "", nil
	}
	return reply.Principal, err
}

// LinkedIDToken returns the service account Principal of the ID token
// token, which this Instance did not sign, as a Linked Instance it
// delegates to signed it, and the links it could not ask. A Linked
// Instance speaks only for the service accounts of the Projects delegated
// to it.
func (m *Manager) LinkedIDToken(ctx context.Context, token string) (principal string, unreachable []string) {
	for _, name := range m.DelegatedLinks() {
		p, err := m.askIDToken(ctx, name, token)
		if err != nil {
			m.cfg.Log.Debug("could not ask a Linked Instance about an ID token", "link", name, "err", err)
			unreachable = append(unreachable, name)
			continue
		}
		if to, ok := m.DelegatedTo(principalProject(p)); ok && to == name && strings.HasPrefix(p, "serviceAccount:") {
			return p, nil
		}
	}
	return "", unreachable
}
