package ar

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Upstream registry client for the pull-through cache (FR-AR-006,
// FR-GKE-006): anonymous OCI distribution pulls with the Docker token
// flow (a 401 carrying "WWW-Authenticate: Bearer realm=...,service=...,
// scope=..." is answered by fetching a token from the realm and retrying).

// dockerHub is the canonical name of Docker Hub; its API lives at
// registry-1.docker.io.
const (
	dockerHub    = "docker.io"
	dockerHubAPI = "https://registry-1.docker.io"
)

// upstreamRef names one repository in an upstream registry.
type upstreamRef struct {
	Host string // canonical registry host, e.g. "docker.io", "ghcr.io"
	Base string // API base URL without trailing slash
	Repo string // repository path, e.g. "library/busybox"
}

func (u upstreamRef) String() string { return u.Host + "/" + u.Repo }

// upstreamError is a non-success upstream response.
type upstreamError struct {
	status int
	msg    string
}

func (e *upstreamError) Error() string {
	return fmt.Sprintf("upstream returned %d: %s", e.status, e.msg)
}

// isNotFound reports an upstream 404, as opposed to an unreachable or
// failing upstream (for which stale cache entries are served).
func isNotFound(err error) bool {
	var ue *upstreamError
	return errors.As(err, &ue) && ue.status == http.StatusNotFound
}

// canonicalHost normalises registry host aliases ("index.docker.io",
// "registry-1.docker.io" → "docker.io") and lower-cases the host.
func canonicalHost(h string) string {
	h = strings.ToLower(strings.TrimSuffix(h, "/"))
	switch h {
	case "docker.io", "index.docker.io", "registry-1.docker.io", "registry.hub.docker.com":
		return dockerHub
	}
	return h
}

// canonicalRepo applies Docker Hub's implicit "library/" namespace.
func canonicalRepo(host, repo string) string {
	if host == dockerHub && !strings.Contains(repo, "/") {
		return "library/" + repo
	}
	return repo
}

// isLoopbackHost reports localhost and loopback IP hosts, which are
// contacted over plain HTTP like docker's default insecure registries.
func isLoopbackHost(h string) bool {
	if hh, _, err := net.SplitHostPort(h); err == nil {
		h = hh
	}
	h = strings.Trim(h, "[]")
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// upstreamBase returns the API base URL for a canonical registry host.
func (s *Service) upstreamBase(host string) string {
	s.mu.Lock()
	o := s.upstreamURLs[host]
	s.mu.Unlock()
	switch {
	case o != "":
		return strings.TrimSuffix(o, "/")
	case host == dockerHub:
		return dockerHubAPI
	case isLoopbackHost(host):
		return "http://" + host
	}
	return "https://" + host
}

// SetUpstream overrides the base URL used to reach an upstream registry
// host, e.g. SetUpstream("docker.io", "https://mirror.example.com") to use
// a corporate Docker Hub mirror, or an httptest server in tests. An empty
// baseURL removes the override.
func (s *Service) SetUpstream(host, baseURL string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if baseURL == "" {
		delete(s.upstreamURLs, canonicalHost(host))
		return
	}
	s.upstreamURLs[canonicalHost(host)] = baseURL
}

// upstreamClient performs authenticated upstream requests and caches
// bearer tokens per realm, service and scope.
type upstreamClient struct {
	hc  *http.Client
	now func() time.Time

	mu     sync.Mutex
	chal   map[string]challengeParams // base URL → last Bearer challenge
	tokens map[string]cachedToken     // realm|service|scope → token
}

type challengeParams struct{ realm, service string }

type cachedToken struct {
	token   string
	expires time.Time
}

func newUpstreamClient(now func() time.Time) *upstreamClient {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.ResponseHeaderTimeout = 60 * time.Second
	tr.MaxIdleConnsPerHost = 16
	return &upstreamClient{
		hc:     &http.Client{Transport: tr},
		now:    now,
		chal:   map[string]challengeParams{},
		tokens: map[string]cachedToken{},
	}
}

// do sends method to base+path for repository repo, performing the
// token flow when challenged. The caller closes the response body. A
// non-2xx final response is returned as *upstreamError (body consumed).
func (c *upstreamClient) do(ctx context.Context, method string, u upstreamRef, path string, hdr http.Header) (*http.Response, error) {
	scope := "repository:" + u.Repo + ":pull"
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, method, u.Base+path, nil)
		if err != nil {
			return nil, err
		}
		for k, v := range hdr {
			req.Header[k] = v
		}
		req.Header.Set("User-Agent", "gcpemu-artifact-registry/1.0")
		if tok := c.cachedFor(u.Base, scope); tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		resp, err := c.hc.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode == http.StatusUnauthorized && attempt == 0 {
			ch, ok := parseBearer(resp.Header.Get("WWW-Authenticate"))
			drain(resp)
			if !ok {
				return nil, &upstreamError{status: http.StatusUnauthorized, msg: "upstream requires credentials"}
			}
			if ch.scope != "" {
				scope = ch.scope
			}
			if err := c.fetchToken(ctx, u.Base, challengeParams{ch.realm, ch.service}, scope); err != nil {
				return nil, err
			}
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			drain(resp)
			return nil, &upstreamError{status: resp.StatusCode, msg: strings.TrimSpace(string(b))}
		}
		return resp, nil
	}
}

