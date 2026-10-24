package link

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"net/http/httputil"
	"strings"
	"time"
)

// Forwarding (ADR 0027): the front Instance forwards a request naming a
// Delegated Project over the link, under forwardPrefix, asserting its
// caller's Principal; the Linked Instance serves it as its own API gateway
// would, taking the asserted Principal as the caller.

const (
	forwardPrefix = "/link/v1/api"
	// principalHeader carries the Principal the front Instance asserts.
	principalHeader = "Gcpemu-Link-Principal"
	// refusedHeader marks a link refusal, so that the front Instance tells
	// it apart from the forwarded request's own answer.
	refusedHeader = "Gcpemu-Link-Refused"
)

// ServeFunc serves a request forwarded over the link from, whose caller
// the other Instance asserts is principal.
type ServeFunc func(w http.ResponseWriter, r *http.Request, from, principal string)

// serveForwarded serves a forwarded request from a paired Instance.
func (m *Manager) serveForwarded(w http.ResponseWriter, r *http.Request) {
	p, _, ok := m.admit(w, r)
	if !ok {
		return
	}
	if m.cfg.Serve == nil {
		m.refuse(w, http.StatusNotFound, "", fmt.Sprintf("Instance %q serves no forwarded requests", m.cfg.Instance))
		return
	}
	principal := r.Header.Get(principalHeader)
	for _, h := range []string{principalHeader, versionsHeader, instanceIDHeader} {
		r.Header.Del(h)
	}
	r.URL.Path = strings.TrimPrefix(r.URL.Path, forwardPrefix)
	r.URL.RawPath = strings.TrimPrefix(r.URL.RawPath, forwardPrefix)
	r.RequestURI = r.URL.RequestURI()
	m.cfg.Serve(w, r, p.Name, principal)
}

// Forward forwards r over the link name, asserting that its caller is
// principal, and writes the Linked Instance's answer, which answered, when
// not nil, sees first. When the link cannot be used, the Linked Instance
// cannot be reached or it refuses the link, Forward instead calls fail
// with an error naming the link, having written nothing.
func (m *Manager) Forward(w http.ResponseWriter, r *http.Request, name, principal string, fail func(http.ResponseWriter, error), answered func(*http.Response)) {
	p, err := m.usable(name)
	if err != nil {
		fail(w, fmt.Errorf("the Linked Instance %q is unavailable: %w", name, err))
		return
	}
	link := p
	proxy := &httputil.ReverseProxy{
		Transport: m.transport(link),
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme, pr.Out.URL.Host = "https", Dialable(link.Address)
			pr.Out.URL.Path = forwardPrefix + pr.In.URL.Path
			if pr.In.URL.RawPath != "" {
				pr.Out.URL.RawPath = forwardPrefix + pr.In.URL.RawPath
			}
			pr.Out.Host = pr.In.Host
			// The Linked Instance answers with URLs of this Instance, such
			// as a resumable upload's, in the scheme the caller used.
			proto := pr.In.Header.Get("X-Forwarded-Proto")
			if proto == "" {
				proto = "http"
				if pr.In.TLS != nil {
					proto = "https"
				}
			}
			pr.Out.Header.Set("X-Forwarded-Proto", proto)
			// The caller's credentials mean nothing to the Linked Instance,
			// which takes the asserted Principal instead, but a request's
			// signature, as on a Cloud Storage XML API request, is checked
			// by the Instance that holds the key.
			if !signature(pr.In.Header.Get("Authorization")) {
				pr.Out.Header.Del("Authorization")
			}
			pr.Out.Header.Set(principalHeader, principal)
			pr.Out.Header.Set(versionsHeader, m.cfg.Versions.String())
			pr.Out.Header.Set(instanceIDHeader, m.cfg.ID)
		},
		ModifyResponse: func(resp *http.Response) error {
			if resp.Header.Get(refusedHeader) == "" {
				if answered != nil {
					answered(resp)
				}
				return nil
			}
			return &RefusedError{Reason: resp.Header.Get(refusedHeader), Msg: fmt.Sprintf("link %q was refused by the other Instance (%s)", name, resp.Status)}
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			if errors.As(err, new(caMismatchError)) {
				if serr := m.found(link, err); serr != nil {
					m.cfg.Log.Warn("could not store the link's state", "link", name, "err", serr)
				}
			}
			fail(w, fmt.Errorf("the Linked Instance %q is unavailable: %w", name, err))
		},
	}
	proxy.ServeHTTP(w, r)
}

// signature reports whether the Authorization header authz carries a
// request signature, by an HMAC or service account key, not a credential.
func signature(authz string) bool {
	return strings.HasPrefix(authz, "GOOG4-") || strings.HasPrefix(authz, "AWS4-")
}

// transport returns the HTTP/2 transport that forwards over the link p,
// trusting only its pinned CA, kept while p's address and CA stay the same.
func (m *Manager) transport(p Link) *http.Transport {
	key := p.Name + "|" + p.Address + "|" + p.Fingerprint
	m.mu.Lock()
	defer m.mu.Unlock()
	if t, ok := m.transports[key]; ok {
		return t
	}
	t := &http.Transport{
		TLSClientConfig:     m.tlsConfig(p.pinned(), nil),
		ForceAttemptHTTP2:   true,
		TLSHandshakeTimeout: 10 * time.Second,
		IdleConnTimeout:     90 * time.Second,
	}
	for k, old := range m.transports {
		if strings.HasPrefix(k, p.Name+"|") {
			old.CloseIdleConnections()
			delete(m.transports, k)
		}
	}
	m.transports[key] = t
	return t
}

// tlsConfig is the client side of a link: this Instance's client
// certificate, and the other Instance's CA checked by verify (any, when
// nil). seen, when set, learns the CA presented and verify's verdict.
func (m *Manager) tlsConfig(verify func(*x509.Certificate) error, seen func(*x509.Certificate, error)) *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS13,
		// The chain is checked against the CA the server presents, and
		// that CA against the pinned one, in VerifyConnection.
		InsecureSkipVerify:   true,
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) { return m.clientCert() },
		VerifyConnection: func(cs tls.ConnectionState) error {
			r, err := presentedCA(cs.PeerCertificates, x509.ExtKeyUsageServerAuth)
			if err == nil && verify != nil {
				err = verify(r)
			}
			if seen != nil {
				seen(r, err)
			}
			return err
		},
	}
}

// usable returns the link name, refusing one this Instance cannot forward
// over: no such link, one with no --link-bind address, or a broken one.
func (m *Manager) usable(name string) (Link, error) {
	for _, q := range m.Links() {
		if q.Name != name {
			continue
		}
		if q.Address == "" {
			return Link{}, fmt.Errorf("link %q has no --link-bind address, so this Instance cannot forward to it", name)
		}
		return q, m.broken(q)
	}
	return Link{}, fmt.Errorf("no link named %q; `gcpemu link list` lists them", name)
}