func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
}

// cachedFor returns a valid cached token for base and scope.
func (c *upstreamClient) cachedFor(base, scope string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch, ok := c.chal[base]
	if !ok {
		return ""
	}
	t, ok := c.tokens[ch.realm+"|"+ch.service+"|"+scope]
	if !ok || !c.now().Before(t.expires) {
		return ""
	}
	return t.token
}

// fetchToken obtains an anonymous token from the challenge realm.
func (c *upstreamClient) fetchToken(ctx context.Context, base string, ch challengeParams, scope string) error {
	ru, err := url.Parse(ch.realm)
	if err != nil || (ru.Scheme != "https" && ru.Scheme != "http") {
		return &upstreamError{status: http.StatusUnauthorized, msg: fmt.Sprintf("invalid token realm %q", ch.realm)}
	}
	q := ru.Query()
	if ch.service != "" {
		q.Set("service", ch.service)
	}
	q.Set("scope", scope)
	ru.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ru.String(), nil)
	if err != nil {
		return err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer drain(resp)
	if resp.StatusCode != http.StatusOK {
		return &upstreamError{status: http.StatusUnauthorized, msg: fmt.Sprintf("token endpoint %s returned %d", ch.realm, resp.StatusCode)}
	}
	var tr struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&tr); err != nil {
		return &upstreamError{status: http.StatusUnauthorized, msg: "invalid token response: " + err.Error()}
	}
	tok := tr.Token
	if tok == "" {
		tok = tr.AccessToken
	}
	if tok == "" {
		return &upstreamError{status: http.StatusUnauthorized, msg: "token endpoint returned no token"}
	}
	ttl := time.Duration(tr.ExpiresIn) * time.Second
	if ttl <= 0 {
		ttl = 60 * time.Second // the distribution spec default
	}
	ttl = min(ttl, 10*time.Minute) * 9 / 10
	c.mu.Lock()
	c.chal[base] = ch
	c.tokens[ch.realm+"|"+ch.service+"|"+scope] = cachedToken{token: tok, expires: c.now().Add(ttl)}
	c.mu.Unlock()
	return nil
}

type bearerChallenge struct{ realm, service, scope string }

// parseBearer parses `Bearer realm="...",service="...",scope="..."`.
func parseBearer(h string) (bearerChallenge, bool) {
	scheme, rest, _ := strings.Cut(strings.TrimSpace(h), " ")
	if !strings.EqualFold(scheme, "bearer") {
		return bearerChallenge{}, false
	}
	var ch bearerChallenge
	for rest != "" {
		rest = strings.TrimLeft(rest, " ,")
		k, v, ok := strings.Cut(rest, "=")
		if !ok {
			break
		}
		k = strings.ToLower(strings.TrimSpace(k))
		if strings.HasPrefix(v, `"`) {
			end := strings.IndexByte(v[1:], '"')
			if end < 0 {
				return bearerChallenge{}, false
			}
			rest, v = v[end+2:], v[1:end+1]
		} else {
			v, rest, _ = strings.Cut(v, ",")
		}
		switch k {
		case "realm":
			ch.realm = v
		case "service":
			ch.service = v
		case "scope":
			ch.scope = v
		}
	}
	return ch, ch.realm != ""
}
